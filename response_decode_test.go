package failover

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestDecodeCompressedResponse(t *testing.T) {
	const plain = "event: message\ndata: hello\n\n"

	tests := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "gzip", encoding: "gzip", body: gzipBytes(t, plain)},
		{name: "zlib deflate", encoding: "deflate", body: zlibBytes(t, plain)},
		{name: "raw deflate", encoding: "deflate", body: rawDeflateBytes(t, plain)},
		{name: "brotli", encoding: "br", body: brotliBytes(t, plain)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: http.Header{
					"Content-Encoding": []string{tt.encoding},
					"Content-Length":   []string{"123"},
				},
				Body:          io.NopCloser(bytes.NewReader(tt.body)),
				ContentLength: int64(len(tt.body)),
			}

			info, err := decodeCompressedResponse(resp)
			if err != nil {
				t.Fatalf("decodeCompressedResponse error: %v", err)
			}
			if !info.Decoded {
				t.Fatalf("Decoded=false, want true")
			}
			if len(info.Encodings) != 1 || info.Encodings[0] != tt.encoding {
				t.Fatalf("Encodings=%v, want [%s]", info.Encodings, tt.encoding)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read decoded body: %v", err)
			}
			if string(got) != plain {
				t.Fatalf("decoded body=%q, want %q", string(got), plain)
			}
			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding=%q, want empty", got)
			}
			if got := resp.Header.Get("Content-Length"); got != "" {
				t.Fatalf("Content-Length=%q, want empty", got)
			}
			if resp.ContentLength != -1 {
				t.Fatalf("ContentLength=%d, want -1", resp.ContentLength)
			}
			if !resp.Uncompressed {
				t.Fatalf("Uncompressed=false, want true")
			}
		})
	}
}

func gzipBytes(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zlibBytes(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

func rawDeflateBytes(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func brotliBytes(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}
