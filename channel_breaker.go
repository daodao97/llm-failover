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
	FailureCount             int       `json:"failure_count"`
	SlowCount                int       `json:"slow_count"`
	MinSamples               int       `json:"min_samples"`
	ErrorRate                float64   `json:"error_rate"`
	ErrorRateThreshold       float64   `json:"error_rate_threshold"`
	SlowRate                 float64   `json:"slow_rate"`
	SlowRateThreshold        float64   `json:"slow_rate_threshold"`
	FailureWindowSec         int64     `json:"failure_window_sec"`
	CooldownSec              int64     `json:"cooldown_sec"`
	AvgLatencyMs             int64     `json:"avg_latency_ms"`
	MaxLatencyMs             int64     `json:"max_latency_ms"`
	LastLatencyMs            int64     `json:"last_latency_ms"`
	SlowThresholdMs          int64     `json:"slow_threshold_ms"`
	LastLatencyMode          string    `json:"last_latency_mode"`
	StreamSlowThresholdMs    int64     `json:"stream_slow_threshold_ms"`
	NonStreamSlowThresholdMs int64     `json:"non_stream_slow_threshold_ms"`
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

	requestCount, failureCount, slowCount, _, _, _, _ := summarizeEvents(state.events)
	if requestCount == 0 {
		return false, 0, ""
	}
	errorRate := float64(failureCount) / float64(requestCount)
	slowRate := float64(slowCount) / float64(requestCount)
	shouldOpen, openReason := b.shouldOpen(state, requestCount, errorRate, slowRate)
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

	requestCount, failureCount, slowCount, _, _, _, _ := summarizeEvents(state.events)
	errorRate := 0.0
	if requestCount > 0 {
		errorRate = float64(failureCount) / float64(requestCount)
	}
	slowRate := 0.0
	if requestCount > 0 {
		slowRate = float64(slowCount) / float64(requestCount)
	}

	shouldOpen, openReason := b.shouldOpen(state, requestCount, errorRate, slowRate)
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
		requestCount, failureCount, slowCount, totalLatency, maxLatency, lastLatency, lastLatencyMode := summarizeEvents(state.events)
		errorRate := 0.0
		if requestCount > 0 {
			errorRate = float64(failureCount) / float64(requestCount)
		}
		slowRate := 0.0
		avgLatencyMs := int64(0)
		if requestCount > 0 {
			slowRate = float64(slowCount) / float64(requestCount)
			avgLatencyMs = totalLatency.Milliseconds() / int64(requestCount)
		}

		status := "closed"
		switch {
		case state.openUntil.After(now):
			status = "open"
		case state.halfOpen:
			status = "half_open"
		}

		snapshots = append(snapshots, ChannelHealthSnapshot{
			Scope:                    b.scope,
			ChannelKey:               key,
			ChannelID:                state.channelID,
			ChannelName:              state.name,
			Status:                   status,
			OpenReason:               state.openReason,
			LastFailureStatusCode:    state.lastFailureStatus,
			RequestCount:             requestCount,
			FailureCount:             failureCount,
			SlowCount:                slowCount,
			MinSamples:               b.cfg.MinSamples,
			ErrorRate:                errorRate,
			ErrorRateThreshold:       b.cfg.ErrorRateThreshold,
			SlowRate:                 slowRate,
			SlowRateThreshold:        b.cfg.SlowRateThreshold,
			FailureWindowSec:         int64(b.cfg.FailureWindow / time.Second),
			CooldownSec:              int64(state.cooldownForDisplay(b.cfg.Cooldown) / time.Second),
			AvgLatencyMs:             avgLatencyMs,
			MaxLatencyMs:             maxLatency.Milliseconds(),
			LastLatencyMs:            lastLatency.Milliseconds(),
			SlowThresholdMs:          selectedSlowThreshold(lastLatencyMode == "stream", b.cfg).Milliseconds(),
			LastLatencyMode:          lastLatencyMode,
			StreamSlowThresholdMs:    effectiveStreamSlowThreshold(b.cfg).Milliseconds(),
			NonStreamSlowThresholdMs: effectiveNonStreamSlowThreshold(b.cfg).Milliseconds(),
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

func summarizeEvents(events []channelCircuitEvent) (requestCount int, failureCount int, slowCount int, totalLatency time.Duration, maxLatency time.Duration, lastLatency time.Duration, lastLatencyMode string) {
	for _, event := range events {
		requestCount++
		if !event.success {
			failureCount++
		}
		if event.slow {
			slowCount++
		}
		totalLatency += event.latency
		if event.latency > maxLatency {
			maxLatency = event.latency
		}
		lastLatency = event.latency
		if event.stream {
			lastLatencyMode = "stream"
		} else {
			lastLatencyMode = "non_stream"
		}
	}
	return requestCount, failureCount, slowCount, totalLatency, maxLatency, lastLatency, lastLatencyMode
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
