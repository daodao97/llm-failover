package failover

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// executeSingleAttempt 执行单次渠道尝试（单 key + 单 attempt）
func (p *Proxy) executeSingleAttempt(r *http.Request, ctx *Context, ch *Channel, key *Key) (*http.Response, error) {
	if ch.Handler != nil {
		return p.executeChannelHandlerAttempt(ctx, ch, key)
	}
	return p.executeHTTPAttempt(r, ctx, ch, key)
}

func (p *Proxy) executeChannelHandlerAttempt(ctx *Context, ch *Channel, key *Key) (*http.Response, error) {
	stats := &Stats{RequestStart: time.Now()}
	ctx.Stats = stats
	acquired := false

	if ch.AcquireKey != nil {
		if ok := ch.AcquireKey(key.ID); !ok {
			return nil, errors.New("key concurrency limit reached")
		}
		acquired = true
	}
	releaseKey := newKeyReleaseFunc(ch, key, acquired)

	resp, err := ch.Handler(ctx)
	if err == nil && resp == nil {
		err = errEmptyResponseChannelHandler
	}
	if resp != nil {
		attachReleaseOnClose(resp, releaseKey)
	} else if releaseKey != nil {
		releaseKey()
	}
	stats.TotalDuration = time.Since(stats.RequestStart)

	return resp, err
}

func (p *Proxy) executeHTTPAttempt(r *http.Request, ctx *Context, ch *Channel, key *Key) (*http.Response, error) {
	acquired := false

	// 初始化统计信息
	stats := &Stats{RequestStart: time.Now()}
	ctx.Stats = stats

	// 使用可能被钩子修改后的数据创建请求
	var bodyReader io.Reader = bytes.NewReader(ctx.RequestBody)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, ctx.TargetURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// 应用目标请求头
	for k, vv := range ctx.TargetHeader {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	// 渠道静态头最高优先级，覆盖已有值
	applyChannelHeaders(req.Header, ch)

	if ch.AcquireKey != nil {
		if ok := ch.AcquireKey(key.ID); !ok {
			return nil, errors.New("key concurrency limit reached")
		}
		acquired = true
	}
	releaseKey := newKeyReleaseFunc(ch, key, acquired)

	// 设置 httptrace 追踪连接时间（DNS、TCP、TLS、TTFB）
	var dnsStart, connectStart, tlsStart time.Time
	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			stats.DNSDuration = time.Since(dnsStart)
		},
		ConnectStart: func(network, addr string) {
			connectStart = time.Now()
		},
		ConnectDone: func(network, addr string, err error) {
			if err == nil {
				stats.ConnectDuration = time.Since(connectStart)
			}
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			if err == nil {
				stats.TLSDuration = time.Since(tlsStart)
				stats.TLSVersion = state.Version
				stats.TLSCipherSuite = state.CipherSuite
			}
		},
		GotFirstResponseByte: func() {
			stats.TTFB = time.Since(stats.RequestStart)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	// 获取自定义 HTTP 客户端
	client := p.cfg.Client
	if ch.GetHttpClient != nil {
		if customClient := ch.GetHttpClient(ctx); customClient != nil {
			client = customClient
		}
	}

	// 记录详细的请求日志
	p.logger().DebugCtx(r.Context(), "proxy request",
		"method", req.Method,
		"url", req.URL.String(),
		"headers", formatHeaders(req.Header),
	)

	resp, err := client.Do(req)
	if err != nil {
		if releaseKey != nil {
			releaseKey()
		}
		if IsContextCanceledError(err) {
			p.logger().InfoCtx(r.Context(), "proxy request canceled",
				"url", req.URL.String(),
				"error", err,
			)
		} else {
			p.logger().WarnCtx(r.Context(), "proxy request failed",
				"url", req.URL.String(),
				"error", err,
			)
		}
		return nil, err
	}
	if resp == nil {
		if releaseKey != nil {
			releaseKey()
		}
		return nil, errEmptyResponseHTTPClient
	}

	attachReleaseOnClose(resp, releaseKey)

	// 记录响应日志
	p.logger().DebugCtx(r.Context(), "proxy response",
		"status", resp.StatusCode,
		"headers", formatHeaders(resp.Header),
	)

	return resp, nil
}

func newKeyReleaseFunc(ch *Channel, key *Key, acquired bool) func() {
	if ch == nil || key == nil || !acquired || ch.ReleaseKey == nil {
		return nil
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			ch.ReleaseKey(key.ID)
		})
	}
}

func attachReleaseOnClose(resp *http.Response, release func()) {
	if resp == nil || release == nil {
		return
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &releaseOnClose{
		rc:      resp.Body,
		release: release,
	}
}

type releaseOnClose struct {
	rc      io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releaseOnClose) Read(p []byte) (int, error) {
	return r.rc.Read(p)
}

func (r *releaseOnClose) Close() error {
	err := r.rc.Close()
	if r.release != nil {
		r.once.Do(r.release)
	}
	return err
}
