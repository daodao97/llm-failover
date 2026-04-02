package failover

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
)

// 渠道类型常量
const (
	CTypePool  = "pool"  // 账号池渠道
	CTypeThird = "third" // 三方渠道
)

type Config struct {
	Channels       []Channel
	GetChannels    func(r *http.Request) ([]Channel, error) // 动态获取 channels，优先于 Channels
	Retry          RetryConfig
	Client         *http.Client
	Logger         Logger                            // 可选日志接口，默认 no-op
	ErrorBody      func(errType, message string) any // 自定义错误响应体结构，返回的对象会被 JSON 编码
	CircuitBreaker CircuitBreakerConfig
	BreakerScope   string
	// ShouldCountFailureForCircuit 允许业务层自定义哪些失败应计入熔断。
	// 返回 true 表示将本次失败记入熔断窗口；返回 false 表示忽略。
	// 为 nil 时使用框架默认规则。
	ShouldCountFailureForCircuit func(ctx *Context, ch *Channel, err error) bool

	// 钩子
	// BeforeProxy 在代理请求前调用，可以修改 ctx.RequestBody、ctx.TargetURL、ctx.TargetHeader
	// 如果返回非 nil 的处理函数，则使用该函数处理请求，不转发到远程渠道
	BeforeProxy    func(ctx *Context) func(ctx *Context) (*http.Response, error)
	BeforeAttempt  func(ctx *Context)
	OnAttemptError func(ctx *Context, err error)
	AfterProxy     func(ctx *Context, resp *http.Response)
	OnChannelFail  func(ctx *Context, err error)
	OnBody         func(ctx *Context, body []byte)
	OnSSE          func(ctx *Context, event *SSEEvent)
	TransformSSE   func(ctx *Context, event SSEEvent) []SSEEvent
	OnError        func(ctx *Context, err *ErrorResponse)
}

type ErrorResponse struct {
	Type       string
	Message    string
	StatusCode int
	Header     http.Header
	Body       []byte
	Err        error
}

type Context struct {
	Request             *http.Request
	Channel             *Channel
	Attempt             int
	ChannelIdx          int
	KeyIdx              int
	CurrentKey          *Key
	RequestBody         []byte
	OriginalRequestBody []byte
	Stats               *Stats
	Model               string
	IsStream            bool

	TargetURL    string
	TargetHeader http.Header
	RetryReason  string

	LastStatusCode     int
	LastResponseHeader http.Header
	LastResponseBody   []byte
	PoolStats          PoolStats
}

func (c *Context) resetForChannel() {
	if c == nil {
		return
	}
	c.Attempt = 0
	c.KeyIdx = 0
	c.CurrentKey = nil
	c.Stats = nil
	c.Model = ""
	c.IsStream = false
	c.TargetURL = ""
	c.TargetHeader = nil
	c.RetryReason = ""
	c.LastStatusCode = 0
	c.LastResponseHeader = nil
	c.LastResponseBody = nil
	c.PoolStats = PoolStats{}
	c.RequestBody = cloneBytes(c.OriginalRequestBody)
}

func (c *Context) resetForAttempt(targetHeader http.Header) {
	if c == nil {
		return
	}
	c.Stats = nil
	c.TargetHeader = targetHeader.Clone()
	c.LastStatusCode = 0
	c.LastResponseHeader = nil
	c.LastResponseBody = nil
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	return append([]byte(nil), src...)
}

// PoolStats 描述一次候选凭证池选择过程中的诊断信息。
//
// 它的设计目标不是表达具体业务模型，而是回答这几个通用问题：
// 1. 本次候选池里原本有多少凭证可供选择
// 2. 有多少凭证因为 scope 不匹配被过滤
// 3. 有多少凭证当前忙碌，暂时不能占用
// 4. 有多少凭证已经无效，不应继续尝试
// 5. 最终挑出了多少凭证，实际返回了多少凭证
//
// 这些字段只用于错误解释、日志和可观测，不参与核心路由决策。
// 如果你的上层系统没有“候选凭证池”概念，可以完全忽略它。
type PoolStats struct {
	RequestedScope  string
	TotalCandidates int
	ScopeFiltered   int
	Busy            int
	Invalid         int
	Selected        int
	Returned        int
}

type SSEEvent struct {
	Event string
	Data  string
	ID    string
}

type Stats struct {
	DNSDuration     time.Duration
	ConnectDuration time.Duration
	TLSDuration     time.Duration
	TLSVersion      uint16
	TLSCipherSuite  uint16
	RequestStart    time.Time
	TTFB            time.Duration
	FirstEventTime  time.Duration
	StreamDuration  time.Duration
	TotalDuration   time.Duration
}

func (s *Stats) TLSVersionString() string {
	switch s.TLSVersion {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return ""
	}
}

type Key struct {
	ID    string
	Value string
}

type ModelRewriteRule struct {
	Source string
	Target string
}

type Channel struct {
	Id      int
	Name    string
	BaseURL string
	Enabled bool
	// ChannelGroupID 标记该运行时渠道是从哪个 token 渠道分组展开出来的；0 表示非分组来源。
	ChannelGroupID int
	// ChannelGroupMultiplier 标记该运行时渠道所属分组的计费倍率；默认 1。
	ChannelGroupMultiplier decimal.Decimal
	Headers                http.Header
	// ModelRewrite 按顺序匹配请求中的 model 并重写
	ModelRewrite []ModelRewriteRule
	Handler      func(ctx *Context) (*http.Response, error)
	// ResolvePath 可按渠道自定义目标 path（可包含 query）
	ResolvePath func(path string, r *http.Request) string

	GetKeys        func(ctx *Context) []Key
	CType          string // "pool" 账号池渠道，"third" 三方渠道
	KeyType        string // "oauth" 使用 Bearer，否则使用 x-api-key
	Retry          *RetryConfig
	GetHttpClient  func(ctx *Context) *http.Client // 获取自定义 HTTP 客户端
	AcquireKey     func(keyID string) bool
	ReleaseKey     func(keyID string)
	CheckAvailable func() (bool, string)
}

type RetryConfig struct {
	MaxAttempts     int
	BaseDelay       time.Duration
	MaxDelay        time.Duration
	RetryOnError    func(ctx *Context, ch *Channel, err error) bool
	RetryOnResponse func(ctx *Context, ch *Channel, code int, body []byte) bool
	RetryOnSSE      func(isErr bool) bool
}

type CircuitBreakerConfig struct {
	Enabled                bool
	MinSamples             int
	ErrorRateThreshold     float64
	FailureWindow          time.Duration
	Cooldown               time.Duration
	SlowThreshold          time.Duration
	StreamSlowThreshold    time.Duration
	NonStreamSlowThreshold time.Duration
	SlowRateThreshold      float64
}

func DefaultRetry() RetryConfig {
	return RetryConfig{
		MaxAttempts: 1,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    1 * time.Second,
		RetryOnError: func(ctx *Context, ch *Channel, err error) bool {
			return IsRetryableError(err)
		},
		RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
			return code >= http.StatusTooManyRequests
		},
		RetryOnSSE: func(isErr bool) bool {
			return isErr
		},
	}
}

func NoRetry() RetryConfig {
	cfg := DefaultRetry()
	cfg.MaxAttempts = 1
	return cfg
}

var _ = NoRetry()

func NewChannel(id int, name, baseURL string) Channel {
	return Channel{
		Id:                     id,
		Name:                   name,
		BaseURL:                baseURL,
		Enabled:                true,
		ChannelGroupMultiplier: decimal.NewFromInt(1),
		Headers:                make(http.Header),
	}
}
