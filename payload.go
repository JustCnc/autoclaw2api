package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The managed gateway validates the payload strictly. Unknown top-level fields
// are dropped instead of being forwarded blindly.
var chatFieldAllowlist = map[string]bool{
	"messages":              true,
	"model":                 true,
	"stream":                true,
	"stream_options":        true,
	"max_tokens":            true,
	"max_completion_tokens": true,
	"temperature":           true,
	"top_p":                 true,
	"n":                     true,
	"stop":                  true,
	"presence_penalty":      true,
	"frequency_penalty":     true,
	"logit_bias":            true,
	"logprobs":              true,
	"top_logprobs":          true,
	"user":                  true,
	"seed":                  true,
	"response_format":       true,
	"tools":                 true,
	"tool_choice":           true,
	"parallel_tool_calls":   true,
	"service_tier":          true,
	"store":                 true,
	"metadata":              true,
	"modalities":            true,
	"audio":                 true,
	"prediction":            true,
	"reasoning_effort":      true,
	"reasoning":             true,
	"web_search_options":    true,
	"functions":             true,
	"function_call":         true,
	"thinking":              true,
	"enable_thinking":       true,
}

// chatFieldOrder mirrors the field order AutoClaw 2.2.2 puts on the wire.
var chatFieldOrder = []string{
	"model",
	"messages",
	"stream",
	"stream_options",
	"store",
	"max_completion_tokens",
	"tools",
	"tool_choice",
	"reasoning_effort",
	"thinking",
	"enable_thinking",
}

var supportedRoles = map[string]string{
	"system":    "system",
	"assistant": "assistant",
	"user":      "user",
	"tool":      "tool",
}

type chatMessage struct {
	Role       string
	Text       string
	ToolCallID string
	Name       string
	Raw        map[string]json.RawMessage
}

func decodeChatPayload(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty request body")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	return fields, nil
}

func decodeMessages(fields map[string]json.RawMessage) ([]chatMessage, bool) {
	raw, ok := fields["messages"]
	if !ok {
		return nil, false
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	out := make([]chatMessage, 0, len(items))
	for _, item := range items {
		message := chatMessage{Raw: item}
		_ = json.Unmarshal(item["role"], &message.Role)
		message.Role = strings.ToLower(strings.TrimSpace(message.Role))
		_ = json.Unmarshal(item["tool_call_id"], &message.ToolCallID)
		_ = json.Unmarshal(item["name"], &message.Name)
		message.Text = messageText(item["content"])
		out = append(out, message)
	}
	return out, true
}

func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		var kind string
		_ = json.Unmarshal(part["type"], &kind)
		if kind != "text" && kind != "input_text" {
			continue
		}
		var value string
		if json.Unmarshal(part["text"], &value) == nil && value != "" {
			b.WriteString(value)
		}
	}
	return b.String()
}

// normalizeChatPayload rewrites the downstream request into the exact shape the
// AutoClaw managed gateway accepts. The system preamble is mandatory: without it
// the gateway answers 406 with an empty body.
func normalizeChatPayload(raw []byte, model string, maxTokensDefault int, maxTokensCap int64, stream bool, reasoningEffort string) ([]byte, []string, error) {
	fields, err := decodeChatPayload(raw)
	if err != nil {
		return nil, nil, err
	}
	var dropped []string
	for key := range fields {
		if !chatFieldAllowlist[key] {
			delete(fields, key)
			dropped = append(dropped, key)
		}
	}

	if encoded, err := json.Marshal(model); err == nil {
		fields["model"] = encoded
	}

	if value, ok := fields["max_completion_tokens"]; ok {
		if _, exists := fields["max_tokens"]; !exists {
			fields["max_tokens"] = value
		}
		delete(fields, "max_completion_tokens")
	}
	limit := int64(maxTokensDefault)
	if maxTokensCap > 0 && (limit <= 0 || limit > maxTokensCap) {
		limit = maxTokensCap
	}
	if _, ok := fields["max_tokens"]; !ok && limit > 0 {
		fields["max_tokens"] = json.RawMessage(fmt.Sprintf("%d", limit))
	} else if ok && maxTokensCap > 0 {
		var requested int64
		if json.Unmarshal(fields["max_tokens"], &requested) == nil && requested > maxTokensCap {
			fields["max_tokens"] = json.RawMessage(fmt.Sprintf("%d", maxTokensCap))
		}
	}
	if value, ok := fields["max_tokens"]; ok {
		fields["max_completion_tokens"] = value
		delete(fields, "max_tokens")
	}

	if stream {
		fields["stream"] = json.RawMessage("true")
		fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		fields["stream"] = json.RawMessage("false")
		delete(fields, "stream_options")
	}
	if _, ok := fields["store"]; !ok {
		fields["store"] = json.RawMessage("false")
	}

	if reasoningEffort != "" {
		_, hasEffort := fields["reasoning_effort"]
		_, hasThinking := fields["thinking"]
		_, hasEnableThinking := fields["enable_thinking"]
		if !hasEffort && !hasThinking && !hasEnableThinking {
			if encoded, err := json.Marshal(reasoningEffort); err == nil {
				fields["reasoning_effort"] = encoded
			}
		}
	}
	if value, ok := fields["reasoning_effort"]; ok {
		fields["reasoning_effort"] = normalizeReasoningEffort(value)
	}

	if messages, ok := fields["messages"]; ok {
		normalized, changed := normalizeMessages(messages)
		if changed {
			fields["messages"] = normalized
		}
	}
	return marshalOrdered(fields, chatFieldOrder), dropped, nil
}

