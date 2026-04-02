package failover

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
)

// tryChannel 尝试单个渠道，支持多 Key 轮换和指数退避重试
// 重试条件: 网络错误、特定状态码(429/5xx)、SSE错误事件
func (p *Proxy) tryChannel(r *http.Request, ctx *Context, ch *Channel, cfg RetryConfig) (*http.Response, error) {
	var lastErr error

	baseHeader, keys, internalHandler, err := p.prepareChannelAttempt(r, ctx, ch)
	if err != nil {
		return nil, wrapChannelError(ch, err)
	}
	if internalHandler != nil {
		resp, err := internalHandler(ctx)
		if err == nil && resp == nil {
			return nil, wrapChannelError(ch, errEmptyResponseInternalHandler)
		}
		return resp, wrapChannelError(ch, err)
	}

	// 遍历所有 Key
	for keyIdx := range keys {
		ctx.KeyIdx = keyIdx
		ctx.CurrentKey = &keys[keyIdx]
		keyHeader := baseHeader.Clone()
		applyKeyHeader(keyHeader, keys[keyIdx].Value, ch.KeyType)

		// 每个 Key 的重试循环
		for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
			ctx.resetForAttempt(keyHeader)
			ctx.Attempt = attempt
			emitAttemptError := func(err error) {
				if p.cfg.OnAttemptError != nil {
					p.cfg.OnAttemptError(ctx, err)
				}
			}

			if err := r.Context().Err(); err != nil {
				return nil, err
			}

			// 调用 BeforeAttempt 钩子（每次尝试前调用，用于日志记录、指标采集）
			if p.cfg.BeforeAttempt != nil {
				p.cfg.BeforeAttempt(ctx)
			}

			resp, err := p.executeSingleAttempt(r, ctx, ch, &keys[keyIdx])
			if err == nil && resp == nil {
				err = errEmptyResponseTryChannelLoop
			}
			if err != nil {
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				lastErr = err
				if IsContextDoneError(err) {
					emitAttemptError(err)
					return nil, err
				}
				if p.shouldRetryOnExecutionError(r, ctx, ch, cfg, attempt, err, emitAttemptError) {
					ctx.LastResponseBody = nil
					continue
				}
				emitAttemptError(err)
				// 网络错误，尝试下一个 key
				break
			}

			if p.cfg.AfterProxy != nil {
				p.cfg.AfterProxy(ctx, resp)
			}

			decision := p.evaluateRetryDecision(ctx, cfg, resp)
			if decision.reason != "" {
				var retryReason string
				lastErr, retryReason = p.buildRetryError(ctx, resp, decision)
				if attempt < cfg.MaxAttempts {
					resp.Body.Close()
					ctx.RetryReason = retryReason
					emitAttemptError(lastErr)
					sleep(r.Context(), backoff(cfg, attempt))
					continue
				}
				emitAttemptError(lastErr)
				// 当前 key 已用尽，尝试下一个 key
				resp.Body.Close()
				break
			}

			// 非 200 状态, 都返回错误 OnChannelFail
			if resp.StatusCode >= http.StatusBadRequest {
				lastErr = wrapChannelError(ch, fmt.Errorf("status: %d", resp.StatusCode))
				ctx.LastStatusCode = resp.StatusCode
				ctx.LastResponseHeader = resp.Header.Clone()
				emitAttemptError(lastErr)
				return resp, lastErr
			}

			// 成功响应，更新状态码和响应头
			ctx.LastStatusCode = resp.StatusCode
			ctx.LastResponseHeader = resp.Header.Clone()
			return resp, nil
		}
	}

	if lastErr == nil {
		p.logger().ErrorCtx(r.Context(), "proxy tryChannel returned nil response and nil error",
			"guard_error", errEmptyResponseTryChannels.Error(),
			"channel", ch.Name,
			"channel_type", ch.CType,
			"attempt", ctx.Attempt,
			"key_id", ctx.CurrentKey.ID,
			"max_attempts", cfg.MaxAttempts,
		)
	}
	return nil, wrapChannelError(ch, lastErr)
}

