package failover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type capturedLogEntry struct {
	level string
	msg   string
	kv    []any
}

type capturedLogger struct {
	entries []capturedLogEntry
}

func (l *capturedLogger) DebugCtx(ctx context.Context, msg string, kv ...any) {
	l.entries = append(l.entries, capturedLogEntry{level: "debug", msg: msg, kv: append([]any(nil), kv...)})
}

func (l *capturedLogger) InfoCtx(ctx context.Context, msg string, kv ...any) {
	l.entries = append(l.entries, capturedLogEntry{level: "info", msg: msg, kv: append([]any(nil), kv...)})
}

func (l *capturedLogger) WarnCtx(ctx context.Context, msg string, kv ...any) {
	l.entries = append(l.entries, capturedLogEntry{level: "warn", msg: msg, kv: append([]any(nil), kv...)})
}

func (l *capturedLogger) ErrorCtx(ctx context.Context, msg string, kv ...any) {
	l.entries = append(l.entries, capturedLogEntry{level: "error", msg: msg, kv: append([]any(nil), kv...)})
}

func TestNewDefaultsLoggerToNoop(t *testing.T) {
	p := New(Config{})
	if p.cfg.Logger == nil {
		t.Fatal("expected default logger to be initialized")
	}
}

func TestEnabledChannelsUsesInjectedLogger(t *testing.T) {
	logger := &capturedLogger{}
	p := New(Config{
		Logger: logger,
		Channels: []Channel{
			{Id: 1, Name: "primary", Enabled: true},
			{Id: 2, Name: "disabled", Enabled: false},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	channels, err := p.enabledChannels(req)
	if err != nil {
		t.Fatalf("enabledChannels error: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("len(channels)=%d, want=1", len(channels))
	}
	if len(logger.entries) == 0 {
		t.Fatal("expected logger to record at least one entry")
	}

	entry := logger.entries[len(logger.entries)-1]
	if entry.level != "debug" {
		t.Fatalf("level=%q, want=debug", entry.level)
	}
	if entry.msg != "enabledChannels" {
		t.Fatalf("msg=%q, want=enabledChannels", entry.msg)
	}
}
