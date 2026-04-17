package failover

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusLabeler 用来把 failover 运行时里的领域信息映射成 Prometheus label。
//
// 这里刻意不把 protocol/channel/error_type 等标签逻辑写死在库里，
// 因为不同接入方的口径并不相同：
// 1. 有的系统按协议区分 `claude/gemini/codex`
// 2. 有的系统按业务线区分
// 3. 有的系统希望对 reason / error_type 再做一次归一化
//
// 因此官方 observer 只负责“在正确的事件点上打指标”，
// 而 label 的具体取值交给外部决定。
type PrometheusLabeler interface {
	Protocol(ctx *Context) string
	Channel(ctx *Context, ch *Channel) string
	ChannelType(ctx *Context, ch *Channel) string
	RetryReason(reason string) string
	ErrorType(errType string) string
}

// PrometheusRateLimitClassifier 决定一次 attempt 完成后，是否应额外记为“限流命中”。
//
// 之所以单独抽这个函数，而不是直接把 429 写死，
// 是因为不同系统对 rate limit 的定义并不完全一样：
// 1. 只把 HTTP 429 视为限流
// 2. 某些上游会在 403/400 body 里表达 quota exhausted
// 3. 某些业务只想统计 pool 渠道上的限流，而忽略 third 渠道
type PrometheusRateLimitClassifier func(ctx *Context, ch *Channel, resp *http.Response, err error) (hit bool, scope string)

// PrometheusObserverOptions 控制官方 Prometheus observer 的注册位置和标签策略。
//
// 这里不暴露每个指标名的自定义能力，是为了保持官方 observer 的可维护性；
// 真正需要变化的，通常是 registerer、label 提取和桶配置。
type PrometheusObserverOptions struct {
	// Registerer 允许接入方注入自定义 registry。
	// 为空时使用 prometheus.DefaultRegisterer。
	Registerer prometheus.Registerer
	// Namespace / Subsystem 用于与现有指标命名体系对齐。
	Namespace string
	Subsystem string

	Labeler             PrometheusLabeler
	RateLimitClassifier PrometheusRateLimitClassifier

	// 这些 buckets 只覆盖网络阶段耗时；
	// 如果为空，则使用一组面向代理场景的默认桶。
	DNSBuckets     []float64
	ConnectBuckets []float64
	TLSBuckets     []float64
	TTFBBuckets    []float64
}

// prometheusObserver 是 Observer 的 Prometheus 实现。
//
// 它的职责很窄：
// 1. 在 pipeline 的关键事件点累加/观测指标
// 2. 复用外部给出的 label 策略
// 3. 避免把 Prometheus collector 的重复注册问题泄漏给上层
type prometheusObserver struct {
	labeler             PrometheusLabeler
	rateLimitClassifier PrometheusRateLimitClassifier

	channelAttemptTotal *prometheus.CounterVec
	retryTotal          *prometheus.CounterVec
	finalFailureTotal   *prometheus.CounterVec
	upstreamStatusTotal *prometheus.CounterVec
	rateLimitHitTotal   *prometheus.CounterVec

	upstreamDNSDurationSeconds     *prometheus.HistogramVec
	upstreamConnectDurationSeconds *prometheus.HistogramVec
	upstreamTLSDurationSeconds     *prometheus.HistogramVec
	upstreamTTFBDurationSeconds    *prometheus.HistogramVec
}

type defaultPrometheusLabeler struct{}

// 默认实现只提供最保守的 fallback，避免调用方未注入 labeler 时直接 panic。
func (defaultPrometheusLabeler) Protocol(*Context) string {
	return "unknown"
}

func (defaultPrometheusLabeler) Channel(_ *Context, ch *Channel) string {
	if ch == nil {
		return "unknown"
	}
	return normalizePrometheusLabel(ch.Name, "unknown")
}

func (defaultPrometheusLabeler) ChannelType(_ *Context, ch *Channel) string {
	if ch == nil {
		return "unknown"
	}
	return normalizePrometheusLabel(string(ch.CType), "unknown")
}