// prepareChannelAttempt 准备单个渠道的请求上下文（目标 URL、基础请求头、可用 Key、BeforeProxy）
func (p *Proxy) prepareChannelAttempt(r *http.Request, ctx *Context, ch *Channel) (http.Header, []Key, func(*Context) (*http.Response, error), error) {
	ctx.TargetURL = p.buildTargetURL(r, ch)

	baseHeader := make(http.Header)
	copyHeaders(baseHeader, r.Header)
	applyChannelHeaders(baseHeader, ch)
	ctx.TargetHeader = baseHeader

	if err := applyChannelModelRewrite(ctx, ch, p.logger()); err != nil {
		return nil, nil, nil, fmt.Errorf("apply channel model rewrite failed: %w", err)
	}

	var internalHandler func(*Context) (*http.Response, error)
	if p.cfg.BeforeProxy != nil {
		internalHandler = p.cfg.BeforeProxy(ctx)
	}
	if internalHandler != nil {
		return nil, nil, internalHandler, nil
	}

	var keys []Key
	if ch.GetKeys != nil {
		keys = ch.GetKeys(ctx)
	}
	if len(keys) == 0 {
		return nil, nil, nil, p.buildNoKeysError(ctx)
	}

	return ctx.TargetHeader, keys, nil, nil
}

func applyChannelModelRewrite(ctx *Context, ch *Channel, logger Logger) error {
	if ctx == nil || ch == nil || len(ch.ModelRewrite) == 0 {
		return nil
	}

	bodyModel := strings.TrimSpace(gjson.GetBytes(ctx.RequestBody, "model").String())
	pathModel, hasPathModel := extractModelFromPath(ctx.TargetURL)

	model := bodyModel
	if model == "" {
		model = pathModel
	}
	if model == "" {
		return nil
	}

	rewrittenModel, matched := rewriteModelByRules(model, ch.ModelRewrite)
	if !matched || rewrittenModel == "" || rewrittenModel == model {
		return nil
	}

	if bodyModel != "" {
		body, err := sjson.SetBytes(ctx.RequestBody, "model", rewrittenModel)
		if err != nil {
			return err
		}
		ctx.RequestBody = body
	}

	if hasPathModel && pathModel == model {
		targetURL, changed, err := rewriteModelInPath(ctx.TargetURL, pathModel, rewrittenModel)
		if err != nil {
			return err
		}
		if changed {
			ctx.TargetURL = targetURL
		}
	}

	ctx.Model = rewrittenModel

	if ctx.Request != nil && logger != nil {
		logger.DebugCtx(ctx.Request.Context(), "channel model rewritten",
			"channel", ch.Name,
			"from", model,
			"to", rewrittenModel,
		)
	}

	return nil
}

func extractModelFromPath(targetURL string) (string, bool) {
	if targetURL == "" {
		return "", false
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return "", false
	}
	pathValue := parsed.Path
	idx := strings.Index(pathValue, "/models/")
	if idx < 0 {
		return "", false
	}

	rest := pathValue[idx+len("/models/"):]
	colonIdx := strings.LastIndex(rest, ":")
	if colonIdx <= 0 || colonIdx == len(rest)-1 {
		return "", false
	}

	model := strings.TrimSpace(rest[:colonIdx])
	if model == "" {
		return "", false
	}
	return model, true
}

func rewriteModelInPath(targetURL, currentModel, rewrittenModel string) (string, bool, error) {
	if targetURL == "" || currentModel == "" || rewrittenModel == "" || currentModel == rewrittenModel {
		return targetURL, false, nil
	}

	parsed, err := url.Parse(targetURL)
	if err != nil {
		return targetURL, false, err
	}

	pathValue := parsed.Path
	idx := strings.Index(pathValue, "/models/")
	if idx < 0 {
		return targetURL, false, nil
	}

	rest := pathValue[idx+len("/models/"):]
	colonIdx := strings.LastIndex(rest, ":")
	if colonIdx <= 0 || colonIdx == len(rest)-1 {
		return targetURL, false, nil
	}

	model := strings.TrimSpace(rest[:colonIdx])
	if model != currentModel {
		return targetURL, false, nil
	}

	action := rest[colonIdx:]
	prefix := pathValue[:idx+len("/models/")]
	parsed.Path = prefix + rewrittenModel + action
	return parsed.String(), true, nil
}

func (p *Proxy) buildNoKeysError(ctx *Context) error {
	stats := ctx.PoolStats
	if stats.TotalCandidates == 0 {
		return errors.New("no keys available: no candidates in pool")
	}
	if stats.RequestedScope != "" && stats.ScopeFiltered >= stats.TotalCandidates {
		return fmt.Errorf("no keys available: scope mismatch (scope=%s)", stats.RequestedScope)
	}
	if stats.Invalid >= stats.TotalCandidates {
		return errors.New("no keys available: all candidates invalid")
	}
	if stats.Busy >= stats.TotalCandidates {
		return errors.New("no keys available: all candidates busy")
	}
	return errors.New("no keys available")
}

