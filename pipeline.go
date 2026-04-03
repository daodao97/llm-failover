package failover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var requestIDAnnotationPattern = regexp.MustCompile(`(?i)\s*\(request id:\s*[^)]+\)`)

// prepareRequestBody 预读请求体，用于重试场景下复用
func (p *Proxy) prepareRequestBody(w http.ResponseWriter, ctx *Context) bool {
	var bodyBytes []byte
	if ctx.Request.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(ctx.Request.Body)
		if err != nil {
			p.writeGeneratedErrorResponse(w, ctx, "invalid_request_error", "failed to read request body", http.StatusBadRequest, err)
			return false
		}
		ctx.Request.Body.Close()
	}

	resetRequestBody(ctx.Request, bodyBytes)
	ctx.RequestBody = bodyBytes
	if len(bodyBytes) > 0 {
		ctx.OriginalRequestBody = cloneBytes(bodyBytes)
	}
	return true
}

// selectChannels 阶段一：选择可用渠道
func (p *Proxy) selectChannels(w http.ResponseWriter, r *http.Request, ctx *Context) ([]Channel, bool) {
	channels, err := p.enabledChannels(cloneRequestWithBody(r, ctx.RequestBody))
	if err != nil {
		p.writeGeneratedErrorResponse(w, ctx, "api_error", err.Error(), classifySelectChannelsStatus(err), err)
		return nil, false
	}
	if len(channels) == 0 {
		p.writeGeneratedErrorResponse(w, ctx, "api_error", "no channels available", http.StatusServiceUnavailable, errors.New("没有可用渠道"))
		return nil, false
	}
	return channels, true
}

