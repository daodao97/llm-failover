package failover

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBuildModelRewriteRulesFiltersInvalidEntries(t *testing.T) {
	input := []ThirdModelRewrite{
		{Key: " ", Value: "x"},
		{Key: "claude-*", Value: " "},
		{Key: "  claude-sonnet-*  ", Value: "  claude-3-5-sonnet  "},
	}

	got := buildModelRewriteRules(input)
	want := []ModelRewriteRule{
		{Source: "claude-sonnet-*", Target: "claude-3-5-sonnet"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules=%v, want=%v", got, want)
	}
}

func TestRewriteModelByRulesSequentialPriority(t *testing.T) {
	rules := []ModelRewriteRule{
		{Source: "claude-*", Target: "model-by-wildcard"},
		{Source: "claude-sonnet-4-5", Target: "model-by-exact"},
	}

	got, matched := rewriteModelByRules("claude-sonnet-4-5", rules)
	if !matched {
		t.Fatal("expected matched=true")
	}
	if got != "model-by-wildcard" {
		t.Fatalf("model=%q, want=%q", got, "model-by-wildcard")
	}
}

func TestRewriteModelByRulesSupportsExactAndWildcard(t *testing.T) {
	rules := []ModelRewriteRule{
		{Source: "claude-opus-4-5", Target: "rewritten-exact"},
		{Source: "claude-sonnet-*", Target: "rewritten-wildcard"},
	}

	gotExact, matchedExact := rewriteModelByRules("claude-opus-4-5", rules)
	if !matchedExact || gotExact != "rewritten-exact" {
		t.Fatalf("exact result=(%q,%v), want=(%q,true)", gotExact, matchedExact, "rewritten-exact")
	}

	gotWildcard, matchedWildcard := rewriteModelByRules("claude-sonnet-4-5", rules)
	if !matchedWildcard || gotWildcard != "rewritten-wildcard" {
		t.Fatalf("wildcard result=(%q,%v), want=(%q,true)", gotWildcard, matchedWildcard, "rewritten-wildcard")
	}
}

func TestRewriteModelByRulesSkipsInvalidPattern(t *testing.T) {
	rules := []ModelRewriteRule{
		{Source: "[invalid", Target: "bad"},
		{Source: "claude-*", Target: "good"},
	}

	got, matched := rewriteModelByRules("claude-sonnet-4-5", rules)
	if !matched {
		t.Fatal("expected matched=true")
	}
	if got != "good" {
		t.Fatalf("model=%q, want=%q", got, "good")
	}
}

func TestPrepareChannelAttemptAppliesModelRewrite(t *testing.T) {
	p := &Proxy{}
	reqBody := []byte(`{"model":"claude-sonnet-4-5"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx := &Context{
		Request:     req,
		RequestBody: append([]byte(nil), reqBody...),
	}
	ch := &Channel{
		Name:    "third",
		BaseURL: "https://example.com",
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		ModelRewrite: []ModelRewriteRule{
			{Source: "claude-*", Target: "rewritten-by-channel"},
		},
	}

	_, _, _, err := p.prepareChannelAttempt(req, ctx, ch)
	if err != nil {
		t.Fatalf("prepareChannelAttempt error: %v", err)
	}

	got := gjson.GetBytes(ctx.RequestBody, "model").String()
	if got != "rewritten-by-channel" {
		t.Fatalf("request model=%q, want=%q", got, "rewritten-by-channel")
	}
	if ctx.Model != "rewritten-by-channel" {
		t.Fatalf("ctx.Model=%q, want=%q", ctx.Model, "rewritten-by-channel")
	}
}

func TestExtractModelFromPath(t *testing.T) {
	model, ok := extractModelFromPath("https://example.com/v1beta/models/gemini-2.5-flash-image:generateContent?alt=sse")
	if !ok {
		t.Fatal("expected model extracted from path")
	}
	if model != "gemini-2.5-flash-image" {
		t.Fatalf("model=%q, want=%q", model, "gemini-2.5-flash-image")
	}
}

func TestRewriteModelInPath(t *testing.T) {
	got, changed, err := rewriteModelInPath(
		"https://example.com/v1beta/models/gemini-2.5-flash-image:generateContent?alt=sse",
		"gemini-2.5-flash-image",
		"gemini-2.5-pro-image",
	)
	if err != nil {
		t.Fatalf("rewriteModelInPath error: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	want := "https://example.com/v1beta/models/gemini-2.5-pro-image:generateContent?alt=sse"
	if got != want {
		t.Fatalf("url=%q, want=%q", got, want)
	}
}

func TestPrepareChannelAttemptAppliesModelRewriteOnPath(t *testing.T) {
	p := &Proxy{}
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash-image:generateContent?alt=sse", nil)
	ctx := &Context{
		Request: req,
	}
	ch := &Channel{
		Name:    "gemini-third",
		BaseURL: "https://example.com",
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		ModelRewrite: []ModelRewriteRule{
			{Source: "gemini-2.5-*", Target: "gemini-2.5-pro-image"},
		},
	}

	_, _, _, err := p.prepareChannelAttempt(req, ctx, ch)
	if err != nil {
		t.Fatalf("prepareChannelAttempt error: %v", err)
	}

	wantURL := "https://example.com/v1beta/models/gemini-2.5-pro-image:generateContent?alt=sse"
	if ctx.TargetURL != wantURL {
		t.Fatalf("targetURL=%q, want=%q", ctx.TargetURL, wantURL)
	}
	if ctx.Model != "gemini-2.5-pro-image" {
		t.Fatalf("ctx.Model=%q, want=%q", ctx.Model, "gemini-2.5-pro-image")
	}
}

func TestTryChannelsResetsChannelScopedContext(t *testing.T) {
	p := &Proxy{
		cfg: Config{
			BeforeProxy: func(ctx *Context) func(*Context) (*http.Response, error) {
				ctx.RequestBody = append(ctx.RequestBody, []byte("\n")...)
				return nil
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx := &Context{
		Request:             req,
		RequestBody:         []byte(`{"model":"source"}`),
		OriginalRequestBody: []byte(`{"model":"source"}`),
	}

	firstSeen := ""
	secondSeen := ""
	channels := []Channel{
		{
			Id:      1,
			Name:    "first",
			BaseURL: "https://first.example.com",
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k1", Value: "v1"}}
			},
			ModelRewrite: []ModelRewriteRule{
				{Source: "source", Target: "first-model"},
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				firstSeen = gjson.GetBytes(ctx.RequestBody, "model").String()
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"first failed"}}`)),
				}, nil
			},
			Retry: &RetryConfig{
				MaxAttempts: 1,
				RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
					return code >= http.StatusInternalServerError
				},
			},
		},
		{
			Id:      2,
			Name:    "second",
			BaseURL: "https://second.example.com",
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k2", Value: "v2"}}
			},
			ModelRewrite: []ModelRewriteRule{
				{Source: "source", Target: "second-model"},
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				if ctx.LastStatusCode != 0 {
					t.Fatalf("LastStatusCode=%d, want 0", ctx.LastStatusCode)
				}
				if len(ctx.LastResponseBody) != 0 {
					t.Fatalf("LastResponseBody=%q, want empty", string(ctx.LastResponseBody))
				}
				secondSeen = gjson.GetBytes(ctx.RequestBody, "model").String()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				}, nil
			},
			Retry: &RetryConfig{MaxAttempts: 1},
		},
	}

	result := p.tryChannels(req, ctx, channels, NoRetry())
	if result.successResp == nil {
		t.Fatal("expected second channel to succeed")
	}
	defer result.successResp.Body.Close()

	if firstSeen != "first-model" {
		t.Fatalf("first channel model=%q, want=%q", firstSeen, "first-model")
	}
	if secondSeen != "second-model" {
		t.Fatalf("second channel model=%q, want=%q", secondSeen, "second-model")
	}
}
