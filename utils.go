package failover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"strconv"
)

var clientErrorURLPattern = regexp.MustCompile(`https?://[^\s"']+`)

var filteredForwardRequestHeaders = map[string]struct{}{
	"Api-Key":             {},
	"Authorization":       {},
	"Connection":          {},
	"Content-Length":      {},
	"Cookie":              {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"X-Api-Key":           {},
}

// 辅助函数

func copyHeaders(dst, src http.Header) {
	connectionHeaders := connectionSpecificHeaders(src)
	for k, vv := range src {
		if !shouldForwardRequestHeader(k, connectionHeaders) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func shouldForwardRequestHeader(name string, connectionHeaders map[string]struct{}) bool {
	canonical := http.CanonicalHeaderKey(name)
	if _, blocked := filteredForwardRequestHeaders[canonical]; blocked {
		return false
	}
	if _, blocked := connectionHeaders[canonical]; blocked {
		return false
	}
	return true
}

func connectionSpecificHeaders(src http.Header) map[string]struct{} {
	if len(src) == 0 {
		return nil
	}

	headers := make(map[string]struct{})
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			headers[http.CanonicalHeaderKey(token)] = struct{}{}
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// applyChannelHeaders 应用渠道的静态头
func applyChannelHeaders(h http.Header, ch *Channel) {
	for k, vv := range ch.Headers {
		for _, v := range vv {
			h.Set(k, v)
		}
	}
}

// applyKeyHeader 根据 KeyType 设置认证头
func applyKeyHeader(h http.Header, key, keyType string) {
	if key == "" {
		return
	}
	if keyType == "oauth" {
		h.Set("Authorization", "Bearer "+key)
	} else {
		h.Set("x-api-key", key)
	}
}

// isSSEResponse 检测响应是否为 SSE 流式响应
// 检测规则：
// 1. Content-Type 包含 text/event-stream
// 2. Content-Type 为空或 text/plain，且客户端 Accept 请求了 text/event-stream（兼容某些 API 不返回 Content-Type 的情况）
func isSSEResponse(respContentType, reqAccept string) bool {
	return strings.Contains(respContentType, "text/event-stream") ||
		((respContentType == "" || strings.HasPrefix(respContentType, "text/plain")) && strings.Contains(reqAccept, "text/event-stream"))
}

// isSSEError 判断 SSE 响应是否为错误（空响应或 error 事件）
func isSSEError(peek []byte) bool {
	return len(peek) == 0 || bytes.Contains(peek, []byte("event: error"))
}

type readerWithCloser struct {
	io.Reader
	io.Closer
}

func resetRequestBody(r *http.Request, body []byte) {
	if r == nil {
		return
	}

	if len(body) == 0 {
		r.Body = http.NoBody
		r.GetBody = func() (io.ReadCloser, error) {
			return http.NoBody, nil
		}
		r.ContentLength = 0
		return
	}

	bodyCopy := cloneBytes(body)
	r.Body = io.NopCloser(bytes.NewReader(bodyCopy))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyCopy)), nil
	}
	r.ContentLength = int64(len(bodyCopy))
}

func cloneRequestWithBody(r *http.Request, body []byte) *http.Request {
	if r == nil {
		return nil
	}
	cloned := r.Clone(r.Context())
	resetRequestBody(cloned, body)
	return cloned
}

// peekBody 预读响应体前 n 字节用于判断重试，同时保持 Body 可继续读取
func peekBody(resp *http.Response, n int) ([]byte, error) {
	buf := make([]byte, n)
	read, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		return nil, err
	}
	orig := resp.Body
	resp.Body = &readerWithCloser{
		Reader: io.MultiReader(bytes.NewReader(buf[:read]), orig),
		Closer: orig,
	}
	return buf[:read], nil
}

// backoff 计算指数退避延迟时间
func backoff(cfg RetryConfig, attempt int) time.Duration {
	if cfg.BaseDelay <= 0 {
		return 0
	}
	delay := cfg.BaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if cfg.MaxDelay > 0 && delay > cfg.MaxDelay {
			return cfg.MaxDelay
		}
	}
	return delay
}

