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
	if cfg.CircuitBreaker.Enabled && cfg.CircuitBreaker.MaxWindowSamples <= 0 {
		if _, ok := cfg.CircuitBreakerStore.(*RedisCircuitBreakerStore); ok {
			// Redis store 每次记账都全量序列化事件列表，窗口条数默认取更小值
			cfg.CircuitBreaker.MaxWindowSamples = defaultRedisCircuitBreakerMaxWindowSamples
		}
	}
	cfg.CircuitBreaker = normalizeCircuitBreakerConfig(cfg.CircuitBreaker)
	breaker := newChannelCircuitBreaker(cfg.BreakerScope, cfg.CircuitBreaker, cfg.CircuitBreakerStore)
	if breaker != nil {
		// 先完成全部字段初始化，再发布到全局 registry，避免构造期 data race
		breaker.logger = cfg.Logger
		globalChannelBreakerRegistry.register(breaker)
	}
	return &Proxy{
		cfg:     cfg,
		breaker: breaker,
	}
}

// Close 将当前 Proxy 的熔断器从全局注册表中注销。
// 动态创建大量 Proxy（如按租户构建）的场景应在 Proxy 不再使用时调用，
// 避免注册表无限增长。Close 不影响熔断状态存储中的数据。
func (p *Proxy) Close() {
	if p == nil || p.breaker == nil {
		return
	}
	globalChannelBreakerRegistry.unregister(p.breaker)
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

func (p *Proxy) observer() Observer {
	if p != nil && p.cfg.Observer != nil {
		return p.cfg.Observer
	}
	return noopObserver{}
}

// ServeHTTP 实现 http.Handler 接口
// 请求流程: 获取可用渠道 -> 按顺序尝试每个渠道 -> 成功则返回响应，全部失败则返回错误
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	retryCfg := p.cfg.Retry

	ctx := &Context{
		Request: r,
	}
	p.initFailoverDeadline(ctx)
	r, cancelFailover := failoverRequestWithDeadline(r, ctx)
	defer cancelFailover()
	obs := p.observer()
	var doneErr error
	obs.OnRequestStart(ctx)
	defer func() {
		obs.OnRequestDone(ctx, doneErr)
	}()

	if err := p.prepareRequestBody(w, ctx); err != nil {
		doneErr = err
		return
	}

	channels, err := p.selectChannels(w, r, ctx)
	if err != nil {
		doneErr = err
		return
	}

	result := p.tryChannels(r, ctx, channels, retryCfg)
	p.writePipelineResponse(w, r, ctx, result)
	doneErr = result.lastErr
	if doneErr == nil {
		// 上游断流时熔断器已按失败记账，OnRequestDone 也应看到同一结论，
		// 避免观测侧"请求成功"与熔断决策互相矛盾。
		doneErr = streamInterruptionError(ctx)
	}
}
