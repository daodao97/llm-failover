package failover

import (
	"errors"
	"net/http"
)

// Proxy 反向代理处理器，实现 http.Handler 接口
type Proxy struct {
	cfg     Config
	breaker *channelCircuitBreaker
}

type pipelineResult struct {
	successResp *http.Response
	lastResp    *http.Response
	lastErr     error
}

type retryDecision struct {
	reason      string
	statusRetry bool
	isSSE       bool
	ssePeek     []byte
}

var (
	errEmptyResponseInternalHandler = errors.New("E_PROXY_EMPTY_RESPONSE_BEFORE_PROXY_INTERNAL_HANDLER")
	errEmptyResponseChannelHandler  = errors.New("E_PROXY_EMPTY_RESPONSE_CHANNEL_HANDLER")
	errEmptyResponseHTTPClient      = errors.New("E_PROXY_EMPTY_RESPONSE_HTTP_CLIENT")
	errEmptyResponseTryChannels     = errors.New("E_PROXY_EMPTY_RESPONSE_TRY_CHANNELS_GUARD")
	errEmptyResponseTryChannelLoop  = errors.New("E_PROXY_EMPTY_RESPONSE_TRY_CHANNEL_LOOP_GUARD")
)

// New 创建代理实例
func New(cfg Config) *Proxy {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 0}
	}
	cfg.Logger = normalizeLogger(cfg.Logger)
	if cfg.Retry.MaxAttempts <= 0 {
		cfg.Retry.MaxAttempts = 1
	}
	cfg.CircuitBreaker = normalizeCircuitBreakerConfig(cfg.CircuitBreaker)
	return &Proxy{
		cfg:     cfg,
		breaker: newChannelCircuitBreaker(cfg.BreakerScope, cfg.CircuitBreaker),
	}
}

// ResetChannelHealthStats 清空当前 Proxy 内熔断器维护的全部渠道统计和状态。
func (p *Proxy) ResetChannelHealthStats() int {
	if p == nil || p.breaker == nil {
		return 0
	}
	return p.breaker.Reset()
}

// ResetChannelHealthStatsForChannel 清空当前 Proxy 内指定渠道的统计和状态。
func (p *Proxy) ResetChannelHealthStatsForChannel(ch *Channel) bool {
	if p == nil || p.breaker == nil || ch == nil {
		return false
	}
	return p.breaker.ResetChannel(ch)
}

// ResetChannelHealthStatsByKey 按渠道 key 清空当前 Proxy 内指定渠道的统计和状态。
func (p *Proxy) ResetChannelHealthStatsByKey(channelKey string) bool {
	if p == nil || p.breaker == nil {
		return false
	}
	return p.breaker.ResetChannelByKey(channelKey)
}

// ServeHTTP 实现 http.Handler 接口
// 请求流程: 获取可用渠道 -> 按顺序尝试每个渠道 -> 成功则返回响应，全部失败则返回错误
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	retryCfg := p.cfg.Retry

	ctx := &Context{
		Request: r,
	}

	if !p.prepareRequestBody(w, ctx) {
		return
	}

	channels, ok := p.selectChannels(w, r, ctx)
	if !ok {
		return
	}

	result := p.tryChannels(r, ctx, channels, retryCfg)
	p.writePipelineResponse(w, r, ctx, result)
}
