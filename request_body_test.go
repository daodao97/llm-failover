package failover

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sharedBodyTestChannel(id int) Channel {
	ch := NewChannel(id, "body-test", "https://fixture.invalid")
	ch.GetKeys = func(*Context) []Key { return []Key{{ID: "one", Value: "test"}} }
	return ch
}

func sharedBodyTestResponse(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestSharedRequestBodyFailoverAndEarlyRelease(t *testing.T) {
	original := []byte(`{"model":"test","input":"` + strings.Repeat("x", 1<<20) + `"}`)
	buffered := NewBufferedRequestBody(original)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", buffered)
	first, second := sharedBodyTestChannel(1), sharedBodyTestChannel(2)
	first.Handler = func(ctx *Context) (*http.Response, error) {
		if &ctx.RequestBody[0] != &original[0] || &ctx.OriginalRequestBody[0] != &original[0] {
			t.Fatal("the proxy copied the buffered request")
		}
		ctx.RequestBody = []byte(`{"input":"rewritten for failed channel"}`)
		return sharedBodyTestResponse(503, "application/json", `{"error":"retry"}`), nil
	}
	second.Handler = func(ctx *Context) (*http.Response, error) {
		if &ctx.RequestBody[0] != &original[0] || !bytes.Equal(ctx.RequestBody, original) {
			t.Fatal("failover did not restore the shared original")
		}
		return sharedBodyTestResponse(200, "application/json", `{"ok":true}`), nil
	}
	released, observed := 0, false
	p := New(Config{
		ShareRequestBody: true, Channels: []Channel{first, second}, Retry: NoRetry(),
		GetChannels: func(clone *http.Request) ([]Channel, error) {
			body, ok := clone.Body.(*BufferedRequestBody)
			if !ok || &body.Bytes()[0] != &original[0] {
				t.Fatal("channel selection copied the request")
			}
			return []Channel{first, second}, nil
		},
		OnRequestBodyRelease: func(ctx *Context) {
			released++
			if !bytes.Equal(ctx.OriginalRequestBody, original) {
				t.Fatal("original lost before the release hook")
			}
		},
		OnBody: func(ctx *Context, _ []byte) {
			observed = true
			if ctx.RequestBody != nil || ctx.OriginalRequestBody != nil || buffered.Bytes() != nil {
				t.Fatal("request bytes retained during response forwarding")
			}
			replay, err := r.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			if data, _ := io.ReadAll(replay); len(data) != 0 {
				t.Fatal("GetBody still retains released request bytes")
			}
		},
	})
	defer p.Close()
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || released != 1 || !observed {
		t.Fatalf("status=%d released=%d observed=%v", w.Code, released, observed)
	}
}

func TestSharedRequestBodySurvivesSSEPreflightRetry(t *testing.T) {
	original := []byte(`{"model":"test","stream":true}`)
	buffered := NewBufferedRequestBody(original)
	ch := sharedBodyTestChannel(1)
	calls, released := 0, 0
	ch.Handler = func(ctx *Context) (*http.Response, error) {
		calls++
		if !bytes.Equal(ctx.RequestBody, original) || released != 0 {
			t.Fatal("body released while SSE could still retry")
		}
		if calls == 1 {
			return sharedBodyTestResponse(200, "text/event-stream", "event: error\ndata: {\"error\":\"retry\"}\n\n"), nil
		}
		return sharedBodyTestResponse(200, "text/event-stream", "data: {\"ok\":true}\n\ndata: [DONE]\n\n"), nil
	}
	p := New(Config{ShareRequestBody: true, Channels: []Channel{ch},
		Retry:                RetryConfig{MaxAttempts: 2, RetryOnSSE: func(failed bool) bool { return failed }},
		OnRequestBodyRelease: func(*Context) { released++ },
		OnSSE: func(ctx *Context, _ *SSEEvent) {
			if ctx.OriginalRequestBody != nil || buffered.Bytes() != nil {
				t.Fatal("body retained after SSE retry decision")
			}
		},
	})
	defer p.Close()
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", buffered))
	if w.Code != 200 || calls != 2 || released != 1 {
		t.Fatalf("status=%d calls=%d released=%d", w.Code, calls, released)
	}
}

func TestDefaultRequestBodyStillAllowsInPlaceMutation(t *testing.T) {
	ch := sharedBodyTestChannel(1)
	ch.Handler = func(ctx *Context) (*http.Response, error) {
		ctx.RequestBody[0] = 'X'
		if ctx.OriginalRequestBody[0] == 'X' {
			t.Fatal("default mutable body aliases its original")
		}
		return sharedBodyTestResponse(200, "application/json", `{}`), nil
	}
	p := New(Config{Channels: []Channel{ch}, OnBody: func(ctx *Context, _ []byte) {
		if len(ctx.OriginalRequestBody) == 0 {
			t.Fatal("default mode released body early")
		}
	}})
	defer p.Close()
	p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"input":"test"}`)))
}

func TestSharedRequestBodyReplaysHTTPRedirect(t *testing.T) {
	original := `{"input":"` + strings.Repeat("x", 128<<10) + `"}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != original || r.ContentLength != int64(len(original)) {
			t.Error("redirect changed the upload or content length")
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	ch := sharedBodyTestChannel(1)
	ch.BaseURL = server.URL
	p := New(Config{ShareRequestBody: true, Channels: []Channel{ch}, Client: server.Client()})
	defer p.Close()
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/redirect", NewBufferedRequestBody([]byte(original))))
	if w.Code != 200 || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body)
	}
}

