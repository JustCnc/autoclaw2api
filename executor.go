package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type upstreamFailure struct {
	Status     int
	Code       string
	Message    string
	Retryable  bool
	Quota      bool
	RetryAfter time.Duration
}

func (e *upstreamFailure) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("upstream HTTP %d", e.Status)
}

func (a *app) execute(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	if normalizeFormat(req.Format) != formatChat {
		return upstreamError("autoclaw2api 只接受 chat-completions 请求", http.StatusBadRequest, false), nil
	}
	model, errModel := a.resolveModel(firstNonEmpty(req.Model, payloadModel(req.Payload)))
	if errModel != nil {
		return upstreamError(errModel.Error(), http.StatusNotFound, false), nil
	}
	cfg := a.currentConfig()
	body, dropped, errNorm := normalizeChatPayload(req.Payload, model, cfg.MaxTokensDefault, a.modelMaxTokens(model), false, cfg.ReasoningEffort)
	if errNorm != nil {
		return upstreamError("请求体无效："+errNorm.Error(), http.StatusBadRequest, false), nil
	}
	if len(dropped) > 0 && cfg.DebugDump {
		a.log.warn("payload", "已丢弃上游不接受的字段："+strings.Join(dropped, ","))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.requestTimeout())
	defer cancel()
	start := time.Now()
	headers, payload, accountID, errCall := a.callUpstream(ctx, cfg, model, body, req.Headers, a.preferredAccount(req.executorRequest))
	if errCall != nil {
		failure := asFailure(errCall)
		a.log.error("upstream", fmt.Sprintf("model=%s HTTP=%d %s", model, failure.Status, failure.Message))
		a.recordRequest(requestEntry{
			At:        start.UnixMilli(),
			AccountID: accountID,
			Model:     withModelPrefix(model),
			Status:    failure.Status,
			TotalMs:   elapsedMs(time.Since(start)),
			Error:     failure.Message,
		})
		return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
	}
	if !isEventStream(headers) && isErrorEnvelope(payload) {
		failure := classifyFailure(http.StatusBadGateway, payload)
		a.recordRequest(requestEntry{
			At:        start.UnixMilli(),
			AccountID: accountID,
			Model:     withModelPrefix(model),
			Status:    failure.Status,
			TotalMs:   elapsedMs(time.Since(start)),
			Error:     failure.Message,
		})
		return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
	}
	streamed := isEventStream(headers)
	if streamed {
		aggregated, failure := aggregateChatStream(payload)
		if failure != nil {
			a.recordRequest(requestEntry{
				At:        start.UnixMilli(),
				AccountID: accountID,
				Model:     withModelPrefix(model),
				Status:    failure.Status,
				TotalMs:   elapsedMs(time.Since(start)),
				Error:     failure.Message,
			})
			return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
		}
		payload = aggregated
	}
	entry := requestEntry{
		At:            start.UnixMilli(),
		AccountID:     accountID,
		Model:         withModelPrefix(model),
		UpstreamModel: modelNameOf(payload),
		Stream:        streamed,
		Status:        http.StatusOK,
		TotalMs:       elapsedMs(time.Since(start)),
	}
	entry.applyUsage(parseUsage(payload))
	a.recordRequest(entry)
	a.log.info("upstream", fmt.Sprintf("model=%s %dms bytes=%d", model, time.Since(start).Milliseconds(), len(payload)))
	return okEnvelope(executorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

func (a *app) executeStream(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	if normalizeFormat(req.Format) != formatChat {
		return upstreamError("autoclaw2api 只接受 chat-completions 请求", http.StatusBadRequest, false), nil
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	model, errModel := a.resolveModel(firstNonEmpty(req.Model, payloadModel(req.Payload)))
	if errModel != nil {
		return upstreamError(errModel.Error(), http.StatusNotFound, false), nil
	}
	cfg := a.currentConfig()
	body, dropped, errNorm := normalizeChatPayload(req.Payload, model, cfg.MaxTokensDefault, a.modelMaxTokens(model), true, cfg.ReasoningEffort)
	if errNorm != nil {
		return upstreamError("请求体无效："+errNorm.Error(), http.StatusBadRequest, false), nil
	}
	if len(dropped) > 0 && cfg.DebugDump {
		a.log.warn("payload", "已丢弃上游不接受的字段："+strings.Join(dropped, ","))
	}

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	resp, reader, accountID, errOpen := a.openStream(ctx, cfg, model, body, req.Headers, a.preferredAccount(req.executorRequest))
	if errOpen != nil {
		cancel()
		failure := asFailure(errOpen)
		a.log.error("stream", fmt.Sprintf("model=%s HTTP=%d %s", model, failure.Status, failure.Message))
		a.recordRequest(requestEntry{
			At:        start.UnixMilli(),
			AccountID: accountID,
			Model:     withModelPrefix(model),
			Stream:    true,
			Status:    failure.Status,
			TotalMs:   elapsedMs(time.Since(start)),
			Error:     failure.Message,
		})
		return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
	}
	if !isEventStream(resp.Header) {
		payload, errRead := readLimited(reader, 64<<20)
		reader.Close()
		cancel()
		if errRead != nil {
			a.recordRequest(requestEntry{
				At:        start.UnixMilli(),
				AccountID: accountID,
				Model:     withModelPrefix(model),
				Stream:    true,
				Status:    http.StatusBadGateway,
				TotalMs:   elapsedMs(time.Since(start)),
				Error:     "读取上游响应失败：" + errRead.Error(),
			})
			return upstreamError("读取上游响应失败："+errRead.Error(), http.StatusBadGateway, true), nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			failure := classifyFailure(resp.StatusCode, payload)
			a.recordRequest(requestEntry{
				At:        start.UnixMilli(),
				AccountID: accountID,
				Model:     withModelPrefix(model),
				Stream:    true,
				Status:    failure.Status,
				TotalMs:   elapsedMs(time.Since(start)),
				Error:     failure.Message,
			})
			return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
		}
		chunks := completionToChunks(payload)
		if len(chunks) == 0 {
			a.recordRequest(requestEntry{
				At:        start.UnixMilli(),
				AccountID: accountID,
				Model:     withModelPrefix(model),
				Stream:    true,
				Status:    http.StatusBadGateway,
				TotalMs:   elapsedMs(time.Since(start)),
				Error:     "上游返回了无法解析的响应体",
			})
			return upstreamError("上游返回了无法解析的响应体", http.StatusBadGateway, true), nil
		}
		entry := requestEntry{
			At:            start.UnixMilli(),
			AccountID:     accountID,
			Model:         withModelPrefix(model),
			UpstreamModel: modelNameOf(payload),
			Stream:        true,
			Status:        http.StatusOK,
			TotalMs:       elapsedMs(time.Since(start)),
		}
		entry.TTFTms = entry.TotalMs
		entry.applyUsage(parseUsage(payload))
		a.recordRequest(entry)
		go func() {
			for _, chunk := range chunks {
				if errEmit := a.emit(streamID, chunk); errEmit != nil {
					a.closeStream(streamID, errEmit.Error())
					return
				}
			}
			a.closeStream(streamID, "")
		}()
		return okEnvelope(map[string]any{
			"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
		})
	}

	idle := time.AfterFunc(cfg.requestTimeout(), cancel)
	go func() {
		defer cancel()
		defer idle.Stop()
		defer reader.Close()
		stats, errPump := a.pumpStream(streamID, model, reader, func() { idle.Reset(cfg.requestTimeout()) }, start)
		entry := requestEntry{
			At:            start.UnixMilli(),
			AccountID:     accountID,
			Model:         withModelPrefix(model),
			UpstreamModel: stats.Model,
			Stream:        true,
			Status:        http.StatusOK,
			TTFTms:        stats.TTFTms,
			TotalMs:       elapsedMs(time.Since(start)),
		}
		entry.applyUsage(stats.Usage)
		if errPump != nil {
			failure := asFailure(errPump)
			entry.Status = failure.Status
			entry.Error = failure.Message
			a.recordRequest(entry)
			a.log.error("stream", fmt.Sprintf("model=%s %dms %s", model, time.Since(start).Milliseconds(), failure.Message))
			a.closeStream(streamID, failure.Message)
			return
		}
		a.recordRequest(entry)
		a.log.info("stream", fmt.Sprintf("model=%s %dms", model, time.Since(start).Milliseconds()))
		a.closeStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func (a *app) countTokens(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	estimate := int64(len(req.Payload) / 3)
	if estimate < 1 {
		estimate = 1
	}
	return okEnvelope(map[string]any{"total_tokens": estimate})
}

// preferredAccount returns the account the host scheduled, importing the
// request's StorageJSON into the pool when needed.
func (a *app) preferredAccount(req executorRequest) *credential {
	if len(req.StorageJSON) > 0 {
		if account, err := decodeCredential(req.StorageJSON); err == nil && account.AccessToken != "" {
			if account.ID == "" {
				account.ID = accountIDForAlias(firstNonEmpty(account.Alias, req.AuthID, a.currentConfig().CredentialAlias))
			}
			if account.Alias == "" {
				account.Alias = aliasFromID(account.ID)
			}
			if current := a.poolAccount(account.ID); current == nil || current.AccessToken != account.AccessToken {
				if account.Priority == 0 {
					account.Priority = a.currentConfig().CredentialPriority
				}
				account.Source = firstNonEmpty(account.Source, "auth")
				a.upsertAccount(account)
			}
			return account
		}
	}
	if id := normalizeAuthID(req.AuthID); id != "" {
		if account := a.poolAccount(id); account != nil {
			return account
		}
		for _, account := range a.poolSnapshot() {
			if account.Alias == id || account.ID == providerKey+"-"+id {
				return account
			}
		}
	}
	return nil
}

func (a *app) callUpstream(ctx context.Context, cfg Config, model string, body []byte, clientHeaders http.Header, preferred *credential) (http.Header, []byte, string, error) {
	excluded := map[string]bool{}
	refreshed := map[string]bool{}
	var lastErr error
	preferredID := ""
	if preferred != nil {
		preferredID = preferred.ID
	}
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		account := a.selectAccount(preferredID, excluded, time.Now())
		if account == nil {
			if lastErr != nil {
				return nil, nil, "", lastErr
			}
			if failure := a.poolUnavailable(); failure != nil {
				return nil, nil, "", failure
			}
			return nil, nil, "", &upstreamFailure{Status: http.StatusServiceUnavailable, Message: "没有可用的 AutoClaw 账号"}
		}
		cred, errFresh := a.ensureAccountFresh(ctx, account)
		if errFresh != nil && cred == nil {
			excluded[account.ID] = true
			lastErr = &upstreamFailure{Status: http.StatusUnauthorized, Message: errFresh.Error()}
			continue
		}
		if okSlot, wait := a.takeRequestSlot(cred.ID); !okSlot {
			lastErr = rateLimitFailure(wait)
			excluded[account.ID] = true
			continue
		}
		conv := conversationFor(cred, clientHeaders, body)
		resp, errSend := a.send(ctx, cfg, model, body, cred, conv)
		if errSend != nil {
			lastErr = errSend
			excluded[account.ID] = true
			if attempt < cfg.MaxAttempts && sleepCtx(ctx, backoff(attempt)) {
				continue
			}
			return nil, nil, "", errSend
		}
		headers := resp.Header.Clone()
		payload, errRead := readLimited(resp.Body, 64<<20)
		resp.Body.Close()
		if errRead != nil {
			lastErr = &upstreamFailure{Status: http.StatusBadGateway, Message: "读取上游响应失败：" + errRead.Error(), Retryable: true}
			excluded[account.ID] = true
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			a.noteAccountSuccess(account.ID)
			return headers, payload, account.ID, nil
		}
		failure := classifyFailure(resp.StatusCode, payload)
		if after := retryAfter(headers); after > 0 {
			failure.RetryAfter = after
		}
		a.noteAccountFailure(account.ID)
		lastErr = failure

		switch {
		case failure.Status == http.StatusUnauthorized && !refreshed[account.ID]:
			refreshed[account.ID] = true
			if next, errRefresh := a.refreshAccount(ctx, cred); errRefresh == nil && next != nil {
				attempt--
				continue
			}
			a.markAccountDead(account.ID, failure.Message)
			excluded[account.ID] = true
		case failure.Retryable:
			a.coolDownAccount(account.ID, cooldownFor(cfg, failure), failure.Message)
			excluded[account.ID] = true
		case failure.Status == http.StatusUnauthorized:
			a.markAccountDead(account.ID, failure.Message)
			excluded[account.ID] = true
		default:
			return nil, nil, account.ID, failure
		}
	}
	if lastErr == nil {
		lastErr = &upstreamFailure{Status: http.StatusBadGateway, Message: "上游请求失败"}
	}
	return nil, nil, "", lastErr
}

func (a *app) openStream(ctx context.Context, cfg Config, model string, body []byte, clientHeaders http.Header, preferred *credential) (*http.Response, io.ReadCloser, string, error) {
	excluded := map[string]bool{}
	refreshed := map[string]bool{}
	var lastErr error
	preferredID := ""
	if preferred != nil {
		preferredID = preferred.ID
	}
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		account := a.selectAccount(preferredID, excluded, time.Now())
		if account == nil {
			if lastErr != nil {
				return nil, nil, "", lastErr
			}
			if failure := a.poolUnavailable(); failure != nil {
				return nil, nil, "", failure
			}
			return nil, nil, "", &upstreamFailure{Status: http.StatusServiceUnavailable, Message: "没有可用的 AutoClaw 账号"}
		}
		cred, errFresh := a.ensureAccountFresh(ctx, account)
		if errFresh != nil && cred == nil {
			excluded[account.ID] = true
			lastErr = &upstreamFailure{Status: http.StatusUnauthorized, Message: errFresh.Error()}
			continue
		}
		if okSlot, wait := a.takeRequestSlot(cred.ID); !okSlot {
			lastErr = rateLimitFailure(wait)
			excluded[account.ID] = true
			continue
		}
		conv := conversationFor(cred, clientHeaders, body)
		resp, errSend := a.send(ctx, cfg, model, body, cred, conv)
		if errSend != nil {
			lastErr = errSend
			excluded[account.ID] = true
			if attempt < cfg.MaxAttempts && sleepCtx(ctx, backoff(attempt)) {
				continue
			}
			return nil, nil, "", errSend
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			a.noteAccountSuccess(account.ID)
			return resp, resp.Body, account.ID, nil
		}
		payload, _ := readLimited(resp.Body, 1<<20)
		resp.Body.Close()
		failure := classifyFailure(resp.StatusCode, payload)
		if after := retryAfter(resp.Header); after > 0 {
			failure.RetryAfter = after
		}
		a.noteAccountFailure(account.ID)
		lastErr = failure
		switch {
		case failure.Status == http.StatusUnauthorized && !refreshed[account.ID]:
			refreshed[account.ID] = true
			if next, errRefresh := a.refreshAccount(ctx, cred); errRefresh == nil && next != nil {
				attempt--
				continue
			}
			a.markAccountDead(account.ID, failure.Message)
			excluded[account.ID] = true
		case failure.Retryable:
			a.coolDownAccount(account.ID, cooldownFor(cfg, failure), failure.Message)
			excluded[account.ID] = true
		case failure.Status == http.StatusUnauthorized:
			a.markAccountDead(account.ID, failure.Message)
			excluded[account.ID] = true
		default:
			return nil, nil, "", failure
		}
	}
	if lastErr == nil {
		lastErr = &upstreamFailure{Status: http.StatusBadGateway, Message: "上游请求失败"}
	}
	return nil, nil, "", lastErr
}

func (a *app) send(ctx context.Context, cfg Config, model string, body []byte, account *credential, conv conversation) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.chatURL(), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header = a.modelHeaders(cfg, account, model, conv)
	if cfg.DebugDump {
		a.log.info("upstream", fmt.Sprintf("POST %s headers=%s body=%s", req.URL, dumpHeaders(req.Header), truncateRunes(string(body), 800)))
	}
	return a.httpClient().Do(req)
}

// streamStats is what a finished stream contributes to the request log.
type streamStats struct {
	TTFTms int64
	Model  string
	Usage  *usageInfo
}

func (a *app) pumpStream(streamID, model string, body io.ReadCloser, touch func(), start time.Time) (streamStats, error) {
	reader := newSSEReader(body)
	sawFinish := false
	sawToolCalls := false
	sawContent := false
	var lastID, lastModel string
	var lastCreated int64
	stats := streamStats{}

	finish := func(reason, errMsg string) {
		if chunk := finishChunk(lastID, lastModel, lastCreated, model, reason, errMsg); chunk != nil {
			_ = a.emit(streamID, chunk)
		}
	}

	for {
		event, err := reader.next()
		if err == io.EOF {
			if !sawFinish {
				reason := "stop"
				if sawToolCalls {
					reason = "tool_calls"
				}
				finish(reason, "")
			}
			return stats, nil
		}
		if err != nil {
			if sawContent {
				reason := "stop"
				if sawToolCalls {
					reason = "tool_calls"
				}
				finish(reason, "上游流中断："+err.Error())
				return stats, nil
			}
			return stats, err
		}
		if len(event) == 0 {
			continue
		}
		if isErrorEnvelope(event) {
			return stats, classifyFailure(http.StatusBadGateway, event)
		}
		if touch != nil {
			touch()
		}
		if stats.TTFTms == 0 && len(event) > 0 && strings.Contains(string(event), "chat.completion.chunk") {
			stats.TTFTms = elapsedMs(time.Since(start))
		}
		if usage := parseUsage(event); usage != nil {
			stats.Usage = usage
		}
		var probe struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					Content          string            `json:"content"`
					ReasoningContent string            `json:"reasoning_content"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(event, &probe) == nil {
			if probe.ID != "" {
				lastID = probe.ID
			}
			if probe.Model != "" {
				lastModel = probe.Model
				stats.Model = probe.Model
			}
			if probe.Created != 0 {
				lastCreated = probe.Created
			}
			for _, choice := range probe.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					sawFinish = true
				}
				if len(choice.Delta.ToolCalls) > 0 {
					sawToolCalls = true
					sawContent = true
				}
				if choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" {
					sawContent = true
				}
			}
		}
		if err := a.emit(streamID, event); err != nil {
			return stats, err
		}
	}
}

func finishChunk(id, model string, created int64, fallbackModel, reason, errMsg string) []byte {
	if created == 0 {
		created = time.Now().Unix()
	}
	chunk := map[string]any{
		"id":      firstNonEmpty(id, "chatcmpl-"+randomHex(12)),
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   firstNonEmpty(model, fallbackModel),
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	if errMsg != "" {
		chunk["error"] = map[string]any{"type": "upstream_stream_error", "message": errMsg}
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return encoded
}

func completionToChunks(payload []byte) [][]byte {
	var completion struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Usage   any    `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &completion) != nil || len(completion.Choices) == 0 {
		return nil
	}
	if completion.Created == 0 {
		completion.Created = time.Now().Unix()
	}
	build := func(delta map[string]any, finishReason string, usage any) []byte {
		choice := map[string]any{"index": 0, "delta": delta}
		if finishReason != "" {
			choice["finish_reason"] = finishReason
		}
		chunk := map[string]any{
			"id":      completion.ID,
			"object":  "chat.completion.chunk",
			"created": completion.Created,
			"model":   completion.Model,
			"choices": []any{choice},
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return nil
		}
		return encoded
	}
	choice := completion.Choices[0]
	var chunks [][]byte
	if chunk := build(map[string]any{"role": "assistant"}, "", nil); chunk != nil {
		chunks = append(chunks, chunk)
	}
	if choice.Message.ReasoningContent != "" {
		if chunk := build(map[string]any{"reasoning_content": choice.Message.ReasoningContent}, "", nil); chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	if choice.Message.Content != "" {
		if chunk := build(map[string]any{"content": choice.Message.Content}, "", nil); chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	if len(choice.Message.ToolCalls) > 0 {
		if chunk := build(map[string]any{"tool_calls": choice.Message.ToolCalls}, "", nil); chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	finishReason := choice.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	if chunk := build(map[string]any{}, finishReason, completion.Usage); chunk != nil {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func (a *app) emit(streamID string, payload []byte) error {
	_, err := hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func (a *app) closeStream(streamID, errMsg string) {
	_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: errMsg})
}

func classifyFailure(status int, body []byte) *upstreamFailure {
	message := strings.TrimSpace(string(body))
	code := ""
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Details string          `json:"details"`
		Error   *struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			Type    string          `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		switch {
		case envelope.Error != nil && envelope.Error.Message != "":
			message = envelope.Error.Message
			code = trimJSONScalar(envelope.Error.Code)
		case envelope.Message != "":
			message = envelope.Message
			code = trimJSONScalar(envelope.Code)
			if envelope.Details != "" && envelope.Details != envelope.Message {
				message = message + "（" + envelope.Details + "）"
			}
		}
	}
	if message == "" {
		message = fmt.Sprintf("上游返回 HTTP %d", status)
	}
	message = truncateRunes(message, 500)
	failure := &upstreamFailure{Status: status, Code: code, Message: message}
	switch {
	case status == http.StatusUnauthorized:
		failure.Status = http.StatusUnauthorized
	case status == http.StatusTooManyRequests:
		failure.Status = http.StatusTooManyRequests
		failure.Retryable = true
	case status >= 500:
		failure.Status = http.StatusBadGateway
		failure.Retryable = status >= 502
	case status == http.StatusRequestTimeout || status == 499:
		failure.Status = http.StatusBadGateway
		failure.Retryable = true
	}
	if status == http.StatusNotAcceptable {
		failure.Message = "AutoClaw 网关拒绝了请求（406）：系统提示词伪装或请求头不匹配"
		failure.Retryable = false
	}
	if isRateLimitFailure(status, code, message) {
		failure.Status = http.StatusTooManyRequests
		failure.Retryable = true
		// The gateway penalty for bursting is short (seconds to tens of
		// seconds); the global account cooldown would be far too pessimistic.
		failure.RetryAfter = 20 * time.Second
	}
	if failure.Status != http.StatusUnauthorized && isQuotaMessage(message) {
		failure.Status = http.StatusPaymentRequired
		failure.Retryable = false
		failure.Quota = true
	}
	return failure
}

func isQuotaMessage(message string) bool {
	lowered := strings.ToLower(message)
	for _, marker := range []string{
		"insufficient credit", "insufficient_quota", "quota exceeded", "credit balance",
		"out of credits", "not enough credits",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	for _, marker := range []string{"余额不足", "积分不足", "额度不足", "余额已用完", "积分已用完"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func retryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	if raw := strings.TrimSpace(headers.Get("Retry-After-Ms")); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
			return clampDelay(time.Duration(ms) * time.Millisecond)
		}
	}
	if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			return clampDelay(time.Duration(seconds) * time.Second)
		}
		if when, err := http.ParseTime(raw); err == nil {
			return clampDelay(time.Until(when))
		}
	}
	return 0
}

func clampDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > 2*time.Hour {
		return 2 * time.Hour
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

func cooldownFor(cfg Config, failure *upstreamFailure) time.Duration {
	if failure != nil && failure.RetryAfter > 0 {
		return failure.RetryAfter
	}
	return cfg.cooldown()
}

func asFailure(err error) *upstreamFailure {
	if failure, ok := err.(*upstreamFailure); ok {
		return failure
	}
	return &upstreamFailure{Status: http.StatusBadGateway, Message: err.Error(), Retryable: true}
}

func trimJSONScalar(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("响应超过 %d 字节上限", limit)
	}
	return data, nil
}

func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(attempt) * 400 * time.Millisecond
	if d > 3*time.Second {
		d = 3 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func dumpHeaders(headers http.Header) string {
	parts := make([]string, 0, len(headers))
	for _, key := range sortedKeys(headers) {
		value := strings.Join(headers[key], ",")
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "X-Authorization") {
			value = "Bearer <redacted>"
		}
		parts = append(parts, key+": "+value)
	}
	return strings.Join(parts, " | ")
}

func isErrorEnvelope(payload []byte) bool {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Code    json.RawMessage `json:"code"`
		Choices json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return false
	}
	if len(envelope.Choices) > 0 && string(envelope.Choices) != "null" {
		return false
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return true
	}
	code := trimJSONScalar(envelope.Code)
	return code != "" && code != "0" && code != "200"
}
