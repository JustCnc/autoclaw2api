package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeInjectsPreamble(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	out, _, err := normalizeChatPayload(body, "zai_glm-5.3-flash", 1000, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 2 || decoded.Messages[0].Role != "system" {
		t.Fatalf("expected inserted system message, got %+v", decoded.Messages)
	}
	if !strings.HasPrefix(decoded.Messages[0].Content, defaultPreamble) {
		t.Fatalf("preamble missing: %q", decoded.Messages[0].Content)
	}
}

func TestNormalizeKeepsCustomSystemAfterPreamble(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"system","content":"Always answer in French."},{"role":"user","content":"hi"}],"stream":false}`)
	out, _, err := normalizeChatPayload(body, "zai_glm-5.3-flash", 1000, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 2 {
		t.Fatalf("unexpected messages: %+v", decoded.Messages)
	}
	content := decoded.Messages[0].Content
	if !strings.HasPrefix(content, defaultPreamble) || !strings.Contains(content, "Always answer in French.") {
		t.Fatalf("system prompt merged incorrectly: %q", content)
	}
}

func TestNormalizeTokenFields(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":5,"stream":false}`)
	out, _, err := normalizeChatPayload(body, "zai_glm-5.3-flash", 1000, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["max_tokens"]; ok {
		t.Fatal("max_tokens should be renamed")
	}
	if string(fields["max_completion_tokens"]) != "5" {
		t.Fatalf("max_completion_tokens=%s", fields["max_completion_tokens"])
	}
	if string(fields["store"]) != "false" {
		t.Fatalf("store=%s", fields["store"])
	}
}

func TestParseCatalog(t *testing.T) {
	raw := []byte(`{"models":[{"id":"zai_auto","name":"Auto","reasoning":true,"input":["image","text"],"contextWindow":1048576,"maxTokens":131072}]}`)
	snapshot, err := parseCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 1 || snapshot.Default != "zai_auto" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestLoginPageAndPoll(t *testing.T) {
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.LoginPort = 18636
	cfg.DebugDump = false
	store, err := newStateStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	plugin.mu.Lock()
	plugin.cfg = cfg
	plugin.state = store
	plugin.mu.Unlock()
	plugin.startLoginServer()
	defer plugin.stopLoginServer()

	raw, err := plugin.startLogin([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("start login envelope: %s", raw)
	}
	var started authLoginStartResponse
	if err := json.Unmarshal(env.Result, &started); err != nil {
		t.Fatal(err)
	}
	if started.URL == "" || started.State == "" {
		t.Fatalf("start login response: %+v", started)
	}

	resp, err := http.Get(started.URL)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "AutoClaw 授权登录") {
		t.Fatalf("login page status=%d body=%q", resp.StatusCode, string(page[:min(len(page), 120)]))
	}

	pollRaw, err := plugin.pollLogin([]byte(`{"State":"` + started.State + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pollEnv envelope
	if err := json.Unmarshal(pollRaw, &pollEnv); err != nil || !pollEnv.OK {
		t.Fatalf("poll envelope: %s", pollRaw)
	}
	var polled authLoginPollResponse
	if err := json.Unmarshal(pollEnv.Result, &polled); err != nil {
		t.Fatal(err)
	}
	if polled.Status != "pending" {
		t.Fatalf("poll status=%q", polled.Status)
	}

	bad, err := http.Post("http://127.0.0.1:18636/api/send-code", "application/json", strings.NewReader(`{"state":"missing","phone":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state status=%d", bad.StatusCode)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestRefreshParsesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"new-access","refresh_token":"new-refresh"}}`))
	}))
	defer srv.Close()
	cfg := defaultConfig()
	cfg.AuthBaseURL = srv.URL
	cfg.DataDir = t.TempDir()
	store, err := newStateStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	plugin.mu.Lock()
	plugin.cfg = cfg
	plugin.state = store
	plugin.client = nil
	plugin.mu.Unlock()
	next, code, err := plugin.requestTokenRefresh(context.Background(), cfg, "/userapi/v1/refresh", &credential{RefreshToken: "r", DeviceID: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if code != "0" || next.AccessToken != "new-access" || next.RefreshToken != "new-refresh" {
		t.Fatalf("unexpected refresh: code=%q next=%+v", code, next)
	}
}

func TestParseAuthAccountJSON(t *testing.T) {
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	store, err := newStateStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	plugin.mu.Lock()
	plugin.cfg = cfg
	plugin.state = store
	plugin.mu.Unlock()
	inner := `{"type":"autoclaw2api","access_token":"eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3OTMxODk0NDAsInVzZXJfaWQiOjF9.x","refresh_token":"r"}`
	raw := []byte(`{"Provider":"autoclaw2api","FileName":"manual.json","RawJSON":"` + base64.StdEncoding.EncodeToString([]byte(inner)) + `"}`)
	out, err := plugin.parseAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s", out)
	}
	var parsed authParseResponse
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		t.Fatal(err)
	}
	if !parsed.Handled || parsed.Auth.Provider != providerKey {
		t.Fatalf("unexpected parse response: %+v", parsed)
	}
}

