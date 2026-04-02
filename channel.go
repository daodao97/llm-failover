package failover

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
)

// DefaultPoolMaxAttempts Pool 渠道的默认最大重试次数
const DefaultPoolMaxAttempts = 5

// PoolInfo pool 类型渠道的配置
type PoolInfo struct {
	BaseURL string `json:"base_url"` // 可选，覆盖默认的 base_url
	Group   string `json:"group"`
}

// ThirdAPIKey 三方渠道的 API Key
type ThirdAPIKey struct {
	Value string `json:"value"`
}

// ThirdHeader 三方渠道的自定义 Header
type ThirdHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ThirdModelRewrite 三方渠道的模型重写规则
type ThirdModelRewrite struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ThirdInfo third 类型渠道的配置
type ThirdInfo struct {
	BaseURL      string              `json:"base_url"`
	APIKeys      []ThirdAPIKey       `json:"api_key"`
	Headers      []ThirdHeader       `json:"headers"`
	ModelRewrite []ThirdModelRewrite `json:"model_rewrite"`
}

// PoolChannelConfig pool 渠道构建配置
type PoolChannelConfig struct {
	Id              int
	Name            string
	DefaultBaseURL  string // HTTP 直连渠道的默认上游地址；自定义 Handler 可留空
	Info            *PoolInfo
	Handler         func(ctx *Context) (*http.Response, error)
	GetKeys         func(ctx *Context) []Key
	GetHttpClient   func(ctx *Context) *http.Client // 获取自定义 HTTP 客户端
	AcquireKey      func(keyID string) bool
	ReleaseKey      func(keyID string)
	CheckAvailable  func() (bool, string)
	RetryOnError    func(ctx *Context, ch *Channel, err error) bool
	RetryOnResponse func(ctx *Context, ch *Channel, code int, body []byte) bool
}

// BuildPoolChannel 构建 pool 类型渠道
func BuildPoolChannel(cfg PoolChannelConfig) (Channel, error) {
	if cfg.GetKeys == nil {
		return Channel{}, errors.New("GetKeys is required")
	}

	baseURL := cfg.DefaultBaseURL
	if cfg.Info != nil && cfg.Info.BaseURL != "" {
		baseURL = cfg.Info.BaseURL
	}

	retryCfg := DefaultRetry()
	retryCfg.MaxAttempts = DefaultPoolMaxAttempts
	if cfg.RetryOnError != nil {
		retryCfg.RetryOnError = cfg.RetryOnError
	}
	if cfg.RetryOnResponse != nil {
		retryCfg.RetryOnResponse = cfg.RetryOnResponse
	}

	ch := NewChannel(cfg.Id, cfg.Name, baseURL)
	ch.Handler = cfg.Handler
	ch.GetKeys = cfg.GetKeys
	ch.GetHttpClient = cfg.GetHttpClient
	ch.AcquireKey = cfg.AcquireKey
	ch.ReleaseKey = cfg.ReleaseKey
	ch.CType = CTypePool
	ch.KeyType = "oauth"
	ch.CheckAvailable = cfg.CheckAvailable
	ch.Retry = &retryCfg

	return ch, nil
}

// BuildThirdPartyChannel 构建三方渠道
func BuildThirdPartyChannel(id int, name string, info *ThirdInfo) (Channel, error) {
	if info == nil {
		return Channel{}, errors.New("channel info is nil")
	}

	if info.BaseURL == "" {
		return Channel{}, errors.New("channel base_url is empty")
	}

	if len(info.APIKeys) == 0 {
		return Channel{}, errors.New("channel api_key is empty")
	}

	// 提取有效的 API keys
	var apiKeys []string
	for _, k := range info.APIKeys {
		if k.Value != "" {
			apiKeys = append(apiKeys, k.Value)
		}
	}

	if len(apiKeys) == 0 {
		return Channel{}, errors.New("channel api_key values are empty")
	}

	// 创建渠道
	ch := NewChannel(id, name, info.BaseURL)
	ch.CType = CTypeThird
	ch.KeyType = "api_key"
	retry := DefaultRetry()
	retry.RetryOnResponse = func(ctx *Context, ch *Channel, code int, body []byte) bool {
		return code > http.StatusBadRequest
	}
	ch.Retry = &retry

	// 设置 GetKeys（轮询多个 API key）
	apiKeysCopy := make([]string, len(apiKeys))
	copy(apiKeysCopy, apiKeys)
	currentIndex := 0
	mu := &sync.Mutex{}

	ch.GetKeys = func(ctx *Context) []Key {
		mu.Lock()
		defer mu.Unlock()

		keys := make([]Key, 0, len(apiKeysCopy))
		for i := range apiKeysCopy {
			idx := (currentIndex + i) % len(apiKeysCopy)
			keys = append(keys, Key{
				ID:    fmt.Sprintf("%s-key-%d", name, idx),
				Value: apiKeysCopy[idx],
			})
		}
		currentIndex = (currentIndex + 1) % len(apiKeysCopy)
		return keys
	}

	// 设置自定义 headers
	for _, h := range info.Headers {
		if h.Key != "" && h.Value != "" {
			ch.Headers.Set(h.Key, h.Value)
		}
	}
	ch.ModelRewrite = buildModelRewriteRules(info.ModelRewrite)

	return ch, nil
}

func buildModelRewriteRules(rules []ThirdModelRewrite) []ModelRewriteRule {
	if len(rules) == 0 {
		return nil
	}

	result := make([]ModelRewriteRule, 0, len(rules))
	for _, rule := range rules {
		source := strings.TrimSpace(rule.Key)
		target := strings.TrimSpace(rule.Value)
		if source == "" || target == "" {
			continue
		}
		result = append(result, ModelRewriteRule{
			Source: source,
			Target: target,
		})
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func rewriteModelByRules(model string, rules []ModelRewriteRule) (string, bool) {
	if model == "" || len(rules) == 0 {
		return model, false
	}

	for _, rule := range rules {
		if model == rule.Source {
			return rule.Target, true
		}

		matched, err := path.Match(rule.Source, model)
		if err != nil || !matched {
			continue
		}
		return rule.Target, true
	}

	return model, false
}
