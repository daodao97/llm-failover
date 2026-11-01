package failover

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// BufferedRequestBody 保存只读请求体；Bytes 与重放 reader 共享同一份数据。
// 调用方不得原地修改传入或返回的字节。ShareRequestBody 模式会在重试结束后
// 释放共享数据；普通 Close 只关闭当前 reader，不影响其他重放 reader。
type BufferedRequestBody struct {
	buffer *requestBodyBuffer
	offset int
	closed bool
}

type requestBodyBuffer struct {
	mu   sync.Mutex
	data []byte
}

func NewBufferedRequestBody(data []byte) *BufferedRequestBody {
	return (&requestBodyBuffer{data: data}).reader()
}

func (b *requestBodyBuffer) reader() *BufferedRequestBody {
	return &BufferedRequestBody{buffer: b}
}

func (b *requestBodyBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data
}

func (b *requestBodyBuffer) release() {
	b.mu.Lock()
	b.data = nil
	b.mu.Unlock()
}

func (b *BufferedRequestBody) Bytes() []byte { return b.buffer.bytes() }

func (b *BufferedRequestBody) Read(p []byte) (int, error) {
	b.buffer.mu.Lock()
	defer b.buffer.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	if b.offset >= len(b.buffer.data) {
		return 0, io.EOF
	}
	n := copy(p, b.buffer.data[b.offset:])
	b.offset += n
	return n, nil
}

func (b *BufferedRequestBody) Close() error {
	b.buffer.mu.Lock()
	b.closed = true
	b.buffer.mu.Unlock()
	return nil
}

func setSharedRequestBody(r *http.Request, buffer *requestBodyBuffer) {
	r.Body = buffer.reader()
	r.ContentLength = int64(len(buffer.bytes()))
	r.GetBody = func() (io.ReadCloser, error) { return buffer.reader(), nil }
}

// 上传 reader 独立持有字节引用，Close 后释放。即使上游提前返回响应头，
// 重试管线释放共享正文也不会截断仍在进行的上传。
type uploadBodyReader struct {
	mu     sync.Mutex
	reader *bytes.Reader
}

func (b *requestBodyBuffer) uploadReader() (io.ReadCloser, error) {
	return &uploadBodyReader{reader: bytes.NewReader(b.bytes())}, nil
}

func (b *uploadBodyReader) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reader == nil {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(p)
}

func (b *uploadBodyReader) Close() error {
	b.mu.Lock()
	b.reader = nil
	b.mu.Unlock()
	return nil
}

func (p *Proxy) releaseRequestBody(ctx *Context) {
	if !p.cfg.ShareRequestBody || ctx.requestBodyReleased {
		return
	}
	ctx.requestBodyReleased = true
	defer func() {
		ctx.RequestBody, ctx.OriginalRequestBody = nil, nil
		if ctx.requestBodyBuffer != nil {
			ctx.requestBodyBuffer.release()
		}
	}()
	if p.cfg.OnRequestBodyRelease != nil {
		p.cfg.OnRequestBodyRelease(ctx)
	}
}
