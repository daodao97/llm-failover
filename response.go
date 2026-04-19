package failover

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultSSEScannerInitialBufferSize = 64 * 1024
	defaultSSEScannerMaxTokenSize      = 10 * 1024 * 1024
)

var filteredErrorResponseHeaders = map[string]struct{}{
	"Connection":          {},
	"Content-Length":      {},
	"Keep-Alive":          {},
	"Proxy-Connection":    {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"TE":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// writeResponse 写入响应到客户端，根据 Content-Type 自动选择 SSE 或普通模式
func (p *Proxy) writeResponse(w http.ResponseWriter, resp *http.Response, ctx *Context) {
	defer resp.Body.Close()

	// 先检测是否为 SSE 响应
	isSSE := isSSEResponse(resp.Header.Get("Content-Type"), ctx.TargetHeader.Get("Accept"))
	if ctx != nil {
		ctx.IsStream = isSSE
	}

	// 复制响应头
	for k, vv := range resp.Header {
		// 如果是 SSE，跳过 Content-Type，稍后设置正确的值
		if isSSE && strings.EqualFold(k, "Content-Type") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Del("Content-Length")

	// 如果是 SSE，设置正确的 Content-Type
	if isSSE {
		w.Header().Set("Content-Type", "text/event-stream")
	}

	w.WriteHeader(resp.StatusCode)

	// 根据响应类型选择流式处理方式
	if isSSE {
		p.streamSSE(w, resp.Body, ctx)
	} else {
		p.streamBody(w, resp.Body, ctx)
	}
}

// streamSSE 处理 SSE 流式响应，解析事件并调用 OnSSE 钩子
func (p *Proxy) streamSSE(w http.ResponseWriter, body io.Reader, ctx *Context) {
	if p.cfg.TransformSSE == nil {
		p.streamSSEPassthrough(w, body, ctx)
		return
	}
	p.streamSSEWithTransform(w, body, ctx)
}

func newSSEScanner(body io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, defaultSSEScannerInitialBufferSize), defaultSSEScannerMaxTokenSize)
	return scanner
}

func (p *Proxy) streamSSEPassthrough(w http.ResponseWriter, body io.Reader, ctx *Context) {
	flusher, _ := w.(http.Flusher)
	scanner := newSSEScanner(body)
	var event SSEEvent
	var firstEventSent bool

	emitObservedEvent := func() {
		if event.Event == "" && event.Data == "" {
			return
		}
		var channel *Channel
		if ctx != nil {
			channel = ctx.Channel
		}
		if !firstEventSent && ctx.Stats != nil {
			ctx.Stats.FirstEventTime = time.Since(ctx.Stats.RequestStart)
			firstEventSent = true
		}
		p.observer().OnSSEEvent(ctx, channel, &event)
		if p.cfg.OnSSE != nil {
			p.cfg.OnSSE(ctx, &event)
		}
	}

	for scanner.Scan() {
		line := scanner.Text()

		// 解析 SSE 事件格式
		switch {
		case strings.HasPrefix(line, "event:"):
			event.Event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			event.Data = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "id:"):
			event.ID = strings.TrimSpace(line[3:])
		case line == "":
			// 空行表示事件结束
			emitObservedEvent()
			event = SSEEvent{}
		}

		fmt.Fprintf(w, "%s\n", line)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emitObservedEvent()
	if err := scanner.Err(); err != nil && ctx != nil && ctx.Request != nil {
		p.logger().WarnCtx(ctx.Request.Context(), "sse passthrough scan failed", "error", err)
	}

	// 记录流完成时间
	if ctx.Stats != nil {
		ctx.Stats.StreamDuration = time.Since(ctx.Stats.RequestStart)
		ctx.Stats.TotalDuration = ctx.Stats.StreamDuration
	}
}

func (p *Proxy) streamSSEWithTransform(w http.ResponseWriter, body io.Reader, ctx *Context) {
	flusher, _ := w.(http.Flusher)
	scanner := newSSEScanner(body)
	var block []string
	var firstEventSent bool

	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	emitRawBlock := func(lines []string) {
		for i := range lines {
			fmt.Fprintf(w, "%s\n", lines[i])
		}
		fmt.Fprint(w, "\n")
		flush()
	}

	emitEvent := func(event SSEEvent) {
		if event.Event != "" {
			fmt.Fprintf(w, "event: %s\n", event.Event)
		}
		if event.ID != "" {
			fmt.Fprintf(w, "id: %s\n", event.ID)
		}
		if event.Data != "" {
			for _, line := range strings.Split(event.Data, "\n") {
				fmt.Fprintf(w, "data: %s\n", line)
			}
		}
		fmt.Fprint(w, "\n")
		flush()
	}

	flushBlock := func(lines []string) {
		if len(lines) == 0 {
			fmt.Fprint(w, "\n")
			flush()
			return
		}

		event := parseSSEBlock(lines)
		if event.Event == "" && event.Data == "" && event.ID == "" {
			emitRawBlock(lines)
			return
		}

		events := p.cfg.TransformSSE(ctx, event)
		if len(events) == 0 {
			events = []SSEEvent{event}
		}
		for i := range events {
			ev := events[i]
			var channel *Channel
			if ctx != nil {
				channel = ctx.Channel
			}
			if (ev.Event != "" || ev.Data != "") && !firstEventSent && ctx.Stats != nil {
				ctx.Stats.FirstEventTime = time.Since(ctx.Stats.RequestStart)
				firstEventSent = true
			}
			p.observer().OnSSEEvent(ctx, channel, &ev)
			if p.cfg.OnSSE != nil && (ev.Event != "" || ev.Data != "") {
				p.cfg.OnSSE(ctx, &ev)
			}
			emitEvent(ev)
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flushBlock(block)
			block = nil
			continue
		}
		block = append(block, line)
	}
	if len(block) > 0 {
		flushBlock(block)
	}
	if err := scanner.Err(); err != nil && ctx != nil && ctx.Request != nil {
		p.logger().WarnCtx(ctx.Request.Context(), "sse transform scan failed", "error", err)
	}

	if ctx.Stats != nil {
		ctx.Stats.StreamDuration = time.Since(ctx.Stats.RequestStart)
		ctx.Stats.TotalDuration = ctx.Stats.StreamDuration
	}
}

func parseSSEBlock(lines []string) SSEEvent {
	var event SSEEvent
	var dataLines []string
	for i := range lines {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "event:"):
			event.Event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "id:"):
			event.ID = strings.TrimSpace(line[3:])
		}
	}
	if len(dataLines) > 0 {
		event.Data = strings.Join(dataLines, "\n")
	}
	return event
}

