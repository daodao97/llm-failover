package failover

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

const (
	defaultChannelCircuitMinSamples         = 3
	defaultChannelCircuitErrorRateThreshold = 1.0
	defaultChannelCircuitFailureWindow      = 30 * time.Second
	defaultChannelCircuitCooldown           = 15 * time.Second
)

type ChannelHealthSnapshot struct {
	Scope                    string    `json:"scope"`
	ChannelKey               string    `json:"channel_key"`
	ChannelID                int       `json:"channel_id"`
	ChannelName              string    `json:"channel_name"`
	Status                   string    `json:"status"`
	OpenReason               string    `json:"open_reason"`
	LastFailureStatusCode    int       `json:"last_failure_status_code"`
	RequestCount             int       `json:"request_count"`
	SuccessCount             int       `json:"success_count"`
	FailureCount             int       `json:"failure_count"`
	SlowCount                int       `json:"slow_count"`
	StreamRequestCount       int       `json:"stream_request_count"`
	StreamSuccessCount       int       `json:"stream_success_count"`
	StreamFailureCount       int       `json:"stream_failure_count"`
	StreamSlowCount          int       `json:"stream_slow_count"`
	NonStreamRequestCount    int       `json:"non_stream_request_count"`
	NonStreamSuccessCount    int       `json:"non_stream_success_count"`
	NonStreamFailureCount    int       `json:"non_stream_failure_count"`
	NonStreamSlowCount       int       `json:"non_stream_slow_count"`
	MinSamples               int       `json:"min_samples"`
	ErrorRate                float64   `json:"error_rate"`
	ErrorRateThreshold       float64   `json:"error_rate_threshold"`
	SlowRate                 float64   `json:"slow_rate"`
	SlowRateThreshold        float64   `json:"slow_rate_threshold"`
	FailureWindowSec         int64     `json:"failure_window_sec"`
	CooldownSec              int64     `json:"cooldown_sec"`
	OpenRemainingSec         int64     `json:"open_remaining_sec"`
	ConsecutiveOpenCount     int       `json:"consecutive_open_count"`
	AvgLatencyMs             int64     `json:"avg_latency_ms"`
	P95LatencyMs             int64     `json:"p95_latency_ms"`
	MaxLatencyMs             int64     `json:"max_latency_ms"`
	StreamAvgLatencyMs       int64     `json:"stream_avg_latency_ms"`
	StreamP95LatencyMs       int64     `json:"stream_p95_latency_ms"`
	StreamMaxLatencyMs       int64     `json:"stream_max_latency_ms"`
	NonStreamAvgLatencyMs    int64     `json:"non_stream_avg_latency_ms"`
	NonStreamP95LatencyMs    int64     `json:"non_stream_p95_latency_ms"`
	NonStreamMaxLatencyMs    int64     `json:"non_stream_max_latency_ms"`
	LastLatencyMs            int64     `json:"last_latency_ms"`
	LastSuccessLatencyMs     int64     `json:"last_success_latency_ms"`
	LastFailureLatencyMs     int64     `json:"last_failure_latency_ms"`
	SlowThresholdMs          int64     `json:"slow_threshold_ms"`
	LastLatencyMode          string    `json:"last_latency_mode"`
	StreamSlowThresholdMs    int64     `json:"stream_slow_threshold_ms"`
	NonStreamSlowThresholdMs int64     `json:"non_stream_slow_threshold_ms"`
	LastSuccessAt            time.Time `json:"last_success_at,omitempty"`
	LastFailureAt            time.Time `json:"last_failure_at,omitempty"`
	OpenUntil                time.Time `json:"open_until,omitempty"`
	UpdatedAt                time.Time `json:"updated_at,omitempty"`
}

type channelCircuitEvent struct {
	at      time.Time
	success bool
	latency time.Duration
	slow    bool
	stream  bool
}

type channelCircuitState struct {
	events             []channelCircuitEvent
	openUntil          time.Time
	halfOpen           bool
	updatedAt          time.Time
	channelID          int
	name               string
	openReason         string
	lastFailureStatus  int
	lastLatency        time.Duration
	currentCooldown    time.Duration
	consecutiveOpenCnt int
}

