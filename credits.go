package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credits live on the identity gateway's asset manager, the same numbers the
// desktop client shows in its account panel:
//
//	GET /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw
//	GET /agent-assetmgr/api/v1/ledgers_std?asset_type=point&wallet_type=all&page_size=N
//
// Both authenticate with a plain Authorization bearer token (X-Authorization is
// only accepted by the model proxy) and are read-only, so polling costs nothing.
const (
	creditsWalletsPath = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
	creditsLedgersPath = "/agent-assetmgr/api/v1/ledgers_std"
)

type creditWallet struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Balance  int64  `json:"balance"`
	View     string `json:"view,omitempty"`
	Priority int64  `json:"priority"`
	Display  bool   `json:"display"`
}

// creditSnapshot is one account's balance as of FetchedAt.
type creditSnapshot struct {
	Total     int64          `json:"total"`
	Wallets   []creditWallet `json:"wallets"`
	FetchedAt int64          `json:"fetched_at"`
	Error     string         `json:"error,omitempty"`
}

// creditLedger is one row of the points ledger.
type creditLedger struct {
	ID          string `json:"id"`
	At          string `json:"at"`
	Model       string `json:"model"`
	Amount      int64  `json:"amount"`
	Wallet      string `json:"wallet"`
	Mutation    string `json:"mutation"`
	Reason      string `json:"reason"`
	Description string `json:"description"`
	RequestID   string `json:"request_id,omitempty"`
	TokensIn    int64  `json:"tokens_in"`
	TokensOut   int64  `json:"tokens_out"`
	CacheTokens int64  `json:"cache_tokens"`
}

type creditCacheEntry struct {
	snapshot creditSnapshot
	at       time.Time
	failed   bool
}

// creditHeaders is the identity block with the plain bearer token the asset
// manager expects.
func (a *app) creditHeaders(cfg Config, account *credential) http.Header {
	h := a.identityHeaders(cfg, account, false)
	h.Set("Accept", "application/json")
	h.Set("User-Agent", cfg.UserAgent)
	return h
}

