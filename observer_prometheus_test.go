package failover

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type testPrometheusLabeler struct{}

func (testPrometheusLabeler) Protocol(*Context) string                   { return "claude" }
func (testPrometheusLabeler) Channel(_ *Context, ch *Channel) string     { return ch.Name }
func (testPrometheusLabeler) ChannelType(_ *Context, ch *Channel) string { return string(ch.CType) }
func (testPrometheusLabeler) RetryReason(reason string) string {
	return normalizePrometheusLabel(reason, "unknown")
}
func (testPrometheusLabeler) ErrorType(errType string) string {
	return normalizePrometheusLabel(errType, "unknown")
}

func TestPrometheusObserverReusesCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	opts := PrometheusObserverOptions{
		Registerer: reg,
		Namespace:  "testproxy",
		Subsystem:  "proxy",
		Labeler:    testPrometheusLabeler{},
	}

	obs1, ok := NewPrometheusObserver(opts).(*prometheusObserver)
	if !ok {
		t.Fatalf("expected prometheusObserver")
	}
	obs2, ok := NewPrometheusObserver(opts).(*prometheusObserver)
	if !ok {
		t.Fatalf("expected prometheusObserver")
	}

	if obs1.channelAttemptTotal != obs2.channelAttemptTotal {
		t.Fatalf("expected repeated observer creation to reuse collectors")
	}

	ctx := &Context{
		LastStatusCode: 429,
		Stats: &Stats{
			DNSDuration:     10 * time.Millisecond,
			ConnectDuration: 20 * time.Millisecond,
			TLSDuration:     30 * time.Millisecond,
			TTFB:            40 * time.Millisecond,
		},
	}
	ch := &Channel{Name: "claude-pool", CType: CTypePool}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests}

	obs1.OnAttemptDone(ctx, ch, nil, resp, errors.New("rate limited"), AttemptResultFailed)
	obs1.OnRetry(ctx, ch, nil, "timeout")
	obs1.OnFinalError(ctx, &ErrorResponse{Type: "upstream_error", StatusCode: http.StatusBadGateway})

	if got := testutil.ToFloat64(obs1.channelAttemptTotal.WithLabelValues("claude", "claude_pool", "pool", "failed")); got != 1 {
		t.Fatalf("unexpected channel_attempt_total: got %v want 1", got)
	}
	if got := testutil.ToFloat64(obs1.retryTotal.WithLabelValues("claude", "timeout")); got != 1 {
		t.Fatalf("unexpected retry_total: got %v want 1", got)
	}
	if got := testutil.ToFloat64(obs1.finalFailureTotal.WithLabelValues("claude", "upstream_error", "502")); got != 1 {
		t.Fatalf("unexpected final_failure_total: got %v want 1", got)
	}
	if got := testutil.ToFloat64(obs1.upstreamStatusTotal.WithLabelValues("claude", "claude_pool", "429")); got != 1 {
		t.Fatalf("unexpected upstream_status_total: got %v want 1", got)
	}
	if got := testutil.ToFloat64(obs1.rateLimitHitTotal.WithLabelValues("claude", "upstream", "claude_pool", "pool")); got != 1 {
		t.Fatalf("unexpected rate_limit_hit_total: got %v want 1", got)
	}
}