func TestTokenBucketBurstAndRefill(t *testing.T) {
	bucket := &tokenBucket{}
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if ok, _ := bucket.take(0.5, 3, now); !ok {
			t.Fatalf("burst request %d should pass", i)
		}
	}
	ok, wait := bucket.take(0.5, 3, now)
	if ok {
		t.Fatal("bucket should be empty after the burst")
	}
	if wait < 1500*time.Millisecond || wait > 2500*time.Millisecond {
		t.Fatalf("expected ~2s wait at 0.5/s, got %s", wait)
	}
	if ok, _ := bucket.take(0.5, 3, now.Add(2*time.Second)); !ok {
		t.Fatal("one token should have refilled after 2s")
	}
}

func TestRateLimitClassification(t *testing.T) {
	cases := []struct {
		status int
		code   string
		msg    string
		want   bool
	}{
		{http.StatusForbidden, "810002", "请求过于频繁，请稍后再试", true},
		{http.StatusForbidden, "", "请求过于频繁，请稍后再试", true},
		{http.StatusTooManyRequests, "", "slow down", true},
		{http.StatusForbidden, "", "invalid system prompt", false},
		{http.StatusBadRequest, "", "too many requests", false},
	}
	for _, tc := range cases {
		if got := isRateLimitFailure(tc.status, tc.code, tc.msg); got != tc.want {
			t.Fatalf("isRateLimitFailure(%d,%q,%q)=%v want %v", tc.status, tc.code, tc.msg, got, tc.want)
		}
	}
	if failure := classifyFailure(http.StatusForbidden, []byte(`{"code":810002,"message":"请求过于频繁，请稍后再试"}`)); failure.Status != http.StatusTooManyRequests || !failure.Retryable {
		t.Fatalf("rate limit should map to a retryable 429, got %+v", failure)
	}
}

func TestConfigRateLimitDefaults(t *testing.T) {
	cfg := defaultConfig()
	rpm, burst := cfg.rateLimit()
	if rpm != 30 || burst != 20 {
		t.Fatalf("unexpected defaults rpm=%v burst=%d", rpm, burst)
	}
	cfg.RateLimitRPM = 0
	if rpm, _ := cfg.rateLimit(); rpm != 0 {
		t.Fatal("rpm=0 should disable the local limiter")
	}
	cfg.RateLimitRPM = 60
	cfg.RateLimitBurst = 0
	if _, burst := cfg.rateLimit(); burst != 30 {
		t.Fatalf("burst should fall back to rpm/2, got %d", burst)
	}
}

func TestRateLimitKeepsCooldownHint(t *testing.T) {
	failure := classifyFailure(http.StatusForbidden, []byte(`{"code":810002,"message":"请求过于频繁，请稍后再试"}`))
	if failure.RetryAfter != 20*time.Second {
		t.Fatalf("expected a 20s retry hint, got %s", failure.RetryAfter)
	}
	// A missing Retry-After header must not erase the classification hint.
	if after := retryAfter(http.Header{}); after != 0 {
		t.Fatalf("empty header should not produce a hint, got %s", after)
	}
	if cooldown := cooldownFor(defaultConfig(), failure); cooldown != 20*time.Second {
		t.Fatalf("cooldown should follow the 20s hint, got %s", cooldown)
	}
}
