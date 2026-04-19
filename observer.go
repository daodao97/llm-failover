package failover

import "net/http"

type AttemptResult string

const (
	AttemptResultSuccess AttemptResult = "success"
	AttemptResultFailed  AttemptResult = "failed"
)

// Observer 定义可选的观测回调，便于统一接入 metrics/tracing。
// 该接口为可选能力，未注入时使用 no-op 实现。
type Observer interface {
	OnRequestStart(ctx *Context)
	OnRequestDone(ctx *Context, err error)

	OnChannelSelected(ctx *Context, ch *Channel)
	OnChannelSkipped(ctx *Context, ch *Channel, reason string)

	OnAttemptStart(ctx *Context, ch *Channel, key *Key)
	OnAttemptDone(ctx *Context, ch *Channel, key *Key, resp *http.Response, err error, result AttemptResult)
	OnRetry(ctx *Context, ch *Channel, key *Key, reason string)

	OnCircuitStateChange(ctx *Context, ch *Channel, from, to, reason string)

	OnSSEEvent(ctx *Context, ch *Channel, event *SSEEvent)

	OnFinalError(ctx *Context, errResp *ErrorResponse)
}

type noopObserver struct{}

func (noopObserver) OnRequestStart(*Context)                     {}
func (noopObserver) OnRequestDone(*Context, error)               {}
func (noopObserver) OnChannelSelected(*Context, *Channel)        {}
func (noopObserver) OnChannelSkipped(*Context, *Channel, string) {}
func (noopObserver) OnAttemptStart(*Context, *Channel, *Key)     {}
func (noopObserver) OnAttemptDone(*Context, *Channel, *Key, *http.Response, error, AttemptResult) {
}
func (noopObserver) OnRetry(*Context, *Channel, *Key, string) {}
func (noopObserver) OnCircuitStateChange(*Context, *Channel, string, string, string) {
}
func (noopObserver) OnSSEEvent(*Context, *Channel, *SSEEvent) {}
func (noopObserver) OnFinalError(*Context, *ErrorResponse)    {}
