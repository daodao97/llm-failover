package failover

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisCircuitBreakerPrefix          = "llm-failover:circuit"
	defaultRedisCircuitBreakerLockTTL         = 2 * time.Second
	defaultRedisCircuitBreakerLockWait        = 100 * time.Millisecond
	defaultRedisCircuitBreakerLockRetry       = 10 * time.Millisecond
	defaultRedisCircuitBreakerStateTTL        = time.Minute
	redisCircuitBreakerReleaseLockScript      = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`
	redisCircuitBreakerDefaultLockTokenLength = 16
)

type RedisCircuitBreakerStoreOptions struct {
	Prefix            string
	LockTTL           time.Duration
	LockWait          time.Duration
	LockRetryInterval time.Duration
}

type RedisCircuitBreakerStore struct {
	client            redis.UniversalClient
	prefix            string
	lockTTL           time.Duration
	lockWait          time.Duration
	lockRetryInterval time.Duration
}

type redisChannelCircuitEvent struct {
	At           time.Time `json:"at"`
	Success      bool      `json:"success"`
	LatencyNanos int64     `json:"latency_nanos"`
	Slow         bool      `json:"slow"`
	Stream       bool      `json:"stream"`
}

type redisChannelCircuitState struct {
	Events             []redisChannelCircuitEvent `json:"events"`
	OpenUntil          time.Time                  `json:"open_until,omitempty"`
	HalfOpen           bool                       `json:"half_open,omitempty"`
	UpdatedAt          time.Time                  `json:"updated_at,omitempty"`
	ChannelID          int                        `json:"channel_id,omitempty"`
	Name               string                     `json:"name,omitempty"`
	OpenReason         string                     `json:"open_reason,omitempty"`
	LastFailureStatus  int                        `json:"last_failure_status,omitempty"`
	LastLatencyNanos   int64                      `json:"last_latency_nanos,omitempty"`
	CurrentCooldown    int64                      `json:"current_cooldown_nanos,omitempty"`
	ConsecutiveOpenCnt int                        `json:"consecutive_open_count,omitempty"`
}

func NewRedisCircuitBreakerStore(client redis.UniversalClient, opts RedisCircuitBreakerStoreOptions) *RedisCircuitBreakerStore {
	if opts.Prefix == "" {
		opts.Prefix = defaultRedisCircuitBreakerPrefix
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = defaultRedisCircuitBreakerLockTTL
	}
	if opts.LockWait <= 0 {
		opts.LockWait = defaultRedisCircuitBreakerLockWait
	}
	if opts.LockRetryInterval <= 0 {
		opts.LockRetryInterval = defaultRedisCircuitBreakerLockRetry
	}
	return &RedisCircuitBreakerStore{
		client:            client,
		prefix:            strings.TrimRight(opts.Prefix, ":"),
		lockTTL:           opts.LockTTL,
		lockWait:          opts.LockWait,
		lockRetryInterval: opts.LockRetryInterval,
	}
}

func (s *RedisCircuitBreakerStore) update(scope string, channelKey string, initial channelCircuitState, fn func(*channelCircuitState) (bool, time.Duration)) error {
	if s == nil || s.client == nil || channelKey == "" {
		return nil
	}

	ctx := context.Background()
	stateKey := s.stateKey(scope, channelKey)
	lockKey := s.lockKey(scope, channelKey)
	token, err := redisCircuitBreakerLockToken()
	if err != nil {
		return err
	}
	if err := s.acquireLock(ctx, lockKey, token); err != nil {
		return err
	}
	defer func() {
		_ = s.releaseLock(ctx, lockKey, token)
	}()

	state := cloneChannelCircuitState(&initial)
	raw, err := s.client.Get(ctx, stateKey).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
	case err != nil:
		return err
	default:
		loaded, err := decodeRedisChannelCircuitState(raw)
		if err != nil {
			return err
		}
		state = loaded
	}

	deleteState, ttl := fn(state)
	if deleteState {
		return s.client.Del(ctx, stateKey).Err()
	}
	if ttl <= 0 {
		ttl = defaultRedisCircuitBreakerStateTTL
	}

	encoded, err := encodeRedisChannelCircuitState(state)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, stateKey, encoded, ttl).Err()
}

func (s *RedisCircuitBreakerStore) snapshot(scope string) ([]channelCircuitStoreEntry, error) {
	if s == nil || s.client == nil {
		return nil, nil
	}

	ctx := context.Background()
	pattern := s.statePrefix(scope) + "*"
	var cursor uint64
	var entries []channelCircuitStoreEntry
	for {
		keys, nextCursor, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			raw, err := s.client.Get(ctx, key).Bytes()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return nil, err
			}
			state, err := decodeRedisChannelCircuitState(raw)
			if err != nil {
				return nil, err
			}
			channelKey, err := s.channelKeyFromStateKey(scope, key)
			if err != nil {
				return nil, err
			}
			entries = append(entries, channelCircuitStoreEntry{
				key:   channelKey,
				state: *state,
			})
		}
		if nextCursor == 0 {
			break
		}
		cursor = nextCursor
	}
	return entries, nil
}

func (s *RedisCircuitBreakerStore) reset(scope string) (int, error) {
	if s == nil || s.client == nil {
		return 0, nil
	}

	ctx := context.Background()
	pattern := s.statePrefix(scope) + "*"
	total := 0
	var cursor uint64
	for {
		keys, nextCursor, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return 0, err
		}
		if len(keys) > 0 {
			n, err := s.client.Del(ctx, keys...).Result()
			if err != nil {
				return 0, err
			}
			total += int(n)
		}
		if nextCursor == 0 {
			break
		}
		cursor = nextCursor
	}
	return total, nil
}

func (s *RedisCircuitBreakerStore) resetChannelByKey(scope string, channelKey string) (bool, error) {
	if s == nil || s.client == nil || channelKey == "" {
		return false, nil
	}

	n, err := s.client.Del(context.Background(), s.stateKey(scope, channelKey)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *RedisCircuitBreakerStore) acquireLock(ctx context.Context, lockKey string, token string) error {
	deadline := time.Now().Add(s.lockWait)
	for {
		ok, err := s.client.SetNX(ctx, lockKey, token, s.lockTTL).Result()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("redis circuit breaker lock timeout")
		}
		time.Sleep(s.lockRetryInterval)
	}
}

func (s *RedisCircuitBreakerStore) releaseLock(ctx context.Context, lockKey string, token string) error {
	return s.client.Eval(ctx, redisCircuitBreakerReleaseLockScript, []string{lockKey}, token).Err()
}

func (s *RedisCircuitBreakerStore) statePrefix(scope string) string {
	return s.prefix + ":state:" + url.PathEscape(scope) + ":"
}

func (s *RedisCircuitBreakerStore) stateKey(scope string, channelKey string) string {
	return s.statePrefix(scope) + url.PathEscape(channelKey)
}

func (s *RedisCircuitBreakerStore) lockKey(scope string, channelKey string) string {
	return s.prefix + ":lock:" + url.PathEscape(scope) + ":" + url.PathEscape(channelKey)
}

func (s *RedisCircuitBreakerStore) channelKeyFromStateKey(scope string, key string) (string, error) {
	encoded := strings.TrimPrefix(key, s.statePrefix(scope))
	return url.PathUnescape(encoded)
}

func redisCircuitBreakerLockToken() (string, error) {
	buf := make([]byte, redisCircuitBreakerDefaultLockTokenLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func encodeRedisChannelCircuitState(state *channelCircuitState) ([]byte, error) {
	redisState := redisChannelCircuitState{
		OpenUntil:          state.openUntil,
		HalfOpen:           state.halfOpen,
		UpdatedAt:          state.updatedAt,
		ChannelID:          state.channelID,
		Name:               state.name,
		OpenReason:         state.openReason,
		LastFailureStatus:  state.lastFailureStatus,
		LastLatencyNanos:   int64(state.lastLatency),
		CurrentCooldown:    int64(state.currentCooldown),
		ConsecutiveOpenCnt: state.consecutiveOpenCnt,
	}
	if len(state.events) > 0 {
		redisState.Events = make([]redisChannelCircuitEvent, 0, len(state.events))
		for _, event := range state.events {
			redisState.Events = append(redisState.Events, redisChannelCircuitEvent{
				At:           event.at,
				Success:      event.success,
				LatencyNanos: int64(event.latency),
				Slow:         event.slow,
				Stream:       event.stream,
			})
		}
	}
	return json.Marshal(redisState)
}

func decodeRedisChannelCircuitState(raw []byte) (*channelCircuitState, error) {
	var redisState redisChannelCircuitState
	if err := json.Unmarshal(raw, &redisState); err != nil {
		return nil, err
	}

	state := &channelCircuitState{
		openUntil:          redisState.OpenUntil,
		halfOpen:           redisState.HalfOpen,
		updatedAt:          redisState.UpdatedAt,
		channelID:          redisState.ChannelID,
		name:               redisState.Name,
		openReason:         redisState.OpenReason,
		lastFailureStatus:  redisState.LastFailureStatus,
		lastLatency:        time.Duration(redisState.LastLatencyNanos),
		currentCooldown:    time.Duration(redisState.CurrentCooldown),
		consecutiveOpenCnt: redisState.ConsecutiveOpenCnt,
	}
	if len(redisState.Events) > 0 {
		state.events = make([]channelCircuitEvent, 0, len(redisState.Events))
		for _, event := range redisState.Events {
			state.events = append(state.events, channelCircuitEvent{
				at:      event.At,
				success: event.Success,
				latency: time.Duration(event.LatencyNanos),
				slow:    event.Slow,
				stream:  event.Stream,
			})
		}
	}
	return state, nil
}
