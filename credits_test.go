package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTokenUsage(t *testing.T) {
	in, out, cache := parseTokenUsage("input_tokens:317, output_tokens:35, cache_tokens:12")
	if in != 317 || out != 35 || cache != 12 {
		t.Fatalf("unexpected: %d %d %d", in, out, cache)
	}
	if in, out, cache = parseTokenUsage(""); in != 0 || out != 0 || cache != 0 {
		t.Fatalf("empty should be zero: %d %d %d", in, out, cache)
	}
}

func TestCreditsFetchUsesBearerToken(t *testing.T) {
	var auth, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"total_balance":14521,"wallets":[` +
			`{"public_wallet_type":"reward","display_name":"Reward Points","balance":14521,"display":true,"priority":10},` +
			`{"public_wallet_type":"daily","display_name":"Daily Points","balance":0,"display":false,"priority":20}]}}`))
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
	plugin.resetHTTPClient()

	snapshot, err := plugin.fetchCredits(context.Background(), &credential{AccessToken: "tok-1"})
	if err != nil {
		t.Fatalf("fetch credits: %v", err)
	}
	if !strings.HasPrefix(auth, "Bearer tok-1") {
		t.Fatalf("asset manager needs a plain bearer token, got %q", auth)
	}
	if path != "/agent-assetmgr/api/v2/wallets" {
		t.Fatalf("unexpected path %q", path)
	}
	if snapshot.Total != 14521 || len(snapshot.Wallets) != 2 || snapshot.Wallets[0].Type != "reward" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestLedgerFetchParsesUsageAndAmount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"entries":[{"id":"400248","occurred_at":"2026-10-28T11:27:22.233+08:00",` +
			`"mutation_type":"debit","amount":-1,"wallet_scope":"campaign","description":"AutoClaw GLM-5.3-Flash model usage",` +
			`"metadata":{"model":"glm-5.3-flash","reason":"model_usage","request_id":"1d14","token_usage":"input_tokens:317, output_tokens:35, cache_tokens:0","public_wallet_type":"reward"}}]}}`))
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
	plugin.resetHTTPClient()

	entries, err := plugin.fetchLedgers(context.Background(), &credential{AccessToken: "tok-1"}, 5)
	if err != nil {
		t.Fatalf("fetch ledgers: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Amount != -1 || entry.TokensIn != 317 || entry.TokensOut != 35 || entry.Model != "glm-5.3-flash" || entry.Wallet != "reward" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestRequestLogSummaryAndPoints(t *testing.T) {
	log := newRequestLog(10)
	seq := log.add(requestEntry{Status: 200, Stream: true, TTFTms: 100, TotalMs: 900, PromptTokens: 100, CompletionTokens: 50, CachedTokens: 25})
	log.add(requestEntry{Status: 502, TotalMs: 100, Error: "boom"})
	log.setPoints(seq, -3)

	entries := log.tail(0)
	if len(entries) != 2 || entries[0].Status != 502 {
		t.Fatalf("newest first expected, got %+v", entries)
	}
	summary := log.summary()
	if summary["total"] != 2 || summary["failures"] != 1 || summary["streams"] != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary["points"] != int64(-3) {
		t.Fatalf("points should sum to -3, got %v", summary["points"])
	}
	if entries[1].Points == nil || *entries[1].Points != -3 {
		t.Fatalf("points not attached: %+v", entries[1])
	}
	if rate, _ := summary["cache_rate"].(float64); rate != 0.25 {
		t.Fatalf("cache rate: %v", rate)
	}
}

func TestClaimBillMatchesTokensAndTime(t *testing.T) {
	a := &app{}
	now := int64(1793163150000)
	item := billWatch{accountID: "acc", seq: 1, model: "zai_glm-5.3-flash", prompt: 316, completion: 60, at: now}

	entries := []creditLedger{
		{ID: "a", At: "2026-10-28T12:52:31.076+08:00", Model: "glm-5.3-flash", Amount: -1, TokensIn: 999, TokensOut: 60},
		{ID: "b", At: "2026-10-28T12:52:31.076+08:00", Model: "glm-5.3-flash", Amount: -2, TokensIn: 316, TokensOut: 60},
	}
	points, ok := a.claimBill(item, entries)
	if !ok || points != -2 {
		t.Fatalf("expected -2 from the token-matching row, got %d ok=%v", points, ok)
	}
	if points, ok = a.claimBill(item, entries); ok {
		t.Fatalf("a row must only be claimed once, got %d", points)
	}
}

func TestManagementPathsForCreditsRoutes(t *testing.T) {
	cases := map[string]string{
		"/v0/management/autoclaw2api/credits":         "/credits",
		"/v0/management/autoclaw2api/credits/ledgers": "/credits/ledgers",
		"/v0/management/autoclaw2api/credits/refresh": "/credits/refresh",
		"/v0/management/autoclaw2api/requests":        "/requests",
		"/v0/management/autoclaw2api/login/status":    "/login/status",
		"/v0/management/autoclaw2api/status":          "/status",
	}
	for input, want := range cases {
		if got := normalizeManagementPath(input); got != want {
			t.Fatalf("%s: want %s, got %s", input, want, got)
		}
	}
}

func TestCreditsCacheTTLAndFailureBackoff(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			_, _ = w.Write([]byte(`{"code":410000,"msg":"Please log in to continue."}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":100,"wallets":[]}}`))
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
	plugin.credits = nil
	plugin.mu.Unlock()
	plugin.resetHTTPClient()
	account := &credential{ID: "acc-ttl", AccessToken: "tok"}

	first := plugin.creditsFor(context.Background(), account, false)
	if first.Total != 100 || first.Error != "" {
		t.Fatalf("first fetch: %+v", first)
	}
	// Cached: no new upstream call.
	if again := plugin.creditsFor(context.Background(), account, false); again.Total != 100 || calls != 1 {
		t.Fatalf("cache hit expected, calls=%d snapshot=%+v", calls, again)
	}
	// refresh bypasses the cache and the failure keeps the last good total.
	failed := plugin.creditsFor(context.Background(), account, true)
	if failed.Error == "" || failed.Total != 100 {
		t.Fatalf("failure should keep the cached total: %+v", failed)
	}
	if calls != 2 {
		t.Fatalf("refresh should hit upstream, calls=%d", calls)
	}
	// A failed entry is retried after ~10s instead of being trusted for the TTL.
	plugin.creditMu.Lock()
	entry := plugin.credits["acc-ttl"]
	entry.at = entry.at.Add(-11 * time.Second)
	plugin.credits["acc-ttl"] = entry
	plugin.creditMu.Unlock()
	plugin.creditsFor(context.Background(), account, false)
	if calls != 3 {
		t.Fatalf("failed cache should expire fast, calls=%d", calls)
	}
}

func TestClaimBillRejectsWrongModelOrWindow(t *testing.T) {
	a := &app{}
	item := billWatch{accountID: "acc", seq: 1, model: "zai_glm-5.3-flash", prompt: 10, completion: 5, at: 1793163150000, totalMs: 1000}

	if _, ok := a.claimBill(item, []creditLedger{{ID: "x", At: "2026-10-28T12:52:31.076+08:00", Model: "glm-5.3", Amount: -1, TokensIn: 10, TokensOut: 5}}); ok {
		t.Fatal("a different model must not be claimed")
	}
	if _, ok := a.claimBill(item, []creditLedger{{ID: "y", At: "2026-10-28T20:52:31.076+08:00", Model: "glm-5.3-flash", Amount: -1, TokensIn: 10, TokensOut: 5}}); ok {
		t.Fatal("a row outside the window must not be claimed")
	}
	if _, ok := a.claimBill(item, []creditLedger{{ID: "z", At: "not-a-time", Model: "glm-5.3-flash", Amount: -1, TokensIn: 10, TokensOut: 5}}); ok {
		t.Fatal("an unparseable timestamp must not be claimed")
	}
	points, ok := a.claimBill(item, []creditLedger{{ID: "ok", At: "2026-10-28T12:52:31.076+08:00", Model: "glm-5.3-flash", Amount: -7, TokensIn: 10, TokensOut: 5}})
	if !ok || points != -7 {
		t.Fatalf("matching row should be claimed: %d ok=%v", points, ok)
	}
}

func TestRewardTaskParsingAndClaimability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/autoclaw-proxy/proxy/autoclaw-task-list":
			_, _ = w.Write([]byte(`{"data":[
				{"task_id":"daily_signin","title":"每日签到","reward_points":400,"task_type":"daily","status":"incomplete","display":true,"client_triggered":true},
				{"task_id":"invite_friends","title":"邀请好友","reward_points":200,"task_type":"count","status":"incomplete","display":true,"client_triggered":false},
				{"task_id":"daily_inspiration_center","title":"进入灵感中心","reward_points":200,"task_type":"daily","status":"complete","display":false,"client_triggered":true}]}`))
		case r.URL.Path == "/autoclaw-proxy/proxy/autoclaw-task-complete":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["task_id"] != "daily_signin" {
				t.Fatalf("unexpected task %q", body["task_id"])
			}
			_, _ = w.Write([]byte(`{"data":{"success":true,"already_completed":false,"reward_points":400,"task_id":"daily_signin"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
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
	plugin.accounts = map[string]*credential{}
	plugin.mu.Unlock()
	plugin.resetHTTPClient()

	account := &credential{ID: "acc-reward", Alias: "reward", AccessToken: "tok", DeviceID: "dev-1"}
	tasks, err := plugin.fetchTasks(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(tasks))
	}
	if !tasks[2].Done {
		t.Fatal("status=complete should map to done")
	}
	if !claimableTask(tasks[0]) {
		t.Fatal("daily task with reward should be claimable")
	}
	if claimableTask(tasks[1]) {
		t.Fatal("invite task must not be claimed automatically")
	}
	if claimableTask(tasks[2]) {
		t.Fatal("completed task must not be claimed")
	}

	claim, err := plugin.completeTask(context.Background(), account, "daily_signin")
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Claimed || claim.Points != 400 {
		t.Fatalf("claim = %+v", claim)
	}
}

func TestCampaignEntryMatchesLoginReward(t *testing.T) {
	if !campaignEntry(creditLedger{EventType: "autoclaw_user_active"}) {
		t.Fatal("event_type should match")
	}
	if !campaignEntry(creditLedger{EventType: "autoclaw_user_signup_day_3"}) {
		t.Fatal("signup day event should match")
	}
	if campaignEntry(creditLedger{EventType: "autoclaw_user_signup"}) {
		t.Fatal("only day-suffixed signup events match")
	}
	if campaignEntry(creditLedger{Description: "每日签到"}) {
		t.Fatal("a sign-in row is not a login reward")
	}
	if !campaignEntry(creditLedger{Description: "AutoClaw Login Reward +4000 credits autoclaw_user_active_reward_202609"}) {
		t.Fatal("activity id fallback should match")
	}
}

func TestDeviceIdentityIsPerAccount(t *testing.T) {
	plugin.mu.Lock()
	plugin.accounts = map[string]*credential{
		"a": {ID: "a", Alias: "a", AccessToken: "t", DeviceID: "shared"},
		"b": {ID: "b", Alias: "b", AccessToken: "t", DeviceID: "shared"},
	}
	plugin.mu.Unlock()
	plugin.ensureUniqueDevices()
	accounts := plugin.poolSnapshot()
	seen := map[string]bool{}
	for _, account := range accounts {
		if account.DeviceID == "" || seen[account.DeviceID] {
			t.Fatalf("device ids must be unique and set: %+v", account.DeviceID)
		}
		seen[account.DeviceID] = true
		if account.DeviceName == "" {
			t.Fatal("device name should be filled in")
		}
	}
}
