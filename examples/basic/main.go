package main

import (
	"io"
	"log"
	"net/http"
	"strings"

	failover "github.com/daodao97/llm-failover"
)

func main() {
	primary := failover.NewChannel(1, "primary", "")
	primary.GetKeys = func(ctx *failover.Context) []failover.Key {
		return []failover.Key{{ID: "primary", Value: "token-primary"}}
	}
	primary.Handler = func(ctx *failover.Context) (*http.Response, error) {
		// 模拟主渠道失败。
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"primary unavailable"}}`)),
		}, nil
	}
	primary.Retry = &failover.RetryConfig{
		MaxAttempts: 1,
		RetryOnResponse: func(ctx *failover.Context, ch *failover.Channel, code int, body []byte) bool {
			return code >= http.StatusBadGateway
		},
	}

	backup := failover.NewChannel(2, "backup", "")
	backup.GetKeys = func(ctx *failover.Context) []failover.Key {
		return []failover.Key{{ID: "backup", Value: "token-backup"}}
	}
	backup.Handler = func(ctx *failover.Context) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true,"channel":"backup"}`)),
		}, nil
	}

	p := failover.New(failover.Config{
		Channels: []failover.Channel{primary, backup},
		Retry:    failover.NoRetry(),
	})

	http.Handle("/v1/messages", p)
	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
