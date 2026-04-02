package failover

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestStreamSSEWithTransformSplitsEvents(t *testing.T) {
	p := &Proxy{
		cfg: Config{
			TransformSSE: func(_ *Context, event SSEEvent) []SSEEvent {
				if event.Event != "content_block_delta" {
					return []SSEEvent{event}
				}
				text := gjson.Get(event.Data, "delta.text").String()
				if text == "" {
					return []SSEEvent{event}
				}
				mid := len(text) / 2
				first, _ := sjson.Set(event.Data, "delta.text", text[:mid])
				second, _ := sjson.Set(event.Data, "delta.text", text[mid:])
				return []SSEEvent{
					{Event: event.Event, Data: first, ID: event.ID},
					{Event: event.Event, Data: second, ID: event.ID},
				}
			},
		},
	}

	input := strings.Join([]string{
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello world"}}`,
		"",
	}, "\n")

	rec := httptest.NewRecorder()
	ctx := &Context{Stats: &Stats{RequestStart: time.Now()}}
	p.streamSSE(rec, strings.NewReader(input), ctx)

	out := rec.Body.String()
	if strings.Count(out, "event: content_block_delta\n") != 2 {
		t.Fatalf("expected 2 split events, got output: %s", out)
	}
	if !strings.Contains(out, `"text":"hello"`) || !strings.Contains(out, `"text":" world"`) {
		t.Fatalf("unexpected split payload: %s", out)
	}
	if ctx.Stats.FirstEventTime <= 0 {
		t.Fatalf("expected first event time recorded, got %v", ctx.Stats.FirstEventTime)
	}
}

func TestParseSSEBlockCollectsMultiDataLines(t *testing.T) {
	event := parseSSEBlock([]string{
		"event: content_block_delta",
		"data: first",
		"data: second",
		"id: abc",
	})

	if event.Event != "content_block_delta" {
		t.Fatalf("event=%q", event.Event)
	}
	if event.ID != "abc" {
		t.Fatalf("id=%q", event.ID)
	}
	if event.Data != "first\nsecond" {
		t.Fatalf("data=%q", event.Data)
	}
}

func TestStreamSSEPassthroughHandlesLargeEvent(t *testing.T) {
	p := &Proxy{}
	largeText := strings.Repeat("x", 70*1024)
	input := strings.Join([]string{
		"event: response.completed",
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"` + largeText + `"}]}]}}`,
		"",
	}, "\n")

	rec := httptest.NewRecorder()
	ctx := &Context{Stats: &Stats{RequestStart: time.Now()}}
	p.streamSSE(rec, strings.NewReader(input), ctx)

	out := rec.Body.String()
	if !strings.Contains(out, "event: response.completed\n") {
		t.Fatalf("expected response.completed event, got output length=%d", len(out))
	}
	if !strings.Contains(out, largeText[:128]) || !strings.Contains(out, largeText[len(largeText)-128:]) {
		t.Fatalf("expected large payload to survive passthrough")
	}
	if ctx.Stats.FirstEventTime <= 0 {
		t.Fatalf("expected first event time recorded, got %v", ctx.Stats.FirstEventTime)
	}
}

func TestStreamSSEWithTransformHandlesLargeEvent(t *testing.T) {
	p := &Proxy{
		cfg: Config{
			TransformSSE: func(_ *Context, event SSEEvent) []SSEEvent {
				return []SSEEvent{event}
			},
		},
	}

	largeText := strings.Repeat("y", 70*1024)
	input := strings.Join([]string{
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"` + largeText + `"}}`,
		"",
	}, "\n")

	rec := httptest.NewRecorder()
	ctx := &Context{Stats: &Stats{RequestStart: time.Now()}}
	p.streamSSE(rec, strings.NewReader(input), ctx)

	out := rec.Body.String()
	if !strings.Contains(out, "event: content_block_delta\n") {
		t.Fatalf("expected content_block_delta event, got output length=%d", len(out))
	}
	if !strings.Contains(out, largeText[:128]) || !strings.Contains(out, largeText[len(largeText)-128:]) {
		t.Fatalf("expected large payload to survive transform")
	}
	if ctx.Stats.FirstEventTime <= 0 {
		t.Fatalf("expected first event time recorded, got %v", ctx.Stats.FirstEventTime)
	}
}
