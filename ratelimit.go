package main

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// AutoClaw's managed gateway enforces a per-account request budget: a burst of
// about 40 requests is accepted, roughly 30 requests/minute is sustainable, and
// pushing past it triggers an escalating "请求过于频繁" (HTTP 403, code 810002)
// penalty that blocks even well-formed requests for tens of seconds.
//
// The plugin therefore keeps a per-account token bucket of its own, so a burst
// from a coding agent is smoothed into 429 + Retry-After instead of teaching the
// gateway that this account is abusive.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) take(ratePerSecond, burst float64, now time.Time) (bool, time.Duration) {
	if burst < 1 {
		burst = 1
	}
	if b.last.IsZero() {
		b.tokens = burst
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*ratePerSecond)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	need := 1 - b.tokens
	return false, time.Duration(need / ratePerSecond * float64(time.Second))
}

// takeRequestSlot reserves one upstream request for an account. It returns the
// suggested wait when the local budget for that account is exhausted.
func (a *app) takeRequestSlot(accountID string) (bool, time.Duration) {
	cfg := a.currentConfig()
	rate, burst := cfg.rateLimit()
	if rate <= 0 || accountID == "" {
		return true, 0
	}
	now := time.Now()
	a.limitMu.Lock()
	if a.limits == nil {
		a.limits = map[string]*tokenBucket{}
	}
	bucket := a.limits[accountID]
	if bucket == nil {
		bucket = &tokenBucket{}
		a.limits[accountID] = bucket
	}
	a.limitMu.Unlock()
	return bucket.take(rate/60.0, float64(burst), now)
}

func rateLimitFailure(wait time.Duration) *upstreamFailure {
	if wait < time.Second {
		wait = time.Second
	}
	return &upstreamFailure{
		Status:     http.StatusTooManyRequests,
		Message:    fmt.Sprintf("AutoClaw 账号本地限速：约 %.0f 秒后可重试（rate_limit_rpm 可调）", wait.Seconds()),
		Retryable:  true,
		RetryAfter: wait,
	}
}

// isRateLimitFailure recognises the gateway's burst penalty. The gateway answers
// HTTP 403 with {"code":810002,"message":"请求过于频繁，请稍后再试"}; treating that
// as 429 lets CPA fail over to another account and tells OpenAI clients to back
// off instead of surfacing a misleading permission error.
func isRateLimitFailure(status int, code, message string) bool {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	if strings.TrimSpace(code) == "810002" {
		return true
	}
	lowered := strings.ToLower(message)
	for _, marker := range []string{
		"过于频繁", "频繁", "too many requests", "rate limit", "ratelimit", "too frequent", "slow down",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// poolUnavailable explains why no account could be scheduled. When every usable
// account is cooling down from a rate limit the caller gets a 429 with the real
// wait instead of an opaque 503.
func (a *app) poolUnavailable() *upstreamFailure {
	accounts := a.poolSnapshot()
	if len(accounts) == 0 {
		return nil
	}
	now := time.Now()
	var soonest time.Duration
	usable := false
	known := false
	for _, account := range accounts {
		if account.Dead || account.Disabled {
			continue
		}
		known = true
		if !account.cooling(now) {
			usable = true
			break
		}
		wait := time.UnixMilli(account.CooldownUntil).Sub(now)
		if soonest == 0 || wait < soonest {
			soonest = wait
		}
	}
	if !known || usable {
		return nil
	}
	if soonest <= 0 {
		soonest = time.Second
	}
	return &upstreamFailure{
		Status:     http.StatusTooManyRequests,
		Message:    fmt.Sprintf("AutoClaw 账号都在冷却中，约 %.0f 秒后恢复", soonest.Seconds()),
		Retryable:  true,
		RetryAfter: soonest,
	}
}
