package failover

import (
	"net/http"
	"testing"
)

func TestPrepareStreamRequestHeadersSetsEventStreamAcceptForContextStream(t *testing.T) {
	h := http.Header{
		"Accept":          []string{"*/*"},
		"Accept-Encoding": []string{"gzip, deflate, br"},
	}

	prepareStreamRequestHeaders(h, &Context{IsStream: true})

	if got := h.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept=%q, want text/event-stream", got)
	}
	if got := h.Get("Accept-Encoding"); got != "gzip, deflate, br" {
		t.Fatalf("Accept-Encoding=%q, want original", got)
	}
}

func TestPrepareStreamRequestHeadersKeepsEventStreamAccept(t *testing.T) {
	h := http.Header{
		"Accept":          []string{"Text/Event-Stream"},
		"Accept-Encoding": []string{"gzip"},
	}

	prepareStreamRequestHeaders(h, nil)

	if got := h.Get("Accept"); got != "Text/Event-Stream" {
		t.Fatalf("Accept=%q, want original event-stream accept", got)
	}
	if got := h.Get("Accept-Encoding"); got != "gzip" {
		t.Fatalf("Accept-Encoding=%q, want original", got)
	}
}

func TestPrepareStreamRequestHeadersKeepsNonStreamCompression(t *testing.T) {
	h := http.Header{
		"Accept":          []string{"application/json"},
		"Accept-Encoding": []string{"gzip, deflate, br"},
	}

	prepareStreamRequestHeaders(h, &Context{})

	if got := h.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept=%q, want application/json", got)
	}
	if got := h.Get("Accept-Encoding"); got != "gzip, deflate, br" {
		t.Fatalf("Accept-Encoding=%q, want original", got)
	}
}