// channelCircuitBreaker 维护渠道短时间失败状态，避免持续串行试错。
type channelCircuitBreaker struct {
	mu     sync.Mutex
	now    func() time.Time
	scope  string
	cfg    CircuitBreakerConfig
	states map[string]*channelCircuitState
}

type channelEventSummary struct {
	RequestCount          int
	SuccessCount          int
	FailureCount          int
	SlowCount             int
	StreamRequestCount    int
	StreamSuccessCount    int
	StreamFailureCount    int
	StreamSlowCount       int
	NonStreamRequestCount int
	NonStreamSuccessCount int
	NonStreamFailureCount int
	NonStreamSlowCount    int
	TotalLatency          time.Duration
	StreamTotalLatency    time.Duration
	NonStreamTotalLatency time.Duration
	MaxLatency            time.Duration
	StreamMaxLatency      time.Duration
	NonStreamMaxLatency   time.Duration
	LastLatency           time.Duration
	LastLatencyMode       string
	LastSuccessAt         time.Time
	LastFailureAt         time.Time
	LastSuccessLatency    time.Duration
	LastFailureLatency    time.Duration
	Latencies             []time.Duration
	StreamLatencies       []time.Duration
	NonStreamLatencies    []time.Duration
}

type channelBreakerRegistry struct {
	mu       sync.RWMutex
	breakers []*channelCircuitBreaker
}

var globalChannelBreakerRegistry = &channelBreakerRegistry{}

func newChannelCircuitBreaker(scope string, cfg CircuitBreakerConfig) *channelCircuitBreaker {
	cfg = normalizeCircuitBreakerConfig(cfg)
	if !cfg.Enabled {
		return nil
	}
	breaker := &channelCircuitBreaker{
		now:    time.Now,
		scope:  scope,
		cfg:    cfg,
		states: make(map[string]*channelCircuitState),
	}
	globalChannelBreakerRegistry.register(breaker)
	return breaker
}

func normalizeCircuitBreakerConfig(cfg CircuitBreakerConfig) CircuitBreakerConfig {
	if !cfg.Enabled {
		return CircuitBreakerConfig{}
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = defaultChannelCircuitMinSamples
	}
	if cfg.ErrorRateThreshold <= 0 {
		cfg.ErrorRateThreshold = defaultChannelCircuitErrorRateThreshold
	}
	if cfg.FailureWindow <= 0 {
		cfg.FailureWindow = defaultChannelCircuitFailureWindow
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultChannelCircuitCooldown
	}
	if effectiveStreamSlowThreshold(cfg) > 0 && cfg.SlowRateThreshold <= 0 {
		cfg.SlowRateThreshold = 1
	}
	return cfg
}