func (defaultPrometheusLabeler) RetryReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "unknown"
	}
	return normalizePrometheusLabel(reason, "unknown")
}

func (defaultPrometheusLabeler) ErrorType(errType string) string {
	errType = strings.TrimSpace(errType)
	if errType == "" {
		return "unknown"
	}
	return normalizePrometheusLabel(errType, "unknown")
}

func defaultPrometheusRateLimitClassifier(_ *Context, _ *Channel, resp *http.Response, _ error) (bool, string) {
	// 默认只把显式的 HTTP 429 记作上游限流命中。
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		return true, "upstream"
	}
	return false, ""
}

// NewPrometheusObserver 创建官方 Prometheus observer。
//
// 设计上有两个关键点：
//  1. collector 在这里统一构造，避免每个接入方重复实现同一套指标
//  2. register 阶段允许“重复创建、复用已注册 collector”，这样业务侧可以放心地在
//     handler 构造路径、测试环境甚至多次初始化场景中调用，而不会因重复注册直接 panic
func NewPrometheusObserver(opts PrometheusObserverOptions) Observer {
	registerer := opts.Registerer
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	labeler := opts.Labeler
	if labeler == nil {
		labeler = defaultPrometheusLabeler{}
	}

	rateLimitClassifier := opts.RateLimitClassifier
	if rateLimitClassifier == nil {
		rateLimitClassifier = defaultPrometheusRateLimitClassifier
	}

	observer := &prometheusObserver{
		labeler:             labeler,
		rateLimitClassifier: rateLimitClassifier,
		channelAttemptTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "channel_attempt_total",
				Help:      "Total channel attempts by protocol/channel/result.",
			},
			[]string{"protocol", "channel", "channel_type", "result"},
		),
		retryTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "retry_total",
				Help:      "Total proxy retry attempts by protocol and reason.",
			},
			[]string{"protocol", "reason"},
		),
		finalFailureTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "final_failure_total",
				Help:      "Total final proxy failures by protocol/error type/status.",
			},
			[]string{"protocol", "error_type", "status"},
		),
		upstreamStatusTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "upstream_status_total",
				Help:      "Total upstream responses by protocol/channel/status.",
			},
			[]string{"protocol", "channel", "status"},
		),
		rateLimitHitTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "rate_limit_hit_total",
				Help:      "Total upstream rate-limit hits.",
			},
			[]string{"protocol", "scope", "channel", "channel_type"},
		),
		upstreamDNSDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "upstream_dns_duration_seconds",
				Help:      "Upstream DNS lookup latency in seconds.",
				Buckets:   nonEmptyBuckets(opts.DNSBuckets, []float64{0.001, 0.003, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}),
			},
			[]string{"protocol", "channel"},
		),
		upstreamConnectDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "upstream_connect_duration_seconds",
				Help:      "Upstream TCP connect latency in seconds.",
				Buckets:   nonEmptyBuckets(opts.ConnectBuckets, []float64{0.001, 0.003, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2}),
			},
			[]string{"protocol", "channel"},
		),
		upstreamTLSDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "upstream_tls_duration_seconds",
				Help:      "Upstream TLS handshake latency in seconds.",
				Buckets:   nonEmptyBuckets(opts.TLSBuckets, []float64{0.001, 0.003, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2}),
			},
			[]string{"protocol", "channel"},
		),
		upstreamTTFBDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: opts.Namespace,
				Subsystem: opts.Subsystem,
				Name:      "upstream_ttfb_duration_seconds",
				Help:      "Upstream time-to-first-byte latency in seconds.",
				Buckets:   nonEmptyBuckets(opts.TTFBBuckets, []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}),
			},
			[]string{"protocol", "channel"},
		),
	}
	return observer.registerOrReuse(registerer)
}