func (a *app) fetchCredits(ctx context.Context, account *credential) (creditSnapshot, error) {
	cfg := a.currentConfig()
	if account == nil || account.AccessToken == "" {
		return creditSnapshot{}, fmt.Errorf("账号没有 access_token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.AuthBaseURL+creditsWalletsPath, nil)
	if err != nil {
		return creditSnapshot{}, err
	}
	req.Header = a.creditHeaders(cfg, account)
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return creditSnapshot{}, fmt.Errorf("积分查询失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return creditSnapshot{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return creditSnapshot{}, fmt.Errorf("积分查询失败：HTTP %d %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	var envelope struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
		Data *struct {
			Total   int64 `json:"total_balance"`
			Wallets []struct {
				Type     string `json:"public_wallet_type"`
				Name     string `json:"display_name"`
				Balance  int64  `json:"balance"`
				View     string `json:"balance_view"`
				Priority int64  `json:"priority"`
				Display  bool   `json:"display"`
			} `json:"wallets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return creditSnapshot{}, fmt.Errorf("积分响应格式不对：%s", truncateRunes(string(raw), 160))
	}
	if code := envelope.Code.String(); code != "" && code != "0" {
		return creditSnapshot{}, fmt.Errorf("积分查询失败：code=%s %s", code, strings.TrimSpace(envelope.Msg))
	}
	if envelope.Data == nil {
		return creditSnapshot{}, fmt.Errorf("积分响应没有数据")
	}
	snapshot := creditSnapshot{Total: envelope.Data.Total, FetchedAt: time.Now().UnixMilli()}
	for _, wallet := range envelope.Data.Wallets {
		snapshot.Wallets = append(snapshot.Wallets, creditWallet{
			Type:     wallet.Type,
			Name:     wallet.Name,
			Balance:  wallet.Balance,
			View:     wallet.View,
			Priority: wallet.Priority,
			Display:  wallet.Display,
		})
	}
	return snapshot, nil
}

func (a *app) fetchLedgers(ctx context.Context, account *credential, limit int) ([]creditLedger, error) {
	cfg := a.currentConfig()
	if account == nil || account.AccessToken == "" {
		return nil, fmt.Errorf("账号没有 access_token")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	endpoint := cfg.AuthBaseURL + creditsLedgersPath + "?asset_type=point&wallet_type=all&page_size=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header = a.creditHeaders(cfg, account)
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("积分账单查询失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 4<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("积分账单查询失败：HTTP %d %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	var envelope struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
		Data *struct {
			Entries []struct {
				ID          string `json:"id"`
				OccurredAt  string `json:"occurred_at"`
				Mutation    string `json:"mutation_type"`
				Amount      int64  `json:"amount"`
				WalletScope string `json:"wallet_scope"`
				Description string `json:"description"`
				Metadata    struct {
					Model       string `json:"model"`
					Reason      string `json:"reason"`
					RequestID   string `json:"request_id"`
					TokenUsage  string `json:"token_usage"`
					WalletType  string `json:"public_wallet_type"`
					Description string `json:"description"`
				} `json:"metadata"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("积分账单格式不对：%s", truncateRunes(string(raw), 160))
	}
	if code := envelope.Code.String(); code != "" && code != "0" {
		return nil, fmt.Errorf("积分账单查询失败：code=%s %s", code, strings.TrimSpace(envelope.Msg))
	}
	if envelope.Data == nil {
		return nil, nil
	}
	entries := make([]creditLedger, 0, len(envelope.Data.Entries))
	for _, item := range envelope.Data.Entries {
		in, out, cache := parseTokenUsage(item.Metadata.TokenUsage)
		entries = append(entries, creditLedger{
			ID:          item.ID,
			At:          item.OccurredAt,
			Model:       item.Metadata.Model,
			Amount:      item.Amount,
			Wallet:      firstNonEmpty(item.Metadata.WalletType, item.WalletScope),
			Mutation:    item.Mutation,
			Reason:      item.Metadata.Reason,
			Description: firstNonEmpty(item.Description, item.Metadata.Description),
			RequestID:   item.Metadata.RequestID,
			TokensIn:    in,
			TokensOut:   out,
			CacheTokens: cache,
		})
	}
	return entries, nil
}

// parseTokenUsage reads the gateway's "input_tokens:317, output_tokens:35,
// cache_tokens:0" summary.
func parseTokenUsage(raw string) (in, out, cache int64) {
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "input_tokens":
			in = number
		case "output_tokens":
			out = number
		case "cache_tokens":
			cache = number
		}
	}
	return in, out, cache
}

// creditsFor returns a cached balance, refreshing it when stale. Failures are
// cached briefly so a broken account does not get hammered on every page load.
func (a *app) creditsFor(ctx context.Context, account *credential, refresh bool) creditSnapshot {
	if account == nil {
		return creditSnapshot{Error: "账号不存在"}
	}
	ttl := time.Duration(a.currentConfig().CreditsCacheSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Minute
	}
	a.creditMu.Lock()
	cached, ok := a.credits[account.ID]
	a.creditMu.Unlock()
	if ok && !refresh {
		limit := ttl
		if cached.failed {
			limit = 10 * time.Second
		}
		if time.Since(cached.at) < limit {
			return cached.snapshot
		}
	}

	if fresh, errFresh := a.ensureAccountFresh(ctx, account); errFresh == nil && fresh != nil {
		account = fresh
	}
	snapshot, err := a.fetchCredits(ctx, account)
	failed := err != nil
	if failed {
		snapshot.Error = err.Error()
		if ok {
			// Keep the last good numbers visible instead of blanking the panel.
			snapshot.Total = cached.snapshot.Total
			snapshot.Wallets = cached.snapshot.Wallets
			snapshot.FetchedAt = cached.snapshot.FetchedAt
		}
	}
	if snapshot.FetchedAt == 0 {
		snapshot.FetchedAt = time.Now().UnixMilli()
	}
	a.creditMu.Lock()
	if a.credits == nil {
		a.credits = map[string]creditCacheEntry{}
	}
	a.credits[account.ID] = creditCacheEntry{snapshot: snapshot, at: time.Now(), failed: failed}
	a.creditMu.Unlock()
	return snapshot
}

// creditsPayload builds the console view: every account's balance plus a pool
// total that only counts accounts which answered.
func (a *app) creditsPayload(refresh bool) map[string]any {
	accounts := a.poolSnapshot()
	views := make([]map[string]any, 0, len(accounts))
	snapshots := a.creditsForAccounts(accounts, refresh)
	var total int64
	okCount := 0
	for index, account := range accounts {
		snapshot := snapshots[index]
		if snapshot.Error == "" {
			total += snapshot.Total
			okCount++
		}
		views = append(views, map[string]any{
			"id":         account.ID,
			"alias":      account.Alias,
			"user_id":    account.UserID,
			"disabled":   account.Disabled || account.Dead,
			"total":      snapshot.Total,
			"wallets":    snapshot.Wallets,
			"fetched_at": snapshot.FetchedAt,
			"error":      snapshot.Error,
		})
	}
	return map[string]any{
		"accounts": views,
		"summary":  map[string]any{"total": total, "accounts": len(accounts), "reporting": okCount},
	}
}

// creditsForAccounts resolves every balance concurrently under one deadline, so
// a single slow account cannot stall the console.
func (a *app) creditsForAccounts(accounts []*credential, refresh bool) []creditSnapshot {
	out := make([]creditSnapshot, len(accounts))
	if len(accounts) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for index, account := range accounts {
		wg.Add(1)
		go func(index int, account *credential) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[index] = a.creditsFor(ctx, account, refresh)
		}(index, account)
	}
	wg.Wait()
	return out
}

// creditsSummary is the compact form used by /status. It walks the pool, so a
// removed account stops contributing instead of lingering in the total.
func (a *app) creditsSummary() map[string]any {
	accounts := a.poolSnapshot()
	a.creditMu.Lock()
	defer a.creditMu.Unlock()
	var total int64
	var fetchedAt int64
	reporting := 0
	for _, account := range accounts {
		cached, ok := a.credits[account.ID]
		if !ok {
			continue
		}
		// A failed refresh keeps the previous total, which still counts.
		if cached.snapshot.Error != "" && cached.snapshot.Total == 0 {
			continue
		}
		total += cached.snapshot.Total
		reporting++
		if cached.snapshot.FetchedAt > fetchedAt {
			fetchedAt = cached.snapshot.FetchedAt
		}
	}
	return map[string]any{"total": total, "reporting": reporting, "fetched_at": fetchedAt}
}