type sharedBodyTestTransport func(*http.Request) (*http.Response, error)

func (f sharedBodyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSharedRequestBodyReleaseDoesNotTruncateEarlyResponseUpload(t *testing.T) {
	original := []byte(`{"input":"` + strings.Repeat("x", 1<<20) + `"}`)
	var upload *http.Request
	client := &http.Client{Transport: sharedBodyTestTransport(func(r *http.Request) (*http.Response, error) {
		upload = r
		return sharedBodyTestResponse(200, "application/json", `{}`), nil
	})}
	ch := sharedBodyTestChannel(1)
	p := New(Config{ShareRequestBody: true, Channels: []Channel{ch}, Client: client,
		OnBody: func(*Context, []byte) {
			// 模拟上游先回响应头、HTTP transport 仍在读取上传正文。
			data, err := io.ReadAll(upload.Body)
			if err != nil || !bytes.Equal(data, original) {
				t.Fatal("release truncated an active upload")
			}
			upload.Body.Close()
			replay, err := upload.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			if data, _ := io.ReadAll(replay); len(data) != 0 {
				t.Fatal("completed HTTP attempt retains a replay copy")
			}
		},
	})
	defer p.Close()
	p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", NewBufferedRequestBody(original)))
	if upload == nil || upload.Body.(*uploadBodyReader).reader != nil {
		t.Fatal("closed upload reader still retains its body")
	}
}

func TestSharedRequestBodyFinalErrorRetainsDiagnosticBodyUntilDone(t *testing.T) {
	original := []byte(`{"input":"test"}`)
	buffered := NewBufferedRequestBody(original)
	ch := sharedBodyTestChannel(1)
	ch.Handler = func(*Context) (*http.Response, error) {
		return sharedBodyTestResponse(400, "application/json", `{"error":"invalid"}`), nil
	}
	errors, released := 0, 0
	p := New(Config{ShareRequestBody: true, Channels: []Channel{ch}, Retry: NoRetry(),
		OnError: func(ctx *Context, _ *ErrorResponse) {
			errors++
			if !bytes.Equal(ctx.RequestBody, original) || len(buffered.Bytes()) == 0 {
				t.Fatal("error diagnostics lost the body")
			}
		},
		OnRequestBodyRelease: func(*Context) { released++ },
	})
	defer p.Close()
	p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", buffered))
	if errors != 1 || released != 1 || buffered.Bytes() != nil {
		t.Fatalf("errors=%d released=%d retained=%d", errors, released, len(buffered.Bytes()))
	}
}

func TestBufferedRequestBodyReplayReadersAreIndependent(t *testing.T) {
	body := NewBufferedRequestBody([]byte("abcdef"))
	second := body.buffer.reader()
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(body, prefix); err != nil || string(prefix) != "ab" {
		t.Fatal("first reader did not advance")
	}
	body.Close()
	if data, err := io.ReadAll(second); err != nil || string(data) != "abcdef" {
		t.Fatal("closing one reader affected replay")
	}
	body.buffer.release()
	if body.Bytes() != nil || second.Bytes() != nil {
		t.Fatal("reader aliases retain the released buffer")
	}
}

func BenchmarkBufferedRequestBodyPreparation(b *testing.B) {
	payload := []byte(`{"input":"` + strings.Repeat("x", 1<<20) + `"}`)
	for _, shared := range []bool{false, true} {
		name := "copied"
		if shared {
			name = "shared"
		}
		b.Run(name, func(b *testing.B) {
			p := New(Config{ShareRequestBody: shared})
			defer p.Close()
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				r := httptest.NewRequest(http.MethodPost, "/", NewBufferedRequestBody(payload))
				ctx := &Context{Request: r}
				if err := p.prepareRequestBody(httptest.NewRecorder(), ctx); err != nil {
					b.Fatal(err)
				}
				ctx.resetForChannel()
				p.releaseRequestBody(ctx)
			}
		})
	}
}