// streamBody 处理普通 JSON 响应体
func (p *Proxy) streamBody(w http.ResponseWriter, body io.Reader, ctx *Context) {
	data, _ := io.ReadAll(body)

	if ctx.Stats != nil {
		ctx.Stats.TotalDuration = time.Since(ctx.Stats.RequestStart)
	}

	if p.cfg.OnBody != nil {
		p.cfg.OnBody(ctx, data)
	}

	w.Write(data)
}

// enabledChannels 返回所有启用且可用的渠道
func (p *Proxy) enabledChannels(r *http.Request) ([]Channel, error) {
	var channels []Channel
	var err error
	if p.cfg.GetChannels != nil {
		channels, err = p.cfg.GetChannels(r)
		if err != nil {
			return nil, err
		}
	} else {
		channels = p.cfg.Channels
	}

	var enabled []Channel
	var channelName []string
	var enabledName []string
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}

		channelName = append(channelName, ch.Name)
		// 检查动态可用性
		if ch.CheckAvailable != nil {
			if available, _ := ch.CheckAvailable(); !available {
				continue
			}
		}
		enabled = append(enabled, ch)
		enabledName = append(enabledName, ch.Name)
	}

	p.logger().DebugCtx(r.Context(), "enabledChannels",
		"channels", strings.Join(channelName, ","),
		"enabled", strings.Join(enabledName, ","),
	)
	return enabled, nil
}

// writeGeneratedErrorResponse 写回代理本地生成的错误响应。
func (p *Proxy) writeGeneratedErrorResponse(w http.ResponseWriter, ctx *Context, errType, message string, code int, err error) {
	message = sanitizeClientFacingErrorMessage(message)
	if prefix := errorPrefix(nil, ctx); prefix != "" {
		message = fmt.Sprintf("%s %s", prefix, message)
	}

	errResp := &ErrorResponse{
		Type:       errType,
		Message:    message,
		StatusCode: code,
		Err:        err,
	}

	// 只有在实际处理过请求（Channel 不为空）时才调用 OnError
	if p.cfg.OnError != nil && ctx.Channel != nil {
		p.cfg.OnError(ctx, errResp)
	}
	p.observer().OnFinalError(ctx, errResp)

	p.writeErrorResponse(w, errResp)
}

func (p *Proxy) marshalErrorBody(errType, message string) []byte {
	var errorBody any
	if p.cfg.ErrorBody != nil {
		errorBody = p.cfg.ErrorBody(errType, message)
	} else {
		// 默认 Claude API 错误格式
		errorBody = map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    errType,
				"message": message,
			},
		}
	}
	data, err := json.Marshal(errorBody)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "api_error",
				"message": "failed to encode error response",
			},
		})
		return append(fallback, '\n')
	}
	return append(data, '\n')
}

func (p *Proxy) writeErrorResponse(w http.ResponseWriter, errResp *ErrorResponse) {
	if errResp == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(p.marshalErrorBody("api_error", "unknown proxy error"))
		return
	}

	for k, vv := range errResp.Header {
		if shouldFilterErrorResponseHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	statusCode := errResp.StatusCode
	if statusCode <= 0 {
		statusCode = http.StatusInternalServerError
	}

	body := errResp.Body
	if len(body) == 0 {
		body = p.marshalErrorBody(errResp.Type, errResp.Message)
	}

	contentType := ""
	if errResp.Header != nil {
		contentType = errResp.Header.Get("Content-Type")
	}
	if contentType == "" {
		contentType = "application/json"
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func shouldFilterErrorResponseHeader(key string) bool {
	_, ok := filteredErrorResponseHeaders[http.CanonicalHeaderKey(key)]
	return ok
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	return src.Clone()
}
