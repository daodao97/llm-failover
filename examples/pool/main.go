package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"

	failover "github.com/daodao97/llm-failover"
)

func main() {
	var key1Failed atomic.Bool

	pool, err := failover.BuildPoolChannel(failover.PoolChannelConfig{
		Id:             31,
		Name:           "account-pool",
		DefaultBaseURL: "",
		GetKeys: func(ctx *failover.Context) []failover.Key {
			return []failover.Key{
				{ID: "k1", Value: "token-1"},
				{ID: "k2", Value: "token-2"},
			}
		},
		Handler: func(ctx *failover.Context) (*http.Response, error) {
			if ctx.CurrentKey != nil && ctx.CurrentKey.ID == "k1" && !key1Failed.Load() {
				key1Failed.Store(true)
				return nil, fmt.Errorf("temporary upstream error on %s", ctx.CurrentKey.ID)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":true,"key":"%s"}`, ctx.CurrentKey.ID))),
			}, nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	p := failover.New(failover.Config{
		Channels: []failover.Channel{pool},
		Retry:    failover.DefaultRetry(),
	})

	http.Handle("/v1/messages", p)
	log.Println("listening on :8082")
	log.Fatal(http.ListenAndServe(":8082", nil))
}
