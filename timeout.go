package failover

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

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

func (p *Proxy) attemptTimeout(ctx *Context) time.Duration {
	if p == nil {
		return 0
	}

	timeout := p.cfg.AttemptTimeout
	if ctx != nil && !ctx.FailoverDeadline.IsZero() {
		remaining := time.Until(ctx.FailoverDeadline)
		if remaining <= 0 {
			return 0
		}
		if timeout <= 0 || remaining < timeout {
			timeout = remaining
		}
	}
	return timeout
}

type attemptRequestCleanup func(resp *http.Response, err error) (attemptTimedOut bool)

func (p *Proxy) requestForAttempt(r *http.Request, ctx *Context) (*http.Request, attemptRequestCleanup, error) {
	if r == nil {
		return r, func(*http.Response, error) bool { return false }, nil
	}
	if err := p.failoverBudgetError(ctx); err != nil {
		return nil, nil, err
	}

	timeout := p.attemptTimeout(ctx)
	if timeout <= 0 {
		return r, func(*http.Response, error) bool { return false }, nil
	}

	attemptCtx, cancel := context.WithCancel(r.Context())
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	attemptReq := r.WithContext(attemptCtx)
	oldReq := (*http.Request)(nil)
	if ctx != nil {
		oldReq = ctx.Request
		ctx.Request = attemptReq
		ctx.AttemptDeadline = time.Now().Add(timeout)
	}

	cleanup := func(resp *http.Response, err error) bool {
		stopped := timer.Stop()
		if ctx != nil {
			ctx.Request = oldReq
			ctx.AttemptDeadline = time.Time{}
		}
		if resp != nil && err == nil && stopped {
			attachCancelOnClose(resp, cancel)
			return false
		}
		cancel()
		return timedOut.Load()
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
