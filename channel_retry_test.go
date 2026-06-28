package failover

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type chunkedReadCloser struct {
	chunks    [][]byte
	readCalls int
}

func (r *chunkedReadCloser) Read(p []byte) (int, error) {
	r.readCalls++
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func (r *chunkedReadCloser) Close() error {
	return nil
}

type dataThenErrorReadCloser struct {
	data []byte
	err  error
	done bool
}

func (r *dataThenErrorReadCloser) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

func (r *dataThenErrorReadCloser) Close() error {
	return nil
}

func TestEvaluateRetryDecisionSkipsRetryOnResponseForHTTPSuccess(t *testing.T) {
	retryOnResponseCalled := false
	p := &Proxy{}
	ctx := &Context{
		TargetHeader: http.Header{
			"Accept": []string{"application/json"},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}

	decision := p.evaluateRetryDecision(ctx, RetryConfig{
		RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
			retryOnResponseCalled = true
			return false
		},
	}, resp)

	if retryOnResponseCalled {
		t.Fatalf("RetryOnResponse should not be called for http 200")
	}
	if decision.statusRetry {
		t.Fatalf("statusRetry should be false for http 200")
	}
}

func TestEvaluateRetryDecisionDoesNotBufferSuccessfulSSE(t *testing.T) {
	retryOnResponseCalled := false
	body := &chunkedReadCloser{
		chunks: [][]byte{
			[]byte("data: first\n\n"),
			[]byte("data: second\n\n"),
		},
	}
	p := &Proxy{}
	ctx := &Context{
		TargetHeader: http.Header{
			"Accept": []string{"text/event-stream"},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: body,
	}

	decision := p.evaluateRetryDecision(ctx, RetryConfig{
		RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
			retryOnResponseCalled = true
			return false
		},
		RetryOnSSE: func(isErr bool) bool {
			return false
		},
	}, resp)

	if retryOnResponseCalled {
		t.Fatalf("RetryOnResponse should not be called for successful SSE")
	}
	if !decision.isSSE {
		t.Fatalf("decision.isSSE should be true")
	}
	if decision.statusRetry {
		t.Fatalf("statusRetry should be false for successful SSE")
	}
	if body.readCalls != 1 {
		t.Fatalf("body.readCalls=%d, want=1", body.readCalls)
	}

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read remaining body failed: %v", err)
	}
	if string(rest) != "data: first\n\ndata: second\n\n" {
		t.Fatalf("unexpected body after peek: %q", string(rest))
	}
}

func TestEvaluateRetryDecisionSkipsSSEProbeForHTTPErrorStatus(t *testing.T) {
	retryOnSSECalled := false
	p := &Proxy{}
	ctx := &Context{
		TargetHeader: http.Header{
			"Accept": []string{"text/event-stream"},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader("")),
	}

	decision := p.evaluateRetryDecision(ctx, RetryConfig{
		RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
			return code >= http.StatusTooManyRequests
		},
		RetryOnSSE: func(isErr bool) bool {
			retryOnSSECalled = true
			return isErr
		},
	}, resp)

	if retryOnSSECalled {
		t.Fatalf("RetryOnSSE should not be called for HTTP error statuses")
	}
	if decision.reason != "" {
		t.Fatalf("decision.reason=%q, want empty", decision.reason)
	}
}

func TestPeekBodyPreservesBytesReadBeforeError(t *testing.T) {
	resp := &http.Response{
		Body: &dataThenErrorReadCloser{
			data: []byte("data: first\n\n"),
			err:  io.ErrUnexpectedEOF,
		},
	}

	peek, err := peekBody(resp, 512)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("err=%v, want %v", err, io.ErrUnexpectedEOF)
	}
	if string(peek) != "data: first\n\n" {
		t.Fatalf("peek=%q", string(peek))
	}

	rest, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read restored body failed: %v", readErr)
	}
	if string(rest) != "data: first\n\n" {
		t.Fatalf("restored body=%q", string(rest))
	}
}