func classifySelectChannelsStatus(err error) int {
	if err == nil {
		return http.StatusBadRequest
	}

	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch msg {
	case "channelhelper not initialized", "no channels configured for token":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// tryChannels 阶段二：按顺序尝试每个渠道
func (p *Proxy) tryChannels(r *http.Request, ctx *Context, channels []Channel, retryCfg RetryConfig) pipelineResult {
	result := pipelineResult{}
	for i := range channels {
		probeMode := false
		if p.breaker != nil {
			if allowed, wait, probe := p.breaker.Allow(&channels[i]); !allowed {
				channelName := MaskChannelName(strconv.Itoa(channels[i].Id))
				result.lastErr = fmt.Errorf("channel circuit open: [%s] %s", channelName, "CC")
				p.logger().InfoCtx(r.Context(), "channel skipped by circuit breaker",
					"channel", channels[i].Name,
					"channel_type", channels[i].CType,
					"channel_idx", i,
					"retry_after", wait,
				)
				continue
			} else {
				probeMode = probe
			}
		}

		ctx.resetForChannel()
		ctx.Channel = &channels[i]
		ctx.ChannelIdx = i

		channelRetryCfg := retryCfg
		if channels[i].Retry != nil {
			channelRetryCfg = *channels[i].Retry
		}
		configuredMaxAttempts := channelRetryCfg.MaxAttempts
		if probeMode && channelRetryCfg.MaxAttempts > 1 {
			channelRetryCfg.MaxAttempts = 1
			p.logger().InfoCtx(r.Context(), "channel half-open probe uses single attempt",
				"channel", channels[i].Name,
				"channel_type", channels[i].CType,
				"channel_idx", i,
				"configured_max_attempts", configuredMaxAttempts,
			)
		}
		if channelRetryCfg.MaxAttempts <= 0 {
			channelRetryCfg.MaxAttempts = 1
		}

		resp, err := p.tryChannel(r, ctx, &channels[i], channelRetryCfg)
		if err != nil {
			result.lastErr = err
			p.handleChannelAttemptFailure(r, ctx, resp, err, &result.lastResp)
			if !p.shouldContinueToNextChannel(ctx, &channels[i], channelRetryCfg, err) {
				return result
			}
			continue
		}
		if resp == nil {
			p.logger().ErrorCtx(r.Context(), "proxy tryChannels guard hit: nil response with nil error",
				"guard_error", errEmptyResponseTryChannels.Error(),
				"channel", channels[i].Name,
				"channel_type", channels[i].CType,
				"channel_idx", i,
				"max_attempts", channelRetryCfg.MaxAttempts,
				"attempt", ctx.Attempt,
				"key_id", ctx.CurrentKey.ID,
				"last_status", ctx.LastStatusCode,
			)
			err = errEmptyResponseTryChannels
			result.lastErr = err
			p.handleChannelAttemptFailure(r, ctx, nil, err, &result.lastResp)
			continue
		}
		result.successResp = resp
		return result
	}

	return result
}

func (p *Proxy) recordCircuitFailure(r *http.Request, ctx *Context, ch *Channel, err error) bool {
	if p == nil || p.breaker == nil || ctx == nil || ch == nil || err == nil {
		return false
	}
	if IsContextDoneError(err) || !p.shouldCountChannelFailureForCircuit(ctx, ch, err) {
		return false
	}

	latency, stream := circuitObservation(ctx)
	opened, reopen, wait, reason := p.breaker.RecordFailure(ch, latency, stream, ctx.LastStatusCode)
	if !opened {
		return false
	}

	logMessage := "channel circuit opened"
	if reopen {
		logMessage = "channel circuit reopened"
	}

	requestCtx := context.Background()
	if r != nil {
		requestCtx = r.Context()
	} else if ctx.Request != nil {
		requestCtx = ctx.Request.Context()
	}

	p.logger().WarnCtx(requestCtx, logMessage,
		"channel", ch.Name,
		"channel_type", ch.CType,
		"channel_idx", ctx.ChannelIdx,
		"cooldown", wait,
		"reason", reason,
		"latency", latency,
		"error", err,
	)
	return true
}

func shouldCountChannelFailureForCircuit(ctx *Context, ch *Channel, err error) bool {
	if ctx == nil || ch == nil || err == nil {
		return false
	}
	if ch.CType != CTypePool {
		return shouldCountNonPoolFailureForCircuit(ctx, err)
	}

	msg := strings.ToLower(err.Error())
	msg = stripChannelErrorPrefix(msg)
	if strings.HasPrefix(msg, "no keys available") {
		return false
	}
	return shouldCountNonPoolFailureForCircuit(ctx, err)
}

func (p *Proxy) shouldCountChannelFailureForCircuit(ctx *Context, ch *Channel, err error) bool {
	if p != nil && p.cfg.ShouldCountFailureForCircuit != nil {
		return p.cfg.ShouldCountFailureForCircuit(ctx, ch, err)
	}
	return shouldCountChannelFailureForCircuit(ctx, ch, err)
}

func (p *Proxy) shouldContinueToNextChannel(ctx *Context, ch *Channel, cfg RetryConfig, err error) bool {
	if err == nil {
		return false
	}
	if IsContextDoneError(err) {
		return false
	}
	if IsPoolExhaustedError(err) {
		return true
	}
	if ctx != nil && ctx.LastStatusCode >= http.StatusBadRequest {
		return cfg.RetryOnResponse != nil && cfg.RetryOnResponse(ctx, ch, ctx.LastStatusCode, ctx.LastResponseBody)
	}
	if cfg.RetryOnError != nil {
		return cfg.RetryOnError(ctx, ch, err)
	}
	return IsRetryableError(err)
}

func shouldCountNonPoolFailureForCircuit(ctx *Context, err error) bool {
	if ctx == nil || err == nil {
		return false
	}
	if ctx.LastStatusCode == http.StatusTooManyRequests {
		return true
	}
	if ctx.LastStatusCode == http.StatusForbidden {
		return true
	}
	if ctx.LastStatusCode == http.StatusUnprocessableEntity {
		return true
	}
	if ctx.LastStatusCode >= http.StatusInternalServerError {
		return true
	}
	if ctx.LastStatusCode >= http.StatusBadRequest {
		return false
	}
	return IsRetryableError(err)
}

func DefaultShouldCountFailureForCircuit(ctx *Context, ch *Channel, err error) bool {
	return shouldCountChannelFailureForCircuit(ctx, ch, err)
}

// writePipelineResponse 阶段三：统一写回响应
func (p *Proxy) writePipelineResponse(w http.ResponseWriter, r *http.Request, ctx *Context, result pipelineResult) {
	if result.successResp != nil {
		if result.lastResp != nil {
			result.lastResp.Body.Close()
		}
		p.writeResponse(w, result.successResp, ctx)
		p.recordChannelSuccess(r, ctx)
		return
	}

	if result.lastErr != nil {
		if result.lastResp != nil {
			result.lastResp.Body.Close()
		}
		if p.writeUpstreamErrorResponse(w, r, ctx, result.lastErr) {
			return
		}
		p.writeGeneratedErrorResponse(w, ctx, "api_error", "all channels failed: "+result.lastErr.Error(), finalErrorStatusCode(ctx, result.lastErr), result.lastErr)
		return
	}

	if result.lastResp == nil {
		p.writeGeneratedErrorResponse(w, ctx, "api_error", "all channels failed", http.StatusBadGateway, errors.New("all channels failed"))
		return
	}
	p.writeResponse(w, result.lastResp, ctx)
}

func (p *Proxy) recordChannelSuccess(r *http.Request, ctx *Context) {
	if p == nil || p.breaker == nil || ctx == nil || ctx.Channel == nil {
		return
	}

	latency, stream := circuitObservation(ctx)
	if opened, wait, reason := p.breaker.RecordSuccess(ctx.Channel, latency, stream); opened {
		p.logger().WarnCtx(r.Context(), "channel circuit opened",
			"channel", ctx.Channel.Name,
			"channel_type", ctx.Channel.CType,
			"channel_idx", ctx.ChannelIdx,
			"cooldown", wait,
			"reason", reason,
			"latency", latency,
		)
	}
}

func circuitObservation(ctx *Context) (time.Duration, bool) {
	stream := ctx != nil && ctx.IsStream
	if ctx == nil || ctx.Stats == nil {
		return 0, stream
	}
	if stream {
		switch {
		case ctx.Stats.FirstEventTime > 0:
			return ctx.Stats.FirstEventTime, true
		case ctx.Stats.TTFB > 0:
			return ctx.Stats.TTFB, true
		case ctx.Stats.TotalDuration > 0:
			return ctx.Stats.TotalDuration, true
		case !ctx.Stats.RequestStart.IsZero():
			return time.Since(ctx.Stats.RequestStart), true
		default:
			return 0, true
		}
	}
	switch {
	case ctx.Stats.TotalDuration > 0:
		return ctx.Stats.TotalDuration, false
	case ctx.Stats.TTFB > 0:
		return ctx.Stats.TTFB, false
	case !ctx.Stats.RequestStart.IsZero():
		return time.Since(ctx.Stats.RequestStart), false
	default:
		return 0, false
	}
}

// writeUpstreamErrorResponse 写回上游渠道已返回的错误响应。
func (p *Proxy) writeUpstreamErrorResponse(w http.ResponseWriter, r *http.Request, ctx *Context, err error) bool {
	if ctx == nil || len(ctx.LastResponseBody) == 0 || ctx.LastStatusCode < http.StatusBadRequest {
		return false
	}

	errMessage := "upstream request failed"
	if err != nil {
		errMessage = err.Error()
	}
	data := append([]byte(nil), ctx.LastResponseBody...)
	contentType := "application/json"
	if ct := ctx.LastResponseHeader.Get("Content-Type"); ct != "" {
		contentType = ct
	}

	errResp := &ErrorResponse{
		Type:       "api_error",
		Message:    errMessage,
		StatusCode: ctx.LastStatusCode,
		Header:     cloneHeader(ctx.LastResponseHeader),
		Body:       data,
		Err:        err,
	}
	if !json.Valid(data) {
		message := normalizeNonJSONErrorMessage(ctx.LastStatusCode, contentType, data)
		if prefix := errorPrefix(r, ctx); prefix != "" {
			message = fmt.Sprintf("%s %s", prefix, message)
		}
		errResp.Type = "proxy_error"
		errResp.Message = message
		errResp.Body = nil
		if errResp.Header == nil {
			errResp.Header = make(http.Header)
		}
		errResp.Header.Set("Content-Type", "application/json")
	} else if ctx.LastStatusCode > http.StatusOK {
		prefix := errorPrefix(r, ctx)
		for _, path := range retryReasonMessagePaths() {
			if message := gjson.GetBytes(data, path); message.Exists() {
				prefixed := sanitizeUpstreamErrorMessage(message.String())
				if prefix != "" {
					prefixed = fmt.Sprintf("%s %s", prefix, prefixed)
				}
				data, _ = sjson.SetBytes(data, path, prefixed)
			}
		}
		if errType := gjson.GetBytes(data, "error.type"); errType.Exists() {
			data, _ = sjson.SetBytes(data, "error.type", "proxy_error")
		}
		errResp.Body = data
		if errResp.Header == nil {
			errResp.Header = make(http.Header)
		}
		errResp.Header.Set("Content-Type", contentType)
	}

	// 调用 OnError 钩子以便记录统计和覆写最终响应
	if p.cfg.OnError != nil {
		p.cfg.OnError(ctx, errResp)
	}

	p.writeErrorResponse(w, errResp)
	return true
}

func normalizeNonJSONErrorMessage(statusCode int, contentType string, data []byte) string {
	raw := sanitizeUpstreamErrorMessage(string(data))
	if raw == "" {
		if status := strings.TrimSpace(http.StatusText(statusCode)); status != "" {
			return status
		}
		return "upstream service returned non-json error response"
	}

	contentType = strings.ToLower(contentType)
	if strings.Contains(contentType, "text/html") || strings.HasPrefix(raw, "<!doctype html") || strings.HasPrefix(raw, "<html") {
		if status := strings.TrimSpace(http.StatusText(statusCode)); status != "" {
			return fmt.Sprintf("upstream service returned non-json error response: %s", status)
		}
		return "upstream service returned non-json error response"
	}

	if len(raw) > 1024 {
		raw = raw[:1024]
	}
	return raw
}

func sanitizeUpstreamErrorMessage(message string) string {
	message = sanitizeClientFacingErrorMessage(message)
	if message == "" {
		return ""
	}
	message = requestIDAnnotationPattern.ReplaceAllString(message, "")
	return strings.TrimSpace(message)
}

func errorPrefix(r *http.Request, ctx *Context) string {
	channelName := ""
	if ctx != nil && ctx.Channel != nil {
		channelName = MaskChannelName(strconv.Itoa(ctx.Channel.Id))
	}

	if r != nil {
		return formatErrorPrefix(TraceIDFromContext(r.Context()), channelName)
	}
	if ctx != nil && ctx.Request != nil {
		return formatErrorPrefix(TraceIDFromContext(ctx.Request.Context()), channelName)
	}
	return formatErrorPrefix("", channelName)
}

func formatErrorPrefix(traceID, channelName string) string {
	switch {
	case traceID != "" && channelName != "":
		return fmt.Sprintf("[%s %s]", traceID, channelName)
	case traceID != "":
		return fmt.Sprintf("[%s]", traceID)
	case channelName != "":
		return fmt.Sprintf("[%s]", channelName)
	default:
		return ""
	}
}

func (p *Proxy) handleChannelAttemptFailure(r *http.Request, ctx *Context, resp *http.Response, err error, lastResp **http.Response) {
	stats := ctx.PoolStats
	p.logger().InfoCtx(r.Context(), "channel attempt failed",
		"channel", ctx.Channel.Name,
		"attempt", ctx.Attempt,
		"requested_scope", stats.RequestedScope,
		"total_candidates", stats.TotalCandidates,
		"scope_filtered", stats.ScopeFiltered,
		"busy", stats.Busy,
		"invalid", stats.Invalid,
		"selected", stats.Selected,
		"returned", stats.Returned,
		"error", err,
	)

	if resp != nil {
		ctx.LastStatusCode = resp.StatusCode
		ctx.LastResponseHeader = resp.Header.Clone()
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ctx.LastResponseBody = body
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}

	if p.cfg.OnChannelFail != nil {
		p.logger().InfoCtx(r.Context(), "calling OnChannelFail",
			"channel", ctx.Channel.Name,
			"attempt", ctx.Attempt,
			"status", ctx.LastStatusCode,
		)
		p.cfg.OnChannelFail(ctx, err)
	} else {
		p.logger().InfoCtx(r.Context(), "OnChannelFail is nil",
			"channel", ctx.Channel.Name,
			"attempt", ctx.Attempt,
			"status", ctx.LastStatusCode,
		)
	}

	if resp != nil {
		if *lastResp != nil {
			(*lastResp).Body.Close()
		}
		*lastResp = resp
	}
}
