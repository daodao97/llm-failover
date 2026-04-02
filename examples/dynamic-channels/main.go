package main

import (
	"io"
	"log"
	"net/http"
	"strings"

	failover "github.com/daodao97/llm-failover"
)

func main() {
	p := failover.New(failover.Config{
		GetChannels: func(r *http.Request) ([]failover.Channel, error) {
			tier := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Tier")))
			if tier == "premium" {
				return []failover.Channel{
					newStaticChannel(11, "premium-primary", `{"tier":"premium","channel":"primary"}`),
					newStaticChannel(12, "premium-backup", `{"tier":"premium","channel":"backup"}`),
				}, nil
			}
			return []failover.Channel{
				newStaticChannel(21, "standard-primary", `{"tier":"standard","channel":"primary"}`),
				newStaticChannel(22, "standard-backup", `{"tier":"standard","channel":"backup"}`),
			}, nil
		},
		Retry: failover.NoRetry(),
	})

	http.Handle("/v1/messages", p)
	log.Println("listening on :8081")
	log.Fatal(http.ListenAndServe(":8081", nil))
}

func newStaticChannel(id int, name, body string) failover.Channel {
	ch := failover.NewChannel(id, name, "")
	ch.GetKeys = func(ctx *failover.Context) []failover.Key {
		return []failover.Key{{ID: name + "-key", Value: "token"}}
	}
	ch.Handler = func(ctx *failover.Context) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	return ch
}
