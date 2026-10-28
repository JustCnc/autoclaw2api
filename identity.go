package main

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

// conversation carries the per-turn identity the managed gateway expects. A
// fresh session id on every request looks synthetic; derive it from the client
// thread and advance the query id per request.
type conversation struct {
	SessionID string
	QueryID   string
	RequestID string
}

func newUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

func readRandom(buf []byte) (int, error) {
	return rand.Read(buf)
}

func stableSessionID(account *credential, headers http.Header, body []byte) string {
	seed := headerValue(headers, "X-Session-Id", "Session-Id", "X-Conversation-Id", "Conversation-Id", "X-Thread-Id")
	if seed == "" {
		seed = conversationSeed(body)
	}
	if seed == "" {
		seed = newUUID()
	}
	subject := ""
	if account != nil {
		subject = firstNonEmpty(account.UserID, account.DeviceID, account.Alias)
	}
	sum := sha256.Sum256([]byte(subject + "\x00" + seed))
	buf := sum[:16]
	buf[6] = (buf[6] & 0x0f) | 0x70
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf)
	return "ses-" + hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

func conversationSeed(body []byte) string {
	fields, err := decodeChatPayload(body)
	if err != nil {
		return ""
	}
	messages, ok := decodeMessages(fields)
	if !ok || len(messages) == 0 {
		return ""
	}
	var b strings.Builder
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			b.WriteString("s:")
			b.WriteString(truncateRunes(message.Text, 400))
			b.WriteString("\n")
		}
	}
	for _, message := range messages {
		if message.Role == "user" {
			b.WriteString("u:")
			b.WriteString(truncateRunes(message.Text, 400))
			break
		}
	}
	return b.String()
}

func conversationFor(account *credential, headers http.Header, body []byte) conversation {
	return conversation{
		SessionID: stableSessionID(account, headers, body),
		QueryID:   newUUID(),
		RequestID: newUUID(),
	}
}

// identityHeaders builds the base desktop identity block. The signature is the
// exact md5(appId&timestamp&appKey) the client computes.
func (a *app) identityHeaders(cfg Config, account *credential, modelRequest bool) http.Header {
	h := http.Header{}
	h.Set("X-Version", cfg.ClientVersion)
	h.Set("X-Tm", cfg.ClientPlatform)
	h.Set("X-Product", cfg.Product)
	h.Set("X-Auth-Appid", cfg.AppID)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	h.Set("X-Auth-TimeStamp", ts)
	sum := md5.Sum([]byte(cfg.AppID + "&" + ts + "&" + cfg.AppKey))
	h.Set("X-Auth-Sign", hex.EncodeToString(sum[:]))
	h.Set("X-Lang", cfg.Lang)
	if cfg.Channel != "" {
		h.Set("X-Channel", cfg.Channel)
	}
	if cfg.Enterprise || (account != nil && account.Enterprise) {
		h.Set("X-Enterprise", "1")
	}
	if account != nil && account.AccessToken != "" {
		if modelRequest {
			// The managed model gateway deletes Authorization and authenticates
			// with X-Authorization; the identity gateway uses Authorization.
			h.Set("X-Authorization", "Bearer "+account.AccessToken)
		} else {
			h.Set("Authorization", "Bearer "+account.AccessToken)
		}
	}
	return h
}

func (a *app) modelHeaders(cfg Config, account *credential, model string, conv conversation) http.Header {
	h := a.identityHeaders(cfg, account, true)
	h.Set("Accept", "application/json")
	h.Set("Accept-Language", "*")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", cfg.UserAgent)
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("X-Client-Type", "pc")
	h.Set("X-Agent-Id", "main")
	h.Set("x_trace_id", "autoclaw-desktop")
	h.Set("X-Trace-Id", conv.RequestID)
	h.Set("X-Request-Id", conv.RequestID)
	h.Set("X-Request-Model", model)
	if conv.SessionID != "" {
		h.Set("X-Session-Id", conv.SessionID)
	}
	if conv.QueryID != "" {
		h.Set("X-Query-Id", conv.QueryID)
	}
	return h
}

func (a *app) catalogHeaders(cfg Config, account *credential) http.Header {
	h := a.identityHeaders(cfg, account, false)
	h.Set("Accept", "application/json")
	h.Set("User-Agent", cfg.UserAgent)
	return h
}

func (a *app) loginHeaders(cfg Config) http.Header {
	h := a.identityHeaders(cfg, nil, false)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", cfg.UserAgent)
	return h
}

func (a *app) httpClient() *http.Client {
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	if a.client != nil {
		return a.client
	}
	cfg := a.currentConfig()
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: cfg.requestTimeout(),
	}
	a.client = &http.Client{Transport: transport}
	return a.client
}

func (a *app) resetHTTPClient() {
	a.clientMu.Lock()
	if a.client != nil {
		if transport, ok := a.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	a.client = nil
	a.clientMu.Unlock()
}

func headerValue(headers http.Header, names ...string) string {
	if headers == nil {
		return ""
	}
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func sanitizeHeaderValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isPrintableASCII(value string) bool {
	for _, r := range value {
		if r > unicode.MaxASCII || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return value != ""
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mustFields(payload []byte) map[string]json.RawMessage {
	fields, err := decodeChatPayload(payload)
	if err != nil {
		return map[string]json.RawMessage{}
	}
	return fields
}