// buildTargetURL 构建渠道目标 URL，并合并 query 参数（路径中的 query 优先）
func (p *Proxy) buildTargetURL(r *http.Request, ch *Channel) string {
	path := r.URL.Path
	if ch != nil && ch.ResolvePath != nil {
		path = ch.ResolvePath(path, r)
	}

	targetURL := ch.BaseURL
	pathPart := path
	var pathQuery string
	if idx := strings.Index(path, "?"); idx != -1 {
		pathPart = path[:idx]
		pathQuery = path[idx+1:]
	}
	targetURL += pathPart

	if pathQuery == "" && r.URL.RawQuery == "" {
		return targetURL
	}

	merged := make(url.Values)
	if r.URL.RawQuery != "" {
		if parsed, err := url.ParseQuery(r.URL.RawQuery); err == nil {
			maps.Copy(merged, parsed)
		}
	}
	if pathQuery != "" {
		if parsed, err := url.ParseQuery(pathQuery); err == nil {
			maps.Copy(merged, parsed)
		}
	}
	if len(merged) == 0 {
		return targetURL
	}
	return targetURL + "?" + merged.Encode()
}

func (p *Proxy) shouldRetryOnExecutionError(r *http.Request, ctx *Context, ch *Channel, cfg RetryConfig, attempt int, err error, emitAttemptError func(error)) bool {
	if cfg.RetryOnError == nil || !cfg.RetryOnError(ctx, ch, err) || attempt >= cfg.MaxAttempts {
		return false
	}
	ctx.RetryReason = fmt.Sprintf("error: %v", err)
	emitAttemptError(err)
	sleep(r.Context(), backoff(cfg, attempt))
	return true
}

// evaluateRetryDecision 判定当前响应是否需要重试（状态码/SSE）
func (p *Proxy) evaluateRetryDecision(ctx *Context, cfg RetryConfig, resp *http.Response) retryDecision {
	decision := retryDecision{}
	decision.isSSE = isSSEResponse(resp.Header.Get("Content-Type"), ctx.TargetHeader.Get("Accept"))
	ctx.IsStream = decision.isSSE
	if cfg.RetryOnResponse != nil && resp.StatusCode >= http.StatusBadRequest {
		var body []byte
		// SSE 不能在重试判定阶段整包读取，否则会破坏流式输出。
		// 仅在非 SSE 的错误响应上读取完整响应体，供业务层基于 body 判定是否重试。
		if !decision.isSSE {
			body, _ = io.ReadAll(resp.Body)
			ctx.LastResponseBody = append([]byte(nil), body...)
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
		decision.statusRetry = cfg.RetryOnResponse(ctx, ctx.Channel, resp.StatusCode, body)
		if decision.statusRetry {
			decision.reason = buildStatusRetryReason(resp.StatusCode, body)
		}
	}

	if !decision.isSSE || cfg.RetryOnSSE == nil {
		return decision
	}

	peek, _ := peekBody(resp, 512)
	decision.ssePeek = peek
	if !cfg.RetryOnSSE(isSSEError(peek)) {
		return decision
	}

	sseReason := "sse_error: empty response"
	if len(peek) > 0 {
		peekStr := string(peek)
		if len(peekStr) > 200 {
			peekStr = peekStr[:200] + "..."
		}
		peekStr = strings.ReplaceAll(peekStr, "\n", "\\n")
		sseReason = fmt.Sprintf("sse_error: %s", peekStr)
	}
	if decision.reason == "" {
		decision.reason = sseReason
	} else {
		decision.reason += ", " + sseReason
	}
	return decision
}

// buildRetryError 根据重试判定写入上下文并返回 retry 错误
func (p *Proxy) buildRetryError(ctx *Context, resp *http.Response, decision retryDecision) (error, string) {
	retryReason := decision.reason

	ctx.LastStatusCode = resp.StatusCode
	ctx.LastResponseHeader = resp.Header.Clone()
	if decision.isSSE {
		if len(decision.ssePeek) > 0 {
			ctx.LastResponseBody = append([]byte(nil), decision.ssePeek...)
		}
	} else {
		body := ctx.LastResponseBody
		if len(body) == 0 {
			body, _ = io.ReadAll(resp.Body)
			ctx.LastResponseBody = body
		}
		if decision.statusRetry {
			retryReason = buildStatusRetryReason(resp.StatusCode, body)
		}
	}

	return wrapChannelError(ctx.Channel, fmt.Errorf("retry: %s", retryReason)), retryReason
}
