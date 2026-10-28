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

func TestManagementRegistrationAndConsole(t *testing.T) {
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

	reg := plugin.managementRegistration()
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "/console" {
		t.Fatalf("console resource missing: %+v", reg.Resources)
	}
	if len(reg.Routes) != len(managementRoutes) {
		t.Fatalf("routes: got %d want %d", len(reg.Routes), len(managementRoutes))
	}
	for _, route := range reg.Routes {
		if !strings.HasPrefix(route.Path, "/"+providerKey) {
			t.Fatalf("route without provider prefix: %+v", route)
		}
	}

	html := plugin.handleManagement(managementRequest{Method: "GET", Path: "/v0/resource/plugins/autoclaw2api/console"})
	if html.StatusCode != 200 || !strings.Contains(string(html.Body), "AutoClaw2API") {
		t.Fatalf("console not served: %d", html.StatusCode)
	}
	// Every list must be height-limited so long pools stay usable.
	if !strings.Contains(string(html.Body), "max-height:400px") {
		t.Fatal("console is missing the scroll container styles")
	}
}

func TestManagementPathNormalization(t *testing.T) {
	cases := map[string]string{
		"/v0/management/autoclaw2api/status":         "/status",
		"/v0/management/autoclaw2api/accounts/reset": "/accounts/reset",
		"autoclaw2api/accounts":                      "/accounts",
		"/accounts/export":                           "/accounts/export",
		"/v0/resource/plugins/autoclaw2api/console":  "/console",
	}
	for input, want := range cases {
		if got := normalizeManagementPath(input); got != want {
			t.Fatalf("normalizeManagementPath(%q)=%q want %q", input, got, want)
		}
	}
}

func TestSplitCredentialJSON(t *testing.T) {
	single := `{"access_token":"a"}`
	if got := splitCredentialJSON(single); len(got) != 1 {
		t.Fatalf("single object: got %d", len(got))
	}
	array := `[{"access_token":"a"},{"access_token":"b"}]`
	if got := splitCredentialJSON(array); len(got) != 2 {
		t.Fatalf("array: got %d", len(got))
	}
	wrapped := `{"accounts":[{"access_token":"a"},{"access_token":"b"},{"access_token":"c"}]}`
	if got := splitCredentialJSON(wrapped); len(got) != 3 {
		t.Fatalf("wrapped: got %d", len(got))
	}
	jsonl := "{\"access_token\":\"a\"}\n{\"access_token\":\"b\"}"
	if got := splitCredentialJSON(jsonl); len(got) != 2 {
		t.Fatalf("jsonl: got %d", len(got))
	}
	if got := splitCredentialJSON("not json"); len(got) != 0 {
		t.Fatalf("garbage should not parse: %d", len(got))
	}
}

func TestSanitizedConfigMasksTokens(t *testing.T) {
	cfg := defaultConfig()
	cfg.AccessToken = "eyJhbGciOiJIUzI1NiJ9.payload.signature"
	cfg.RefreshToken = "refresh-token-value-123456"
	plugin.mu.Lock()
	plugin.cfg = cfg
	plugin.mu.Unlock()

	items := plugin.sanitizedConfig()
	seen := map[string]string{}
	for _, item := range items {
		seen[item.Key] = item.Value
	}
	if strings.Contains(seen["access_token"], "payload") || !strings.Contains(seen["access_token"], "••••") {
		t.Fatalf("access token not masked: %q", seen["access_token"])
	}
	if strings.Contains(seen["refresh_token"], "value") {
		t.Fatalf("refresh token not masked: %q", seen["refresh_token"])
	}
	if seen["rate_limit_rpm"] != "30" || seen["rate_limit_burst"] != "20" {
		t.Fatalf("rate limit defaults missing: %q/%q", seen["rate_limit_rpm"], seen["rate_limit_burst"])
	}
}

