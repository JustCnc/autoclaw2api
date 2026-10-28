package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