// registerOrReuse 负责把本 observer 持有的 collector 注册到目标 registerer。
//
// 如果同名 collector 已注册，则直接复用已有实例。
// 这样做的原因是：
// 1. Prometheus 的默认 registerer 是进程级全局单例
// 2. 很多项目会在测试里反复构造 handler / proxy
// 3. 如果每次都 MustRegister，同名指标会立刻 panic
func (o *prometheusObserver) registerOrReuse(registerer prometheus.Registerer) *prometheusObserver {
	o.channelAttemptTotal = registerCounterVec(registerer, o.channelAttemptTotal)
	o.retryTotal = registerCounterVec(registerer, o.retryTotal)
	o.finalFailureTotal = registerCounterVec(registerer, o.finalFailureTotal)
	o.upstreamStatusTotal = registerCounterVec(registerer, o.upstreamStatusTotal)
	o.rateLimitHitTotal = registerCounterVec(registerer, o.rateLimitHitTotal)
	o.upstreamDNSDurationSeconds = registerHistogramVec(registerer, o.upstreamDNSDurationSeconds)
	o.upstreamConnectDurationSeconds = registerHistogramVec(registerer, o.upstreamConnectDurationSeconds)
	o.upstreamTLSDurationSeconds = registerHistogramVec(registerer, o.upstreamTLSDurationSeconds)
	o.upstreamTTFBDurationSeconds = registerHistogramVec(registerer, o.upstreamTTFBDurationSeconds)
	return o
}

// registerCounterVec / registerHistogramVec 封装“注册或复用”的通用逻辑。
//
// 这里不静默吞掉所有错误，只对白名单情况 AlreadyRegistered 做复用；
// 其他错误依旧直接 panic，因为那通常意味着真正的配置错误，
// 比如同名 collector 的类型或标签集合不一致。
func registerCounterVec(registerer prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := registerer.Register(collector); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
		panic(err)
	}
	return collector
}

func registerHistogramVec(registerer prometheus.Registerer, collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := registerer.Register(collector); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.HistogramVec); ok {
				return existing
			}
		}
		panic(err)
	}
	return collector
}

func (o *prometheusObserver) OnRequestStart(*Context) {}

func (o *prometheusObserver) OnRequestDone(*Context, error) {}

func (o *prometheusObserver) OnChannelSelected(*Context, *Channel) {}

func (o *prometheusObserver) OnChannelSkipped(*Context, *Channel, string) {}

func (o *prometheusObserver) OnAttemptStart(*Context, *Channel, *Key) {}

// OnAttemptDone 是这一版 observer 的核心事件点。
//
// 这里统一完成 4 类观测：
// 1. attempt 成败计数
// 2. 上游 HTTP 状态码计数
// 3. 限流命中计数
// 4. 网络阶段耗时直方图
//
// 这样上层业务就不需要再在 AfterProxy / OnAttemptError 等多个 hook 里手工拼指标。
func (o *prometheusObserver) OnAttemptDone(ctx *Context, ch *Channel, _ *Key, resp *http.Response, err error, result AttemptResult) {
	protocol, channel, channelType := o.labelValues(ctx, ch)
	o.channelAttemptTotal.WithLabelValues(protocol, channel, channelType, normalizePrometheusLabel(string(result), "unknown")).Inc()

	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	} else if ctx != nil && ctx.LastStatusCode > 0 {
		// 某些失败路径没有可用的 resp，但 ctx 上仍保留了最后一次状态码，
		// 这里优先复用它，尽量避免丢指标。
		statusCode = ctx.LastStatusCode
	}
	if statusCode > 0 {
		o.upstreamStatusTotal.WithLabelValues(protocol, channel, strconv.Itoa(statusCode)).Inc()
	}

	if hit, scope := o.rateLimitClassifier(ctx, ch, resp, err); hit {
		o.rateLimitHitTotal.WithLabelValues(
			protocol,
			normalizePrometheusLabel(scope, "unknown"),
			channel,
			channelType,
		).Inc()
	}

	if ctx == nil || ctx.Stats == nil {
		return
	}

	observeDuration(o.upstreamDNSDurationSeconds, protocol, channel, ctx.Stats.DNSDuration)
	observeDuration(o.upstreamConnectDurationSeconds, protocol, channel, ctx.Stats.ConnectDuration)
	observeDuration(o.upstreamTLSDurationSeconds, protocol, channel, ctx.Stats.TLSDuration)
	observeDuration(o.upstreamTTFBDurationSeconds, protocol, channel, ctx.Stats.TTFB)
}