func (b *channelCircuitBreaker) Allow(ch *Channel) (bool, time.Duration, bool) {
	if b == nil || ch == nil {
		return true, 0, false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	state := b.stateForLocked(ch)
	state.events = trimEvents(state.events, now, b.cfg.FailureWindow)
	state.updatedAt = now
	if len(state.events) == 0 && !state.openUntil.After(now) && !state.halfOpen {
		delete(b.states, channelCircuitKey(ch))
		return true, 0, false
	}

	if state.openUntil.After(now) {
		return false, time.Until(state.openUntil), false
	}

	if !state.openUntil.IsZero() {
		if state.halfOpen {
			return false, 0, true
		}
		state.halfOpen = true
		return true, 0, true
	}

	return true, 0, false
}

func (b *channelCircuitBreaker) RecordSuccess(ch *Channel, latency time.Duration, stream bool) (opened bool, wait time.Duration, reason string) {
	if b == nil || ch == nil {
		return false, 0, ""
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	state := b.stateForLocked(ch)
	event := channelCircuitEvent{
		at:      now,
		success: true,
		latency: latency,
		slow:    isSlowEvent(latency, stream, b.cfg),
		stream:  stream,
	}
	state.events = append(trimEvents(state.events, now, b.cfg.FailureWindow), event)
	state.updatedAt = now
	state.lastLatency = latency

	if state.halfOpen {
		state.events = nil
		state.halfOpen = false
		state.openUntil = time.Time{}
		state.openReason = ""
		state.currentCooldown = 0
		state.consecutiveOpenCnt = 0
		return false, 0, ""
	}
	if state.openUntil.After(now) {
		state.openUntil = time.Time{}
	}

	summary := summarizeEvents(state.events)
	if summary.RequestCount == 0 {
		return false, 0, ""
	}
	errorRate := float64(summary.FailureCount) / float64(summary.RequestCount)
	slowRate := float64(summary.SlowCount) / float64(summary.RequestCount)
	shouldOpen, openReason := b.shouldOpen(state, summary.RequestCount, errorRate, slowRate)
	if !shouldOpen {
		return false, 0, ""
	}

	state.halfOpen = false
	wait = b.nextCooldownLocked(state)
	state.openUntil = now.Add(wait)
	state.openReason = openReason
	return true, wait, openReason
}

func (b *channelCircuitBreaker) RecordFailure(ch *Channel, latency time.Duration, stream bool, statusCode int) (opened bool, reopen bool, wait time.Duration, reason string) {
	if b == nil || ch == nil {
		return false, false, 0, ""
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	state := b.stateForLocked(ch)
	event := channelCircuitEvent{
		at:      now,
		success: false,
		latency: latency,
		slow:    isSlowEvent(latency, stream, b.cfg),
		stream:  stream,
	}
	state.events = append(trimEvents(state.events, now, b.cfg.FailureWindow), event)
	state.updatedAt = now
	state.lastLatency = latency
	state.lastFailureStatus = statusCode

	summary := summarizeEvents(state.events)
	errorRate := 0.0
	if summary.RequestCount > 0 {
		errorRate = float64(summary.FailureCount) / float64(summary.RequestCount)
	}
	slowRate := 0.0
	if summary.RequestCount > 0 {
		slowRate = float64(summary.SlowCount) / float64(summary.RequestCount)
	}

	shouldOpen, openReason := b.shouldOpen(state, summary.RequestCount, errorRate, slowRate)
	if !shouldOpen {
		return false, false, 0, ""
	}

	reopen = state.halfOpen
	state.halfOpen = false
	wait = b.nextCooldownLocked(state)
	state.openUntil = now.Add(wait)
	state.openReason = openReason
	return true, reopen, wait, openReason
}

func (b *channelCircuitBreaker) stateForLocked(ch *Channel) *channelCircuitState {
	key := channelCircuitKey(ch)
	if state, ok := b.states[key]; ok {
		if ch.Id > 0 {
			state.channelID = ch.Id
		}
		if ch.Name != "" {
			state.name = ch.Name
		}
		return state
	}
	state := &channelCircuitState{
		channelID: ch.Id,
		name:      ch.Name,
	}
	b.states[key] = state
	return state
}

func (b *channelCircuitBreaker) Snapshot() []ChannelHealthSnapshot {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	snapshots := make([]ChannelHealthSnapshot, 0, len(b.states))
	for key, state := range b.states {
		state.events = trimEvents(state.events, now, b.cfg.FailureWindow)
		if len(state.events) == 0 && !state.openUntil.After(now) && !state.halfOpen {
			delete(b.states, key)
			continue
		}
		summary := summarizeEvents(state.events)
		errorRate := 0.0
		if summary.RequestCount > 0 {
			errorRate = float64(summary.FailureCount) / float64(summary.RequestCount)
		}
		slowRate := 0.0
		avgLatencyMs := int64(0)
		streamAvgLatencyMs := int64(0)
		nonStreamAvgLatencyMs := int64(0)
		if summary.RequestCount > 0 {
			slowRate = float64(summary.SlowCount) / float64(summary.RequestCount)
			avgLatencyMs = summary.TotalLatency.Milliseconds() / int64(summary.RequestCount)
		}
		if summary.StreamRequestCount > 0 {
			streamAvgLatencyMs = summary.StreamTotalLatency.Milliseconds() / int64(summary.StreamRequestCount)
		}
		if summary.NonStreamRequestCount > 0 {
			nonStreamAvgLatencyMs = summary.NonStreamTotalLatency.Milliseconds() / int64(summary.NonStreamRequestCount)
		}

		status := "closed"
		switch {
		case state.openUntil.After(now):
			status = "open"
		case state.halfOpen:
			status = "half_open"
		}

		openRemainingSec := int64(0)
		if state.openUntil.After(now) {
			openRemainingSec = int64(math.Ceil(state.openUntil.Sub(now).Seconds()))
		}

		snapshots = append(snapshots, ChannelHealthSnapshot{
			Scope:                    b.scope,
			ChannelKey:               key,
			ChannelID:                state.channelID,
			ChannelName:              state.name,
			Status:                   status,
			OpenReason:               state.openReason,
			LastFailureStatusCode:    state.lastFailureStatus,
			RequestCount:             summary.RequestCount,
			SuccessCount:             summary.SuccessCount,
			FailureCount:             summary.FailureCount,
			SlowCount:                summary.SlowCount,
			StreamRequestCount:       summary.StreamRequestCount,
			StreamSuccessCount:       summary.StreamSuccessCount,
			StreamFailureCount:       summary.StreamFailureCount,
			StreamSlowCount:          summary.StreamSlowCount,
			NonStreamRequestCount:    summary.NonStreamRequestCount,
			NonStreamSuccessCount:    summary.NonStreamSuccessCount,
			NonStreamFailureCount:    summary.NonStreamFailureCount,
			NonStreamSlowCount:       summary.NonStreamSlowCount,
			MinSamples:               b.cfg.MinSamples,
			ErrorRate:                errorRate,
			ErrorRateThreshold:       b.cfg.ErrorRateThreshold,
			SlowRate:                 slowRate,
			SlowRateThreshold:        b.cfg.SlowRateThreshold,
			FailureWindowSec:         int64(b.cfg.FailureWindow / time.Second),
			CooldownSec:              int64(state.cooldownForDisplay(b.cfg.Cooldown) / time.Second),
			OpenRemainingSec:         openRemainingSec,
			ConsecutiveOpenCount:     state.consecutiveOpenCnt,
			AvgLatencyMs:             avgLatencyMs,
			P95LatencyMs:             latencyPercentileMs(summary.Latencies, 0.95),
			MaxLatencyMs:             summary.MaxLatency.Milliseconds(),
			StreamAvgLatencyMs:       streamAvgLatencyMs,
			StreamP95LatencyMs:       latencyPercentileMs(summary.StreamLatencies, 0.95),
			StreamMaxLatencyMs:       summary.StreamMaxLatency.Milliseconds(),
			NonStreamAvgLatencyMs:    nonStreamAvgLatencyMs,
			NonStreamP95LatencyMs:    latencyPercentileMs(summary.NonStreamLatencies, 0.95),
			NonStreamMaxLatencyMs:    summary.NonStreamMaxLatency.Milliseconds(),
			LastLatencyMs:            summary.LastLatency.Milliseconds(),
			LastSuccessLatencyMs:     summary.LastSuccessLatency.Milliseconds(),
			LastFailureLatencyMs:     summary.LastFailureLatency.Milliseconds(),
			SlowThresholdMs:          selectedSlowThreshold(summary.LastLatencyMode == "stream", b.cfg).Milliseconds(),
			LastLatencyMode:          summary.LastLatencyMode,
			StreamSlowThresholdMs:    effectiveStreamSlowThreshold(b.cfg).Milliseconds(),
			NonStreamSlowThresholdMs: effectiveNonStreamSlowThreshold(b.cfg).Milliseconds(),
			LastSuccessAt:            summary.LastSuccessAt,
			LastFailureAt:            summary.LastFailureAt,
			OpenUntil:                state.openUntil,
			UpdatedAt:                state.updatedAt,
		})
	}

	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Scope != snapshots[j].Scope {
			return snapshots[i].Scope < snapshots[j].Scope
		}
		if snapshots[i].ChannelName != snapshots[j].ChannelName {
			return snapshots[i].ChannelName < snapshots[j].ChannelName
		}
		return snapshots[i].ChannelKey < snapshots[j].ChannelKey
	})

	return snapshots
}

func (b *channelCircuitBreaker) Reset() int {
	if b == nil {
		return 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	count := len(b.states)
	b.states = make(map[string]*channelCircuitState)
	return count
}

func (b *channelCircuitBreaker) ResetChannel(ch *Channel) bool {
	if ch == nil {
		return false
	}
	return b.ResetChannelByKey(channelCircuitKey(ch))
}

func (b *channelCircuitBreaker) ResetChannelByKey(channelKey string) bool {
	if b == nil || channelKey == "" {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.states[channelKey]; !ok {
		return false
	}
	delete(b.states, channelKey)
	return true
}

func (r *channelBreakerRegistry) register(b *channelCircuitBreaker) {
	if b == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.breakers = append(r.breakers, b)
}

func ListChannelHealthSnapshots() []ChannelHealthSnapshot {
	return globalChannelBreakerRegistry.snapshot()
}

// ResetChannelHealthStats 清空当前进程内所有熔断器的渠道统计和状态。
func ResetChannelHealthStats() int {
	return globalChannelBreakerRegistry.reset()
}

// ResetChannelHealthStatsForChannel 按渠道清空当前进程内所有熔断器的统计和状态。
func ResetChannelHealthStatsForChannel(ch *Channel) int {
	if ch == nil {
		return 0
	}
	return globalChannelBreakerRegistry.resetChannelByKey(channelCircuitKey(ch))
}

// ResetChannelHealthStatsByKey 按渠道 key 清空当前进程内所有熔断器的统计和状态。
func ResetChannelHealthStatsByKey(channelKey string) int {
	return globalChannelBreakerRegistry.resetChannelByKey(channelKey)
}

func (r *channelBreakerRegistry) snapshot() []ChannelHealthSnapshot {
	r.mu.RLock()
	breakers := append([]*channelCircuitBreaker(nil), r.breakers...)
	r.mu.RUnlock()

	var snapshots []ChannelHealthSnapshot
	for _, breaker := range breakers {
		snapshots = append(snapshots, breaker.Snapshot()...)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Scope != snapshots[j].Scope {
			return snapshots[i].Scope < snapshots[j].Scope
		}
		if snapshots[i].ChannelName != snapshots[j].ChannelName {
			return snapshots[i].ChannelName < snapshots[j].ChannelName
		}
		return snapshots[i].ChannelKey < snapshots[j].ChannelKey
	})
	return snapshots
}

func (r *channelBreakerRegistry) reset() int {
	if r == nil {
		return 0
	}

	r.mu.RLock()
	breakers := append([]*channelCircuitBreaker(nil), r.breakers...)
	r.mu.RUnlock()

	total := 0
	for _, breaker := range breakers {
		total += breaker.Reset()
	}
	return total
}

func (r *channelBreakerRegistry) resetChannelByKey(channelKey string) int {
	if r == nil || channelKey == "" {
		return 0
	}

	r.mu.RLock()
	breakers := append([]*channelCircuitBreaker(nil), r.breakers...)
	r.mu.RUnlock()

	resetCount := 0
	for _, breaker := range breakers {
		if breaker.ResetChannelByKey(channelKey) {
			resetCount++
		}
	}
	return resetCount
}

func channelCircuitKey(ch *Channel) string {
	if ch == nil {
		return ""
	}
	if ch.Id > 0 {
		return fmt.Sprintf("id:%d", ch.Id)
	}
	if ch.Name != "" {
		return "name:" + ch.Name
	}
	return "url:" + ch.BaseURL
}

func trimEvents(events []channelCircuitEvent, now time.Time, window time.Duration) []channelCircuitEvent {
	if len(events) == 0 {
		return events
	}

	cutoff := now.Add(-window)
	idx := 0
	for idx < len(events) && events[idx].at.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return events
	}
	return append([]channelCircuitEvent(nil), events[idx:]...)
}

func summarizeEvents(events []channelCircuitEvent) channelEventSummary {
	summary := channelEventSummary{}
	for _, event := range events {
		summary.RequestCount++
		summary.Latencies = append(summary.Latencies, event.latency)
		if event.stream {
			summary.StreamRequestCount++
			summary.StreamTotalLatency += event.latency
			summary.StreamLatencies = append(summary.StreamLatencies, event.latency)
			if event.latency > summary.StreamMaxLatency {
				summary.StreamMaxLatency = event.latency
			}
		} else {
			summary.NonStreamRequestCount++
			summary.NonStreamTotalLatency += event.latency
			summary.NonStreamLatencies = append(summary.NonStreamLatencies, event.latency)
			if event.latency > summary.NonStreamMaxLatency {
				summary.NonStreamMaxLatency = event.latency
			}
		}
		if event.success {
			summary.SuccessCount++
			summary.LastSuccessAt = event.at
			summary.LastSuccessLatency = event.latency
			if event.stream {
				summary.StreamSuccessCount++
			} else {
				summary.NonStreamSuccessCount++
			}
		} else {
			summary.FailureCount++
			summary.LastFailureAt = event.at
			summary.LastFailureLatency = event.latency
			if event.stream {
				summary.StreamFailureCount++
			} else {
				summary.NonStreamFailureCount++
			}
		}
		if event.slow {
			summary.SlowCount++
			if event.stream {
				summary.StreamSlowCount++
			} else {
				summary.NonStreamSlowCount++
			}
		}
		summary.TotalLatency += event.latency
		if event.latency > summary.MaxLatency {
			summary.MaxLatency = event.latency
		}
		summary.LastLatency = event.latency
		if event.stream {
			summary.LastLatencyMode = "stream"
		} else {
			summary.LastLatencyMode = "non_stream"
		}
	}
	return summary
}

func latencyPercentileMs(latencies []time.Duration, percentile float64) int64 {
	if len(latencies) == 0 {
		return 0
	}

	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	switch {
	case percentile <= 0:
		return sorted[0].Milliseconds()
	case percentile >= 1:
		return sorted[len(sorted)-1].Milliseconds()
	}

	idx := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx].Milliseconds()
}

