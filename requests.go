package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// requestEntry is one proxied call, reduced to what an operator looks at when
// something feels slow, broken or expensive.
type requestEntry struct {
	// Seq keys the in-memory entry so the bill watcher can attach points once
	// the upstream ledger becomes visible; it is never serialized.
	Seq              int64   `json:"-"`
	At               int64   `json:"at"`
	Account          string  `json:"account,omitempty"`
	AccountID        string  `json:"account_id,omitempty"`
	Model            string  `json:"model"`
	UpstreamModel    string  `json:"upstream_model,omitempty"`
	Reasoning        string  `json:"reasoning,omitempty"`
	Stream           bool    `json:"stream"`
	Status           int     `json:"status"`
	TTFTms           int64   `json:"ttft_ms"`
	TotalMs          int64   `json:"total_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CacheRate        float64 `json:"cache_rate"`
	// Points is the signed ledger total for this call: negative spent credits,
	// positive a refund or grant booked to the same turn.
	Points *int64 `json:"points,omitempty"`
	Error  string `json:"error,omitempty"`
}

// usageInfo mirrors the OpenAI usage block, including the cache and reasoning
// details the gateway reports.
type usageInfo struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (e *requestEntry) applyUsage(usage *usageInfo) {
	if usage == nil {
		return
	}
	e.PromptTokens = usage.PromptTokens
	e.CompletionTokens = usage.CompletionTokens
	e.TotalTokens = usage.TotalTokens
	e.CachedTokens = usage.PromptTokensDetails.CachedTokens
	e.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	if e.PromptTokens > 0 {
		e.CacheRate = float64(e.CachedTokens) / float64(e.PromptTokens)
	}
}

// parseUsage reads the usage block out of a completion or a stream chunk.
func parseUsage(payload []byte) *usageInfo {
	if len(payload) == 0 {
		return nil
	}
	var probe struct {
		Usage *usageInfo `json:"usage"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return nil
	}
	return probe.Usage
}

// modelNameOf reads the model the gateway reports, which is the upstream id
// without the catalog prefix.
func modelNameOf(payload []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return ""
	}
	return probe.Model
}

// requestLog keeps the most recent calls in memory, newest first.
type requestLog struct {
	mu       sync.Mutex
	entries  []requestEntry
	capacity int
	nextSeq  int64
}

func newRequestLog(capacity int) *requestLog {
	if capacity <= 0 {
		capacity = 300
	}
	return &requestLog{capacity: capacity}
}

func (l *requestLog) add(entry requestEntry) int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextSeq++
	entry.Seq = l.nextSeq
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.capacity {
		l.entries = append([]requestEntry(nil), l.entries[len(l.entries)-l.capacity:]...)
	}
	return entry.Seq
}

// resize keeps the newest entries when the configured capacity changes.
func (l *requestLog) resize(capacity int) {
	if l == nil {
		return
	}
	if capacity <= 0 {
		capacity = 300
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.capacity = capacity
	if len(l.entries) > capacity {
		l.entries = append([]requestEntry(nil), l.entries[len(l.entries)-capacity:]...)
	}
}

func (l *requestLog) setPoints(seq, points int64) {
	if l == nil || seq == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for index := range l.entries {
		if l.entries[index].Seq == seq {
			value := points
			l.entries[index].Points = &value
			return
		}
	}
}

func (l *requestLog) tail(limit int) []requestEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]requestEntry, 0, limit)
	for index := len(l.entries) - 1; index >= 0 && len(out) < limit; index-- {
		out = append(out, l.entries[index])
	}
	return out
}

// summary aggregates the visible window: volume, latency, cache and spend.
func (l *requestLog) summary() map[string]any {
	entries := l.tail(0)
	summary := map[string]any{"total": len(entries)}
	if len(entries) == 0 {
		return summary
	}
	var ttftSum, totalSum, prompt, completion, cached, failures, streams, points int64
	var ttftCount int64
	pointsKnown := false
	for _, entry := range entries {
		if entry.TTFTms > 0 {
			ttftSum += entry.TTFTms
			ttftCount++
		}
		totalSum += entry.TotalMs
		prompt += entry.PromptTokens
		completion += entry.CompletionTokens
		cached += entry.CachedTokens
		if entry.Points != nil {
			points += *entry.Points
			pointsKnown = true
		}
		if entry.Status >= 400 {
			failures++
		}
		if entry.Stream {
			streams++
		}
	}
	if ttftCount > 0 {
		summary["avg_ttft_ms"] = ttftSum / ttftCount
	}
	summary["avg_total_ms"] = totalSum / int64(len(entries))
	summary["prompt_tokens"] = prompt
	summary["completion_tokens"] = completion
	summary["cached_tokens"] = cached
	if prompt > 0 {
		summary["cache_rate"] = float64(cached) / float64(prompt)
	}
	summary["failures"] = int(failures)
	summary["streams"] = int(streams)
	if pointsKnown {
		summary["points"] = points
	}
	return summary
}

