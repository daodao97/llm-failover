package failover

import (
	"context"
	"log/slog"
)

// Logger 定义 failover 内部使用的最小日志接口。
// 使用方可以注入任意实现，例如 slog、zap 的适配器或 no-op logger。
type Logger interface {
	DebugCtx(ctx context.Context, msg string, kv ...any)
	InfoCtx(ctx context.Context, msg string, kv ...any)
	WarnCtx(ctx context.Context, msg string, kv ...any)
	ErrorCtx(ctx context.Context, msg string, kv ...any)
}

type noopLogger struct{}

func (noopLogger) DebugCtx(context.Context, string, ...any) {}
func (noopLogger) InfoCtx(context.Context, string, ...any)  {}
func (noopLogger) WarnCtx(context.Context, string, ...any)  {}
func (noopLogger) ErrorCtx(context.Context, string, ...any) {}

// SlogLogger 将标准库 slog.Logger 适配为 failover.Logger。
type SlogLogger struct {
	Logger *slog.Logger
}

// NewSlogLogger 返回一个基于 slog 的 Logger 适配器。
// 当 logger 为 nil 时，会回退到 slog.Default()。
func NewSlogLogger(logger *slog.Logger) Logger {
	return &SlogLogger{Logger: logger}
}

func (l *SlogLogger) DebugCtx(ctx context.Context, msg string, kv ...any) {
	l.slog().DebugContext(normalizeLogContext(ctx), msg, kv...)
}

func (l *SlogLogger) InfoCtx(ctx context.Context, msg string, kv ...any) {
	l.slog().InfoContext(normalizeLogContext(ctx), msg, kv...)
}

func (l *SlogLogger) WarnCtx(ctx context.Context, msg string, kv ...any) {
	l.slog().WarnContext(normalizeLogContext(ctx), msg, kv...)
}

func (l *SlogLogger) ErrorCtx(ctx context.Context, msg string, kv ...any) {
	l.slog().ErrorContext(normalizeLogContext(ctx), msg, kv...)
}

func (l *SlogLogger) slog() *slog.Logger {
	if l != nil && l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

func normalizeLogger(logger Logger) Logger {
	if logger != nil {
		return logger
	}
	return noopLogger{}
}

func normalizeLogContext(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (p *Proxy) logger() Logger {
	if p != nil && p.cfg.Logger != nil {
		return p.cfg.Logger
	}
	return noopLogger{}
}