func isSlowEvent(latency time.Duration, stream bool, cfg CircuitBreakerConfig) bool {
	threshold := selectedSlowThreshold(stream, cfg)
	return threshold > 0 && latency > 0 && latency >= threshold
}

func selectedSlowThreshold(stream bool, cfg CircuitBreakerConfig) time.Duration {
	if stream {
		return effectiveStreamSlowThreshold(cfg)
	}
	return effectiveNonStreamSlowThreshold(cfg)
}

func effectiveStreamSlowThreshold(cfg CircuitBreakerConfig) time.Duration {
	if cfg.StreamSlowThreshold > 0 {
		return cfg.StreamSlowThreshold
	}
	return cfg.SlowThreshold
}

func effectiveNonStreamSlowThreshold(cfg CircuitBreakerConfig) time.Duration {
	if cfg.NonStreamSlowThreshold > 0 {
		return cfg.NonStreamSlowThreshold
	}
	return cfg.SlowThreshold
}

func (b *channelCircuitBreaker) shouldOpen(state *channelCircuitState, requestCount int, errorRate float64, slowRate float64) (bool, string) {
	if state == nil {
		return false, ""
	}
	if state.halfOpen {
		return true, "probe_failed"
	}
	if requestCount < b.cfg.MinSamples {
		return false, ""
	}

	errorOpen := b.cfg.ErrorRateThreshold > 0 && errorRate >= b.cfg.ErrorRateThreshold
	slowEnabled := effectiveStreamSlowThreshold(b.cfg) > 0 || effectiveNonStreamSlowThreshold(b.cfg) > 0
	slowOpen := slowEnabled && b.cfg.SlowRateThreshold > 0 && slowRate >= b.cfg.SlowRateThreshold

	switch {
	case errorOpen && slowOpen:
		return true, "error_and_slow"
	case errorOpen:
		return true, "error_rate"
	case slowOpen:
		return true, "slow_rate"
	default:
		return false, ""
	}
}

func (b *channelCircuitBreaker) nextCooldownLocked(state *channelCircuitState) time.Duration {
	base := b.cfg.Cooldown
	if base <= 0 {
		base = defaultChannelCircuitCooldown
	}
	if state == nil || state.consecutiveOpenCnt == 0 || state.currentCooldown <= 0 {
		if state != nil {
			state.consecutiveOpenCnt = 1
			state.currentCooldown = base
		}
		return base
	}

	next := state.currentCooldown
	if next > time.Duration(math.MaxInt64/2) {
		next = time.Duration(math.MaxInt64)
	} else {
		next *= 2
	}
	state.consecutiveOpenCnt++
	state.currentCooldown = next
	return next
}

func (s *channelCircuitState) cooldownForDisplay(base time.Duration) time.Duration {
	if s == nil {
		return base
	}
	if s.currentCooldown > 0 {
		return s.currentCooldown
	}
	return base
}
