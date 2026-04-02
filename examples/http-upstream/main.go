package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	failover "github.com/daodao97/llm-failover"
)

func main() {
	var jsonPrimaryFailed atomic.Bool
	var streamPrimaryFailed atomic.Bool

	primaryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/stream":
			if !streamPrimaryFailed.Load() {
				streamPrimaryFailed.Store(true)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"message":"primary SSE upstream failed"}}`))
				return
			}
			writeSSE(w, []string{
				`event: message`,
				`data: {"chunk":"primary-stream"}`,
				"",
			})
		default:
			if !jsonPrimaryFailed.Load() {
				jsonPrimaryFailed.Store(true)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"message":"primary upstream failed"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"upstream":"primary"}`))
		}
	}))
	defer primaryUpstream.Close()

	backupUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/stream":
			writeSSE(w, []string{
				`event: message`,
				`data: {"chunk":"backup-stream-1"}`,
				"",
				`event: message`,
				`data: {"chunk":"backup-stream-2"}`,
				"",
			})
		default:
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"upstream":"backup","body":` + quoteJSON(string(body)) + `}`))
		}
	}))
	defer backupUpstream.Close()

	primary := failover.NewChannel(101, "primary-http", primaryUpstream.URL)
	primary.GetKeys = func(ctx *failover.Context) []failover.Key {
		return []failover.Key{{ID: "primary-key", Value: "token-primary"}}
	}
	primary.Retry = &failover.RetryConfig{
		MaxAttempts: 1,
		RetryOnResponse: func(ctx *failover.Context, ch *failover.Channel, code int, body []byte) bool {
			return code >= http.StatusBadGateway
		},
	}

	backup := failover.NewChannel(102, "backup-http", backupUpstream.URL)
	backup.GetKeys = func(ctx *failover.Context) []failover.Key {
		return []failover.Key{{ID: "backup-key", Value: "token-backup"}}
	}

	p := failover.New(failover.Config{
		Channels: []failover.Channel{primary, backup},
		Retry:    failover.NoRetry(),
	})

	http.Handle("/v1/", p)
	log.Println("listening on :8083")
	log.Println(`json:   curl -sS http://127.0.0.1:8083/v1/messages -H 'content-type: application/json' -d '{"model":"demo","messages":[]}'`)
	log.Println(`sse:    curl -N http://127.0.0.1:8083/v1/messages/stream -H 'accept: text/event-stream'`)
	log.Fatal(http.ListenAndServe(":8083", nil))
}

func writeSSE(w http.ResponseWriter, lines []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for _, line := range lines {
		_, _ = fmt.Fprint(w, line+"\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func quoteJSON(s string) string {
	replacer := strings.NewReplacer(
		`\\`, `\\\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	)
	return `"` + replacer.Replace(s) + `"`
}
