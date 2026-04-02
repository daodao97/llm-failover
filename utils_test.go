package failover

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPeekBodyKeepsUnreadBytes(t *testing.T) {
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("abcdefg")),
	}

	peek, err := peekBody(resp, 3)
	if err != nil {
		t.Fatalf("peekBody failed: %v", err)
	}
	if string(peek) != "abc" {
		t.Fatalf("unexpected peek: %q", string(peek))
	}

	all, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read all failed: %v", err)
	}
	if string(all) != "abcdefg" {
		t.Fatalf("unexpected body after peek: %q", string(all))
	}

	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
}

func TestPeekBodyPreservesOriginalClose(t *testing.T) {
	var released int32
	resp := &http.Response{
		Body: &releaseOnClose{
			rc: io.NopCloser(strings.NewReader("event: message\ndata: ok\n\n")),
			release: func() {
				atomic.AddInt32(&released, 1)
			},
		},
	}

	if _, err := peekBody(resp, 16); err != nil {
		t.Fatalf("peekBody failed: %v", err)
	}

	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read all failed: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close second time failed: %v", err)
	}

	if got := atomic.LoadInt32(&released); got != 1 {
		t.Fatalf("release called %d times, want 1", got)
	}
}