// recordRequest stores a finished call and, when it spent credits, queues a
// ledger lookup so the row can show what it cost.
func (a *app) recordRequest(entry requestEntry) {
	if entry.At == 0 {
		entry.At = time.Now().UnixMilli()
	}
	if entry.AccountID != "" && entry.Account == "" {
		entry.Account = a.accountAlias(entry.AccountID)
	}
	seq := a.requests.add(entry)
	if seq == 0 || entry.Status >= 400 || entry.PromptTokens == 0 {
		return
	}
	a.watchBill(billWatch{
		accountID:  entry.AccountID,
		seq:        seq,
		model:      firstNonEmpty(entry.UpstreamModel, trimModelPrefix(entry.Model)),
		prompt:     entry.PromptTokens,
		completion: entry.CompletionTokens,
		at:         entry.At,
		totalMs:    entry.TotalMs,
	})
}

// elapsedMs never rounds a real call down to zero: the console uses 0 to mean
// "not measured".
func elapsedMs(d time.Duration) int64 {
	if ms := d.Milliseconds(); ms > 0 {
		return ms
	}
	if d > 0 {
		return 1
	}
	return 0
}

// accountAlias resolves an account id to the alias shown in the console.
func (a *app) accountAlias(id string) string {
	if id == "" {
		return ""
	}
	if account := a.poolAccount(id); account != nil {
		return account.Alias
	}
	return id
}

// billWatch is one queued points lookup.
type billWatch struct {
	accountID  string
	seq        int64
	model      string
	prompt     int64
	completion int64
	at         int64
	totalMs    int64
}

func (a *app) watchBill(item billWatch) {
	if item.accountID == "" || item.seq == 0 {
		return
	}
	a.billMu.Lock()
	if a.billQueue == nil {
		a.billQueue = make(chan billWatch, 512)
		for worker := 0; worker < 3; worker++ {
			go a.billWorker()
		}
	}
	queue := a.billQueue
	a.billMu.Unlock()
	select {
	case queue <- item:
	default:
		a.log.warn("billing", "积分回填队列已满，跳过一条请求的积分匹配")
	}
}

// billWorker waits out the ledger lag (the bill appears a few seconds after the
// call) and then matches the entry to the request by model, token counts and
// time, so no per-request id has to be invented.
func (a *app) billWorker() {
	for item := range a.billQueue {
		time.Sleep(5 * time.Second)
		a.resolveBill(item)
	}
}

func (a *app) resolveBill(item billWatch) {
	for attempt := 0; attempt < 4; attempt++ {
		account := a.poolAccount(item.accountID)
		if account == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		entries, err := a.fetchLedgers(ctx, account, 25)
		cancel()
		if err == nil {
			if points, ok := a.claimBill(item, entries); ok {
				a.requests.setPoints(item.seq, points)
				return
			}
		}
		time.Sleep(6 * time.Second)
	}
}

// claimBill picks the ledger row that belongs to this request. Rows are claimed
// once, so a burst of identical prompts cannot bill the same row twice.
func (a *app) claimBill(item billWatch, entries []creditLedger) (int64, bool) {
	a.billMu.Lock()
	defer a.billMu.Unlock()
	if a.claimedBills == nil {
		a.claimedBills = map[string]bool{}
	}
	start := time.UnixMilli(item.at)
	end := start.Add(time.Duration(item.totalMs)*time.Millisecond + 5*time.Minute)
	for _, entry := range entries {
		if entry.ID == "" || a.claimedBills[entry.ID] {
			continue
		}
		if entry.TokensIn != item.prompt || entry.TokensOut != item.completion {
			continue
		}
		if !sameModelName(entry.Model, item.model) {
			continue
		}
		occurred, err := time.Parse(time.RFC3339, entry.At)
		if err != nil {
			continue
		}
		if occurred.Before(start.Add(-90*time.Second)) || occurred.After(end) {
			continue
		}
		a.claimedBills[entry.ID] = true
		if len(a.claimedBills) > 20000 {
			a.claimedBills = map[string]bool{entry.ID: true}
		}
		return entry.Amount, true
	}
	return 0, false
}

// sameModelName compares ledger and proxy model ids, tolerating the catalog
// prefixes the gateway strips on its side.
func sameModelName(left, right string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimPrefix(value, "autoclaw/")
		for _, prefix := range []string{"zai_", "zaicoding_"} {
			value = strings.TrimPrefix(value, prefix)
		}
		return value
	}
	left, right = normalize(left), normalize(right)
	// Both sides must name the same model: guessing when one is empty would
	// attach another account's (or another model's) spend to this row.
	return left != "" && left == right
}