// normalizeMessages maps roles and guarantees the AutoClaw preamble starts the
// system prompt. Existing system text is kept after the preamble.
func normalizeMessages(raw json.RawMessage) (json.RawMessage, bool) {
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw, false
	}
	changed := false
	for _, item := range items {
		var role string
		_ = json.Unmarshal(item["role"], &role)
		role = strings.ToLower(strings.TrimSpace(role))
		mapped, ok := supportedRoles[role]
		if !ok {
			switch role {
			case "developer":
				mapped = "system"
			case "function":
				var toolCallID string
				_ = json.Unmarshal(item["tool_call_id"], &toolCallID)
				if strings.TrimSpace(toolCallID) != "" {
					mapped = "tool"
				} else {
					mapped = "user"
				}
			default:
				mapped = "user"
			}
			if encoded, err := json.Marshal(mapped); err == nil {
				item["role"] = encoded
				changed = true
			}
		}
	}

	systemIndex := -1
	for i, item := range items {
		var role string
		_ = json.Unmarshal(item["role"], &role)
		if role == "system" {
			systemIndex = i
			break
		}
	}
	if systemIndex < 0 {
		system := map[string]json.RawMessage{
			"role":    json.RawMessage(`"system"`),
			"content": mustJSON(defaultPreamble),
		}
		items = append([]map[string]json.RawMessage{system}, items...)
		changed = true
	} else {
		item := items[systemIndex]
		var content string
		if json.Unmarshal(item["content"], &content) == nil {
			if !strings.HasPrefix(strings.TrimSpace(content), defaultPreamble) {
				item["content"] = mustJSON(defaultPreamble + "\n\n" + content)
				changed = true
			}
		} else {
			var parts []json.RawMessage
			if json.Unmarshal(item["content"], &parts) == nil {
				prefix := []byte(`{"type":"text","text":` + string(mustJSON(defaultPreamble+"\n\n")) + `}`)
				parts = append([]json.RawMessage{prefix}, parts...)
				if encoded, err := json.Marshal(parts); err == nil {
					item["content"] = encoded
					changed = true
				}
			}
		}
		// Move the system message to the front, matching the desktop runtime.
		if systemIndex != 0 {
			items = append([]map[string]json.RawMessage{item}, append(items[:systemIndex], items[systemIndex+1:]...)...)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func normalizeReasoningEffort(raw json.RawMessage) json.RawMessage {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "off", "none", "disabled", "minimal":
		normalized = "low"
	case "low", "medium", "high":
	default:
		return raw
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return raw
	}
	return encoded
}

func marshalOrdered(fields map[string]json.RawMessage, order []string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	written := make(map[string]bool, len(fields))
	first := true
	write := func(key string) {
		value, ok := fields[key]
		if !ok || written[key] {
			return
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		encodedKey, _ := json.Marshal(key)
		buf.Write(encodedKey)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(value))
		written[key] = true
	}
	for _, key := range order {
		write(key)
	}
	for _, key := range sortedKeys(fields) {
		write(key)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func payloadModel(payload []byte) string {
	fields, err := decodeChatPayload(payload)
	if err != nil {
		return ""
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	return strings.TrimSpace(model)
}