func (o *prometheusObserver) OnRetry(ctx *Context, _ *Channel, _ *Key, reason string) {
	// Retry 事件是框架确认“要重试”时触发的，比在 BeforeAttempt 里用 attempt>1 推断更准确。
	protocol := normalizePrometheusLabel(o.labeler.Protocol(ctx), "unknown")
	o.retryTotal.WithLabelValues(protocol, o.labeler.RetryReason(reason)).Inc()
}

func (o *prometheusObserver) OnCircuitStateChange(*Context, *Channel, string, string, string) {}

func (o *prometheusObserver) OnSSEEvent(*Context, *Channel, *SSEEvent) {}

// OnFinalError 用于统计“整个 pipeline 最终失败”，而不是单次 attempt 失败。
//
// 这和 channel_attempt_total 的语义不同：
// 1. channel_attempt_total 关注每次尝试
// 2. final_failure_total 关注对调用方可见的最终失败结果
func (o *prometheusObserver) OnFinalError(ctx *Context, errResp *ErrorResponse) {
	protocol := normalizePrometheusLabel(o.labeler.Protocol(ctx), "unknown")
	errType := ""
	statusCode := 0
	if errResp != nil {
		errType = errResp.Type
		statusCode = errResp.StatusCode
	}
	if statusCode <= 0 && ctx != nil {
		statusCode = ctx.LastStatusCode
	}
	if statusCode <= 0 {
		statusCode = http.StatusBadGateway
	}
	o.finalFailureTotal.WithLabelValues(protocol, o.labeler.ErrorType(errType), strconv.Itoa(statusCode)).Inc()
}

// labelValues 统一做一层 fallback + normalize，确保最终打到 Prometheus 的 label 可控。
func (o *prometheusObserver) labelValues(ctx *Context, ch *Channel) (protocol string, channel string, channelType string) {
	protocol = normalizePrometheusLabel(o.labeler.Protocol(ctx), "unknown")
	channel = normalizePrometheusLabel(o.labeler.Channel(ctx, ch), "unknown")
	channelType = normalizePrometheusLabel(o.labeler.ChannelType(ctx, ch), "unknown")
	return protocol, channel, channelType
}

func observeDuration(vec *prometheus.HistogramVec, protocol string, channel string, d time.Duration) {
	// 只有正数耗时才有观测意义；零值通常表示该阶段未发生或未成功采集。
	if d <= 0 {
		return
	}
	vec.WithLabelValues(protocol, channel).Observe(d.Seconds())
}

func nonEmptyBuckets(buckets []float64, fallback []float64) []float64 {
	if len(buckets) > 0 {
		return buckets
	}
	return fallback
}

// normalizePrometheusLabel 把任意输入收敛为适合作为 label value 的稳定字符串。
//
// 这里做的事情包括：
// 1. trim + lower
// 2. 非字母数字统一折叠成单个下划线
// 3. 去掉前后下划线
// 4. 限制最大长度，避免把高基数原始字符串直接打进指标
func normalizePrometheusLabel(v string, fallback string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return fallback
	}

	const maxLen = 64
	var b strings.Builder
	lastUnderscore := false
	for _, r := range v {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if lastUnderscore {
			continue
		}
		b.WriteByte('_')
		lastUnderscore = true
		if b.Len() >= maxLen {
			break
		}
	}

	s := strings.Trim(b.String(), "_")
	if s == "" {
		return fallback
	}
	if len(s) > maxLen {
		return strings.Trim(s[:maxLen], "_")
	}
	return s
}
