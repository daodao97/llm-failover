package failover

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

type failingCircuitStore struct{ err error }

func (s *failingCircuitStore) update(string, string, channelCircuitState, func(*channelCircuitState) (bool, time.Duration)) error {
	return s.err
}

func (s *failingCircuitStore) snapshot(string) ([]channelCircuitStoreEntry, error) {
	return nil, s.err
}

func (s *failingCircuitStore) reset(string) (int, error) { return 0, s.err }

func (s *failingCircuitStore) resetChannelByKey(string, string) (bool, error) {
	return false, s.err
}

func countWarns(logger *capturedLogger) int {
	count := 0
	for _, entry := range logger.entries {
		if entry.level == "warn" && entry.msg == "circuit breaker store error, degrading to fail-open" {
			count++
		}
	}
	return count
}

// TestChannelCircuitBreakerLogsStoreErrorsThrottled 验证 store 故障时熔断器 fail-open
// 且打节流 Warn 日志（30 秒内不重复），避免 Redis 故障时熔断静默失效。
func TestChannelCircuitBreakerLogsStoreErrorsThrottled(t *testing.T) {
	logger := &capturedLogger{}
	p := New(Config{
		Logger: logger,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled: true,
		},
		CircuitBreakerStore: &failingCircuitStore{err: errors.New("redis down")},
	})

	now := time.Unix(1700000000, 0)
	p.breaker.now = func() time.Time { return now }
	ch := &Channel{Id: 1, Name: "ch"}

	// store 故障：Allow fail-open，且打一条 Warn
	if allowed, _, _ := p.breaker.Allow(ch); !allowed {
		t.Fatal("store error should fail open")
	}
	if got := countWarns(logger); got != 1 {
		t.Fatalf("warns=%d, want=1 after first store error", got)
	}

	// 节流窗口内的后续错误不再打日志
	p.breaker.RecordFailure(ch, 0, false, http.StatusBadGateway)
	p.breaker.RecordSuccess(ch, 0, false)
	if got := countWarns(logger); got != 1 {
		t.Fatalf("warns=%d, want=1 within throttle window", got)
	}

	// 超过节流窗口后允许再打一条
	now = now.Add(31 * time.Second)
	p.breaker.Allow(ch)
	if got := countWarns(logger); got != 2 {
		t.Fatalf("warns=%d, want=2 after throttle window", got)
	}
}