func TestLoginProviderAndCallbackURL(t *testing.T) {
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.LoginCallbackPort = 18765
	store, err := newStateStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	plugin.mu.Lock()
	plugin.cfg = cfg
	plugin.state = store
	plugin.mu.Unlock()

	if got := plugin.loginProvider("google"); got != loginProviderGoogle {
		t.Fatalf("provider google: %q", got)
	}
	if got := plugin.loginProvider(""); got != loginProviderZai {
		t.Fatalf("default provider: %q", got)
	}

	url, err := plugin.loginCallbackURL(loginProviderZai)
	if err != nil {
		t.Fatalf("callback url: %v", err)
	}
	if !strings.HasSuffix(url, "/auth/callback-zai") || !strings.HasPrefix(url, "http://localhost:") {
		t.Fatalf("unexpected callback url: %q", url)
	}
	defer plugin.stopLoginCallback()

	// Unknown callback path is not served.
	rec := httptest.NewRecorder()
	plugin.serveLoginCallback(rec, httptest.NewRequest(http.MethodGet, "/auth/callback-nope?code=1&state=2", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path should 404, got %d", rec.Code)
	}
	// Missing code renders the failure page instead of exchanging anything.
	rec = httptest.NewRecorder()
	plugin.serveLoginCallback(rec, httptest.NewRequest(http.MethodGet, "/auth/callback-zai?state=abc", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "授权没有完成") {
		t.Fatalf("missing code: %d %s", rec.Code, rec.Body.String()[:80])
	}
}

func TestOAuthURLAndExchange(t *testing.T) {
	var sawCaptcha, sawExchange string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/userapi/overseasv1/zai-oauth-url":
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			sawCaptcha, _ = payload["ali_captcha_verify_param"].(string)
			if sawCaptcha == "" {
				_, _ = w.Write([]byte(`{"code":631002,"msg":"当前版本已停止服务，请前往官网下载最新版"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"oauth_url":"https://chat.z.ai/oauth/authorize?client_id=x&state=upstream-state"}}`))
		case "/userapi/overseasv1/zai-oauth-login":
			var payload map[string]string
			_ = json.Unmarshal(body, &payload)
			sawExchange = payload["code"] + "/" + payload["state"] + "/" + payload["navigate_uri"]
			_, _ = w.Write([]byte(`{"code":0,"data":{"user_id":184702,"access_token":"` + testJWT() + `","refresh_token":"refresh-1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.AuthBaseURL = server.URL
	store, err := newStateStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	plugin.mu.Lock()
	plugin.cfg = cfg
	plugin.state = store
	plugin.mu.Unlock()

	if _, err := plugin.requestOAuthURL(context.Background(), loginProviderZai, "http://localhost:18765/auth/callback-zai", ""); err == nil ||
		!strings.Contains(err.Error(), "安全验证") {
		t.Fatalf("missing captcha should map to a captcha hint, got %v", err)
	}

	account, err := plugin.exchangeOAuthCode(context.Background(), loginProviderZai, "code-1", "upstream-state", "http://localhost:18765/auth/callback-zai")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if account.AccessToken == "" || account.UserID != "184702" {
		t.Fatalf("unexpected account: %+v", account)
	}
	if sawExchange != "code-1/upstream-state/http://localhost:18765/auth/callback-zai" {
		t.Fatalf("exchange payload: %q", sawExchange)
	}

	link, err := plugin.requestOAuthURL(context.Background(), loginProviderZai, "http://localhost:18765/auth/callback-zai", "captcha-proof")
	if err != nil || !strings.HasPrefix(link, "https://chat.z.ai/oauth/authorize") {
		t.Fatalf("authorize url: %q %v", link, err)
	}
	if sawCaptcha != "captcha-proof" {
		t.Fatalf("captcha proof not forwarded: %q", sawCaptcha)
	}
}

// testJWT builds a minimal token with the claims AutoClaw uses.
func testJWT() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":184702,"device_id":"dev-1","exp":4102444800}`))
	return header + "." + claims + ".sig"
}

func TestManagementPathExactMatchWins(t *testing.T) {
	if got := normalizeManagementPath("/v0/management/autoclaw2api/login/status"); got != "/login/status" {
		t.Fatalf("login/status should not fall through to /status, got %q", got)
	}
	if got := normalizeManagementPath("/v0/management/autoclaw2api/status"); got != "/status" {
		t.Fatalf("status: %q", got)
	}
	if got := normalizeManagementPath("/v0/management/autoclaw2api/login/phone/send"); got != "/login/phone/send" {
		t.Fatalf("phone send: %q", got)
	}
}
