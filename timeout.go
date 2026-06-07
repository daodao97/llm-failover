package failover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ErrAttemptTimeout 表示单次上游 attempt 在拿到响应头前耗尽了配置的 AttemptTimeout 预算。
// 若先耗尽的是 FailoverTimeout 总预算，错误仍保持为 context deadline exceeded。
// 它有别于客户端取消和总预算耗尽：attempt 超时计入熔断失败记账，
// 且只要总预算仍有剩余，管线会继续轮换同渠道下一个 key 以及后续渠道。
var ErrAttemptTimeout = errors.New("attempt timeout exceeded")

// IsAttemptTimeoutError 判断错误链中是否包含单次 attempt 超时。
func IsAttemptTimeoutError(err error) bool {
	return errors.Is(err, ErrAttemptTimeout)
}

func (p *Proxy) initFailoverDeadline(ctx *Context) {
	if p == nil || ctx == nil || p.cfg.FailoverTimeout <= 0 || !ctx.FailoverDeadline.IsZero() {
		return
	}
	ctx.FailoverDeadline = time.Now().Add(p.cfg.FailoverTimeout)
}

func failoverRequestWithDeadline(r *http.Request, ctx *Context) (*http.Request, context.CancelFunc) {
	if r == nil || ctx == nil || ctx.FailoverDeadline.IsZero() {
		return r, func() {}
	}
	requestCtx, cancel := context.WithDeadline(r.Context(), ctx.FailoverDeadline)
	req := r.WithContext(requestCtx)
	ctx.Request = req
	return req, cancel
}

func (p *Proxy) failoverBudgetError(ctx *Context) error {
	if ctx == nil || ctx.FailoverDeadline.IsZero() {
		return nil
	}

	remaining := time.Until(ctx.FailoverDeadline)
	if remaining <= 0 {
		return fmt.Errorf("failover timeout exceeded: %w", context.DeadlineExceeded)
	}
	if p != nil && p.cfg.MinAttemptTimeout > 0 && remaining < p.cfg.MinAttemptTimeout {
		return fmt.Errorf("failover remaining budget %s below minimum attempt budget %s: %w", remaining, p.cfg.MinAttemptTimeout, context.DeadlineExceeded)
	}
	return nil
}

type attemptTimeoutSource uint8

const (
	attemptTimeoutNone attemptTimeoutSource = iota
	attemptTimeoutAttempt
	attemptTimeoutFailover
)

type attemptDeadlineSpec struct {
	deadline time.Time
	source   attemptTimeoutSource
}

func (p *Proxy) attemptDeadline(r *http.Request, ctx *Context, now time.Time) attemptDeadlineSpec {
	spec := attemptDeadlineSpec{}
	if p != nil && p.cfg.AttemptTimeout > 0 {
		spec = attemptDeadlineSpec{
			deadline: now.Add(p.cfg.AttemptTimeout),
			source:   attemptTimeoutAttempt,
		}
	}
	if ctx != nil && !ctx.FailoverDeadline.IsZero() && (spec.deadline.IsZero() || !ctx.FailoverDeadline.After(spec.deadline)) {
		spec = attemptDeadlineSpec{
			deadline: ctx.FailoverDeadline,
			source:   attemptTimeoutFailover,
		}
	}
	if r != nil {
		if parentDeadline, ok := r.Context().Deadline(); ok && (spec.deadline.IsZero() || parentDeadline.Before(spec.deadline)) {
			spec = attemptDeadlineSpec{
				deadline: parentDeadline,
				source:   attemptTimeoutNone,
			}
		}
	}
	return spec
}

type attemptRequestCleanup func(resp *http.Response, err error) (attemptTimedOut bool)

func (p *Proxy) requestForAttempt(r *http.Request, ctx *Context) (*http.Request, attemptRequestCleanup, error) {
	if r == nil {
		return r, func(*http.Response, error) bool { return false }, nil
	}
	if err := p.failoverBudgetError(ctx); err != nil {
		return nil, nil, err
	}

	spec := p.attemptDeadline(r, ctx, time.Now())
	if spec.deadline.IsZero() {
		return r, func(*http.Response, error) bool { return false }, nil
	}

	var attemptCtx context.Context
	var cancel context.CancelFunc
	var attemptTimedOut atomic.Bool
	var stopAttemptTimer func() bool
	if spec.source == attemptTimeoutAttempt {
		attemptCtx, cancel = context.WithCancel(r.Context())
		timer := time.AfterFunc(time.Until(spec.deadline), func() {
			attemptTimedOut.Store(true)
			cancel()
		})
		stopAttemptTimer = timer.Stop
	} else {
		attemptCtx, cancel = context.WithDeadline(r.Context(), spec.deadline)
	}
	attemptReq := r.WithContext(attemptCtx)
	oldReq := (*http.Request)(nil)
	if ctx != nil {
		oldReq = ctx.Request
		ctx.Request = attemptReq
		ctx.AttemptDeadline = spec.deadline
	}

	cleanup := func(resp *http.Response, err error) bool {
		attemptTimerStopped := false
		if stopAttemptTimer != nil {
			attemptTimerStopped = stopAttemptTimer()
		}
		doneErr := attemptCtx.Err()
		if ctx != nil {
			ctx.Request = oldReq
			ctx.AttemptDeadline = time.Time{}
		}
		if resp != nil && err == nil && (doneErr == nil || attemptTimerStopped) {
			attachCancelOnClose(resp, cancel)
			return false
		}
		cancel()
		if stopAttemptTimer != nil {
			return attemptTimedOut.Load()
		}
		return false
	}

	return attemptReq, cleanup, nil
}

func sleepWithFailoverBudget(ctx context.Context, d time.Duration, deadline time.Time) {
	if d <= 0 {
		return
	}
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		if remaining < d {
			d = remaining
		}
	}
	sleep(ctx, d)
}

func attachCancelOnClose(resp *http.Response, cancel context.CancelFunc) {
	if resp == nil || cancel == nil {
		return
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &cancelOnClose{
		rc:     resp.Body,
		cancel: cancel,
	}
}

type cancelOnClose struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelOnClose) Read(p []byte) (int, error) {
	return r.rc.Read(p)
}

func (r *cancelOnClose) Close() error {
	err := r.rc.Close()
	if r.cancel != nil {
		r.once.Do(r.cancel)
	}
	return err
}
