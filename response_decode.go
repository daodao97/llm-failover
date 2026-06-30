package failover

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

type responseDecodeInfo struct {
	Encodings []string
	Decoded   bool
}

func decodeCompressedResponse(resp *http.Response) (responseDecodeInfo, error) {
	info := responseDecodeInfo{}
	if resp == nil || resp.Body == nil {
		return info, nil
	}

	encodings := responseContentEncodings(resp.Header)
	if len(encodings) == 0 {
		return info, nil
	}
	info.Encodings = append([]string(nil), encodings...)

	body := resp.Body
	var err error
	for i := len(encodings) - 1; i >= 0; i-- {
		body, err = newResponseDecoder(body, encodings[i])
		if err != nil {
			_ = body.Close()
			return info, err
		}
	}

	resp.Body = body
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	info.Decoded = true
	return info, nil
}

func responseContentEncodings(h http.Header) []string {
	if h == nil {
		return nil
	}
	values := h.Values("Content-Encoding")
	encodings := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			encoding := strings.ToLower(strings.TrimSpace(part))
			if encoding == "" || encoding == "identity" {
				continue
			}
			encodings = append(encodings, encoding)
		}
	}
	return encodings
}

func newResponseDecoder(rc io.ReadCloser, encoding string) (io.ReadCloser, error) {
	switch encoding {
	case "gzip", "x-gzip":
		r, err := gzip.NewReader(rc)
		if err != nil {
			return rc, err
		}
		return &decodeReadCloser{Reader: r, closers: []io.Closer{r, rc}}, nil
	case "deflate":
		br := bufio.NewReader(rc)
		if header, _ := br.Peek(2); isLikelyZlibHeader(header) {
			r, err := zlib.NewReader(br)
			if err != nil {
				return rc, err
			}
			return &decodeReadCloser{Reader: r, closers: []io.Closer{r, rc}}, nil
		}
		r := flate.NewReader(br)
		return &decodeReadCloser{Reader: r, closers: []io.Closer{r, rc}}, nil
	case "br":
		return &decodeReadCloser{Reader: brotli.NewReader(rc), closers: []io.Closer{rc}}, nil
	default:
		return rc, nil
	}
}

func isLikelyZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf := int(header[0])
	flg := int(header[1])
	return cmf&0x0f == 8 && ((cmf<<8)+flg)%31 == 0
}

type decodeReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (r *decodeReadCloser) Close() error {
	var firstErr error
	for _, closer := range r.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
