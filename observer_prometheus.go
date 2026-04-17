package failover

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusLabeler controls how request/channel/error metadata is mapped to metric labels.
type PrometheusLabeler interface {
	Protocol(ctx *Context) string
	Channel(ctx *Context, ch *Channel) string
	ChannelType(ctx *Context, ch *Channel) string
	RetryReason(reason string) string
	ErrorType(errType string) string
}

// PrometheusRateLimitClassifier decides whether a completed attempt should count as a rate-limit hit.
type PrometheusRateLimitClassifier func(ctx *Context, ch *Channel, resp *http.Response, err error) (hit bool, scope string)

type PrometheusObserverOptions struct {
	Registerer prometheus.Registerer
	Namespace  string
	Subsystem  string

	Labeler             PrometheusLabeler
	RateLimitClassifier PrometheusRateLimitClassifier

	DNSBuckets     []float64
	ConnectBuckets []float64
	TLSBuckets     []float64
	TTFBBuckets    []float64
}

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
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		return true, "upstream"
	}
	return false, ""
}

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

func (o *prometheusObserver) OnAttemptDone(ctx *Context, ch *Channel, _ *Key, resp *http.Response, err error, result AttemptResult) {
	protocol, channel, channelType := o.labelValues(ctx, ch)
	o.channelAttemptTotal.WithLabelValues(protocol, channel, channelType, normalizePrometheusLabel(string(result), "unknown")).Inc()

	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	} else if ctx != nil && ctx.LastStatusCode > 0 {
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
	protocol := normalizePrometheusLabel(o.labeler.Protocol(ctx), "unknown")
	o.retryTotal.WithLabelValues(protocol, o.labeler.RetryReason(reason)).Inc()
}

func (o *prometheusObserver) OnCircuitStateChange(*Context, *Channel, string, string, string) {}

func (o *prometheusObserver) OnSSEEvent(*Context, *Channel, *SSEEvent) {}

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

func (o *prometheusObserver) labelValues(ctx *Context, ch *Channel) (protocol string, channel string, channelType string) {
	protocol = normalizePrometheusLabel(o.labeler.Protocol(ctx), "unknown")
	channel = normalizePrometheusLabel(o.labeler.Channel(ctx, ch), "unknown")
	channelType = normalizePrometheusLabel(o.labeler.ChannelType(ctx, ch), "unknown")
	return protocol, channel, channelType
}

func observeDuration(vec *prometheus.HistogramVec, protocol string, channel string, d time.Duration) {
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
