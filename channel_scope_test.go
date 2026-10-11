package failover

import (
	"net/http"
	"testing"
	"time"
)

func TestChannelCircuitScopeIsolatesMixedChannelsAndSharesNativeState(t *testing.T) {
	store := newMemoryChannelCircuitStore()
	cfg := CircuitBreakerConfig{Enabled: true, MinSamples: 1, ErrorRateThreshold: 0.5, FailureWindow: time.Minute, Cooldown: time.Minute}
	mixed := newChannelCircuitBreaker("client", cfg, store)
	native := newChannelCircuitBreaker("upstream-a", cfg, store)
	a := NewChannel(1, "same-channel", "http://example.invalid")
	a.CircuitBreakerScope = "upstream-a"
	b := a
	b.CircuitBreakerScope = "upstream-b"
	if opened, _, _, _ := mixed.RecordFailure(&a, time.Millisecond, false, http.StatusTooManyRequests); !opened {
		t.Fatal("expected first scope to open")
	}
	if allowed, _, _ := mixed.Allow(&b); !allowed {
		t.Fatal("one upstream scope blocked another")
	}
	nativeChannel := a
	nativeChannel.CircuitBreakerScope = ""
	if allowed, _, _ := native.Allow(&nativeChannel); allowed {
		t.Fatal("mixed and native proxies did not share the same upstream scope")
	}
	mixed.RecordSuccess(&b, time.Millisecond, true)
	snapshots := mixed.Snapshot()
	if len(snapshots) != 2 || snapshots[0].Scope != "upstream-a" || snapshots[1].Scope != "upstream-b" || snapshots[0].FailureCount != 1 || snapshots[1].SuccessCount != 1 {
		t.Fatalf("wrong scope snapshots: %+v", snapshots)
	}
	if !mixed.ResetChannel(&a) {
		t.Fatal("failed to reset first scope")
	}
	snapshots = mixed.Snapshot()
	if len(snapshots) != 1 || snapshots[0].Scope != "upstream-b" {
		t.Fatalf("scoped reset removed unrelated upstream: %+v", snapshots)
	}
	if !mixed.ResetChannelByKey(channelCircuitKey(&b)) || len(mixed.Snapshot()) != 0 {
		t.Fatal("key reset did not include channel scopes")
	}
	mixed.RecordFailure(&a, 0, false, 502)
	mixed.RecordFailure(&b, 0, false, 502)
	if mixed.Reset() != 2 || len(mixed.Snapshot()) != 0 {
		t.Fatal("full reset did not clear all used scopes")
	}
}