// sleep 可取消的延迟等待
func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// IsContextCanceledError 判断错误链中是否包含 context canceled。
func IsContextCanceledError(err error) bool {
	return errors.Is(err, context.Canceled)
}

// IsContextDeadlineExceededError 判断错误链中是否包含 context deadline exceeded。
func IsContextDeadlineExceededError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// IsContextDoneError 判断错误链中是否包含 context 取消或超时。
func IsContextDoneError(err error) bool {
	return IsContextCanceledError(err) || IsContextDeadlineExceededError(err)
}

// IsRetryableError 判断错误是否应触发重试（context 取消/超时不重试）
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if IsContextDoneError(err) {
		return false
	}
	if IsPoolExhaustedError(err) {
		return false
	}
	return true
}

// IsPoolExhaustedError 判断错误是否表示账号池当前没有可用 key。
func IsPoolExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	msg = stripChannelErrorPrefix(msg)
	return strings.HasPrefix(msg, "no keys available") || strings.Contains(msg, "key concurrency limit reached")
}

func finalErrorStatusCode(ctx *Context, err error) int {
	if ctx != nil && ctx.LastStatusCode >= http.StatusBadRequest {
		return ctx.LastStatusCode
	}
	if IsContextDeadlineExceededError(err) {
		return http.StatusGatewayTimeout
	}
	if IsContextCanceledError(err) {
		return 499
	}
	if te, ok := err.(interface{ Timeout() bool }); ok && te.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func retryReasonMessagePaths() []string {
	return []string{"message", "error.message", "detail"}
}

func extractRetryMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	for _, path := range retryReasonMessagePaths() {
		msg := strings.TrimSpace(gjson.GetBytes(body, path).String())
		if msg != "" {
			return msg
		}
	}
	// 回退到原始响应体文本（兼容非 JSON/非标准错误结构）
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return ""
	}
	raw = strings.ReplaceAll(raw, "\n", " ")
	raw = strings.Join(strings.Fields(raw), " ")
	if len(raw) > 200 {
		raw = raw[:200] + "..."
	}
	return raw
}

func buildStatusRetryReason(statusCode int, body []byte) string {
	if msg := extractRetryMessage(body); msg != "" {
		return fmt.Sprintf("message: %s", sanitizeClientFacingErrorMessage(msg))
	}
	return fmt.Sprintf("status: %d", statusCode)
}

func sanitizeClientFacingErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	message = clientErrorURLPattern.ReplaceAllString(message, "[upstream-url]")
	message = strings.ReplaceAll(message, "\n", " ")
	message = strings.Join(strings.Fields(message), " ")
	return strings.TrimSpace(message)
}

func wrapChannelError(ch *Channel, err error) error {
	if ch == nil || err == nil {
		return err
	}

	channelName := MaskChannelName(strconv.Itoa(ch.Id))
	if channelName == "" {
		return err
	}

	prefix := fmt.Sprintf("[%s]", channelName)
	if strings.HasPrefix(err.Error(), prefix) {
		return err
	}
	return fmt.Errorf("%s %w", prefix, err)
}

func stripChannelErrorPrefix(message string) string {
	message = strings.TrimSpace(message)
	if !strings.HasPrefix(message, "[") {
		return message
	}

	end := strings.Index(message, "] ")
	if end <= 0 {
		return message
	}
	return strings.TrimSpace(message[end+2:])
}

// formatHeaders 格式化 HTTP 头用于日志输出
// 隐藏敏感信息（Authorization 等）
func formatHeaders(h http.Header) map[string]string {
	result := make(map[string]string)
	for k, vv := range h {
		if len(vv) == 0 {
			continue
		}
		// 隐藏敏感头的值
		lowerKey := strings.ToLower(k)
		if lowerKey == "authorization" || lowerKey == "x-api-key" || lowerKey == "cookie" {
			result[k] = "[REDACTED]"
		} else {
			result[k] = strings.Join(vv, ", ")
		}
	}
	return result
}
