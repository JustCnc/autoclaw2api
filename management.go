package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// managementRegistration publishes the console resource plus the JSON routes
// the page talks to. The host mounts resources under
// /v0/resource/plugins/<plugin id><path> and forwards API calls to
// management.handle.
func (a *app) managementRegistration() managementRegistration {
	prefix := "/" + providerKey
	routes := make([]managementRouteSpec, 0, len(managementRoutes))
	for _, route := range managementRoutes {
		routes = append(routes, managementRouteSpec{
			Method: route.method,
			Path:   prefix + route.path,
		})
	}
	return managementRegistration{
		Routes: routes,
		Resources: []resourceRouteSpec{
			{Path: "/console", Menu: "AutoClaw2API", Description: "AutoClaw 反代控制台：账号池、模型目录、限流状态与日志。"},
		},
	}
}

type managementRoute struct {
	method string
	path   string
}

// managementRoutes is the closed set of routes this plugin answers. Matching on
// the suffix keeps working when the host derives the plugin id from the library
// file name instead of the provider key.
var managementRoutes = []managementRoute{
	{"GET", "/console"},
	{"GET", "/status"},
	{"GET", "/accounts"},
	{"POST", "/accounts/refresh"},
	{"POST", "/accounts/reset"},
	{"POST", "/accounts/disable"},
	{"POST", "/accounts/delete"},
	{"POST", "/accounts/import"},
	{"GET", "/accounts/export"},
	{"GET", "/models"},
	{"POST", "/catalog/refresh"},
	{"GET", "/logs"},
	{"GET", "/requests"},
	{"GET", "/credits"},
	{"GET", "/credits/ledgers"},
	{"POST", "/credits/refresh"},
	{"GET", "/tasks"},
	{"POST", "/tasks/claim"},
	{"POST", "/rewards/login"},
	{"GET", "/config"},
	{"POST", "/test"},
	{"GET", "/login/status"},
	{"GET", "/login/captcha"},
	{"POST", "/login/start"},
	{"POST", "/login/callback"},
	{"POST", "/login/poll"},
	{"POST", "/login/phone/send"},
	{"POST", "/login/phone/verify"},
}

func normalizeManagementPath(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if idx := strings.Index(p, providerKey); idx >= 0 {
		trimmed := p[idx+len(providerKey):]
		if trimmed == "" {
			p = "/"
		} else {
			p = trimmed
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
		}
	}
	// Exact match first: "/login/status" must not be captured by "/status".
	for _, route := range managementRoutes {
		if p == route.path {
			return route.path
		}
	}
	for _, route := range managementRoutes {
		if strings.HasSuffix(p, route.path) {
			return route.path
		}
	}
	return p
}

func (a *app) handleManagement(req managementRequest) managementResponse {
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	switch {
	case method == "GET" && path == "/console":
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: consoleHTML,
		}
	case method == "GET" && (path == "/" || path == "/status"):
		return jsonResponse(a.statusPayload())
	case method == "GET" && path == "/accounts":
		return jsonResponse(map[string]any{"accounts": a.accountViews()})
	case method == "POST" && path == "/accounts/refresh":
		return jsonResponse(a.refreshAccountsPayload(req))
	case method == "POST" && path == "/accounts/reset":
		return jsonResponse(a.resetAccountsPayload(req))
	case method == "POST" && path == "/accounts/disable":
		return jsonResponse(a.disableAccountPayload(req))
	case method == "POST" && path == "/accounts/delete":
		return jsonResponse(a.deleteAccountPayload(req))
	case method == "POST" && path == "/accounts/import":
		return jsonResponse(a.importAccountsPayload(req))
	case method == "GET" && path == "/accounts/export":
		return a.exportAccounts()
	case method == "GET" && path == "/models":
		snapshot := a.snapshotCatalog()
		return jsonResponse(map[string]any{
			"provider":   providerKey,
			"source":     snapshot.Source,
			"fetched_at": snapshot.FetchedAt,
			"default":    snapshot.Default,
			"models":     snapshot.Models,
			"exposed":    a.modelInfoList(),
		})
	case method == "POST" && path == "/catalog/refresh":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.refreshCatalog(ctx); err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		snapshot := a.snapshotCatalog()
		return jsonResponse(map[string]any{
			"ok":      true,
			"message": fmt.Sprintf("模型目录已刷新：%d 个模型", len(snapshot.Models)),
			"models":  snapshot.Models,
		})
	case method == "GET" && path == "/logs":
		limit := 300
		if raw := req.Query.Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 2000 {
				limit = parsed
			}
		}
		entries := a.log.snapshot()
		if len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		return jsonResponse(map[string]any{"entries": entries})
	case method == "GET" && path == "/requests":
		limit := 100
		if raw := req.Query.Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
				limit = parsed
			}
		}
		return jsonResponse(map[string]any{
			"summary": a.requests.summary(),
			"entries": a.requests.tail(limit),
		})
	case method == "GET" && path == "/credits":
		refresh := strings.TrimSpace(req.Query.Get("refresh")) == "1"
		return jsonResponse(a.creditsPayload(refresh))
	case method == "POST" && path == "/credits/refresh":
		payload := a.creditsPayload(true)
		summary, _ := payload["summary"].(map[string]any)
		total := int64(0)
		if summary != nil {
			if value, ok := summary["total"].(int64); ok {
				total = value
			}
		}
		return jsonResponse(map[string]any{
			"ok":       true,
			"message":  fmt.Sprintf("积分余额已刷新：合计 %d 积分", total),
			"summary":  payload["summary"],
			"accounts": payload["accounts"],
		})
	case method == "GET" && path == "/credits/ledgers":
		limit := 30
		if raw := req.Query.Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 200 {
				limit = parsed
			}
		}
		return jsonResponse(a.ledgerPayload(req.Query.Get("account"), limit))
	case method == "GET" && path == "/tasks":
		return jsonResponse(a.tasksPayload(req.Query.Get("account")))
	case method == "POST" && path == "/tasks/claim":
		var body struct {
			Account string `json:"account"`
			TaskID  string `json:"task_id"`
		}
		_ = json.Unmarshal(req.Body, &body)
		target := a.pickAccount(body.Account)
		if target == nil {
			return jsonResponse(map[string]any{"ok": false, "message": "账号池是空的"})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result := a.claimRewards(ctx, target, strings.TrimSpace(body.TaskID))
		payload := map[string]any{
			"ok":      result.Points > 0 || result.LoginPoints > 0,
			"message": result.Message,
			"result":  result,
		}
		if result.Points == 0 && result.LoginPoints == 0 && result.Error != "" {
			payload["ok"] = false
		}
		return jsonResponse(payload)
	case method == "POST" && path == "/rewards/login":
		var body struct {
			Account string `json:"account"`
		}
		_ = json.Unmarshal(req.Body, &body)
		target := a.pickAccount(body.Account)
		if target == nil {
			return jsonResponse(map[string]any{"ok": false, "message": "账号池是空的"})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := a.triggerLoginActive(ctx, target); err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		time.Sleep(1500 * time.Millisecond)
		points, entries := a.todayCampaignPoints(ctx, target)
		a.dropCreditsCache(target.ID)
		message := "今天还没有登录奖励到账"
		if points > 0 {
			message = fmt.Sprintf("今日登录奖励已到账 +%d 积分", points)
		}
		return jsonResponse(map[string]any{"ok": true, "message": message, "points": points, "entries": entries})
	case method == "GET" && path == "/config":
		return jsonResponse(map[string]any{"config": a.sanitizedConfig()})
	case method == "POST" && path == "/test":
		return jsonResponse(a.testPayload(req))
	case method == "GET" && path == "/login/status":
		cfg := a.currentConfig()
		base := ""
		a.loginCbMu.Lock()
		if a.loginCbPort > 0 {
			base = fmt.Sprintf("http://localhost:%d", a.loginCbPort)
		}
		a.loginCbMu.Unlock()
		return jsonResponse(map[string]any{
			"callback_base": base,
			"providers":     []string{loginProviderZai, loginProviderGoogle},
			"timeout":       cfg.LoginTimeoutSeconds,
		})
	case method == "GET" && path == "/login/captcha":
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		captcha, err := a.loginCaptchaConfig(ctx)
		if err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		return jsonResponse(map[string]any{"ok": true, "captcha": captcha})
	case method == "POST" && path == "/login/start":
		var body struct {
			Provider string `json:"provider"`
			Captcha  string `json:"captcha_verify_param"`
		}
		_ = json.Unmarshal(req.Body, &body)
		provider := a.loginProvider(body.Provider)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		session, url, err := a.startOAuthLogin(ctx, provider, body.Captcha)
		if err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		a.log.info("login", fmt.Sprintf("已生成 %s 授权链接：%s", provider, url))
		return jsonResponse(map[string]any{"ok": true, "url": url, "state": session.State, "provider": provider, "expires_at": session.Expires})
	case method == "POST" && path == "/login/callback":
		payload, err := a.completeLoginCallback(req.Body)
		if err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		return jsonResponse(payload)
	case method == "POST" && path == "/login/poll":
		var body struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(req.Body, &body)
		session := a.loginSession(strings.TrimSpace(body.State))
		if session == nil {
			return jsonResponse(map[string]any{"status": "error", "message": "登录会话不存在或已过期"})
		}
		if session.Auth != nil {
			return jsonResponse(map[string]any{"status": "success", "message": "登录成功", "alias": session.Auth.Alias})
		}
		if session.Err != "" {
			return jsonResponse(map[string]any{"status": "error", "message": session.Err})
		}
		if time.Now().After(session.Expires) {
			return jsonResponse(map[string]any{"status": "error", "message": "授权链接已过期，请重新发起"})
		}
		return jsonResponse(map[string]any{"status": "pending", "message": "等待在浏览器里完成授权"})
	case method == "POST" && path == "/login/phone/send":
		var body struct {
			Phone string `json:"phone"`
		}
		_ = json.Unmarshal(req.Body, &body)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.sendLoginCode(ctx, body.Phone); err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		return jsonResponse(map[string]any{"ok": true, "message": "验证码已发送"})
	case method == "POST" && path == "/login/phone/verify":
		var body struct {
			Phone string `json:"phone"`
			Code  string `json:"code"`
			Alias string `json:"alias"`
		}
		_ = json.Unmarshal(req.Body, &body)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		account, err := a.completePhoneLogin(ctx, body.Phone, body.Code, body.Alias)
		if err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		return jsonResponse(map[string]any{"ok": true, "message": "已添加账号：" + account.Alias, "alias": account.Alias})
	default:
		return managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"error":"unknown route"}`),
		}
	}
}

func jsonResponse(v any) managementResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(`{"error":"encode failed"}`)
	}
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}

// --- views -----------------------------------------------------------------

type accountView struct {
	ID            string `json:"id"`
	Alias         string `json:"alias"`
	UserID        string `json:"user_id,omitempty"`
	Email         string `json:"email,omitempty"`
	UserName      string `json:"user_name,omitempty"`
	UserPhone     string `json:"user_phone,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	DeviceName    string `json:"device_name,omitempty"`
	DeviceOS      string `json:"device_os,omitempty"`
	RewardDate    string `json:"reward_date,omitempty"`
	Source        string `json:"source,omitempty"`
	Priority      int    `json:"priority"`
	Enterprise    bool   `json:"enterprise"`
	HasRefresh    bool   `json:"has_refresh_token"`
	TokenTail     string `json:"token_tail,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	ExpiresIn     int64  `json:"expires_in_seconds"`
	Disabled      bool   `json:"disabled"`
	Dead          bool   `json:"dead"`
	Cooling       bool   `json:"cooling"`
	CooldownLeft  int64  `json:"cooldown_left_seconds"`
	LastError     string `json:"last_error,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	Active        bool   `json:"active"`
	CPAFile       string `json:"cpa_file"`
	ConfigManaged bool   `json:"config_managed"`
}

func tailOf(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return "…" + value[len(value)-8:]
}

func (a *app) accountViews() []accountView {
	now := time.Now()
	accounts := a.poolSnapshot()
	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		view := accountView{
			ID:            account.ID,
			Alias:         account.Alias,
			UserID:        account.UserID,
			Email:         account.Email,
			UserName:      account.UserName,
			UserPhone:     account.UserPhone,
			DeviceID:      account.DeviceID,
			DeviceName:    account.DeviceName,
			DeviceOS:      account.DeviceOS,
			RewardDate:    account.RewardDate,
			Source:        account.Source,
			Priority:      account.Priority,
			Enterprise:    account.Enterprise,
			HasRefresh:    account.RefreshToken != "",
			TokenTail:     tailOf(account.AccessToken),
			Disabled:      account.Disabled,
			Dead:          account.Dead,
			Cooling:       account.cooling(now),
			LastError:     account.LastError,
			CPAFile:       account.ID + ".json",
			ConfigManaged: account.Source == "config",
			Active:        !account.Disabled && !account.Dead && !account.cooling(now),
		}
		if account.ExpiresAt > 0 {
			view.ExpiresAt = account.expiresAtTime().UTC().Format(time.RFC3339)
			view.ExpiresIn = int64(time.Until(account.expiresAtTime()).Seconds())
		}
		if account.CooldownUntil > 0 {
			view.CooldownLeft = int64(time.Until(time.UnixMilli(account.CooldownUntil)).Seconds())
			if view.CooldownLeft < 0 {
				view.CooldownLeft = 0
			}
		}
		if account.UpdatedAt > 0 {
			view.UpdatedAt = time.UnixMilli(account.UpdatedAt).UTC().Format(time.RFC3339)
		}
		views = append(views, view)
	}
	return views
}

func (a *app) statusPayload() map[string]any {
	cfg := a.currentConfig()
	snapshot := a.snapshotCatalog()
	now := time.Now()

	accounts := a.poolSnapshot()
	usable, cooling, disabled := 0, 0, 0
	for _, account := range accounts {
		switch {
		case account.Disabled || account.Dead:
			disabled++
		case account.cooling(now):
			cooling++
		default:
			usable++
		}
	}
	exposed := a.modelInfoList()
	rpm, burst := cfg.rateLimit()

	return map[string]any{
		"provider":        providerKey,
		"version":         pluginVersion,
		"accounts":        map[string]any{"total": len(accounts), "usable": usable, "cooling": cooling, "disabled": disabled},
		"catalog":         map[string]any{"count": len(snapshot.Models), "exposed": len(exposed), "default": snapshot.Default, "source": snapshot.Source, "fetched_at": snapshot.FetchedAt},
		"limit":           map[string]any{"rpm": int(rpm), "burst": burst},
		"config":          map[string]any{"data_dir": cfg.DataDir, "login_callback_port": cfg.LoginCallbackPort, "model_prefix": modelPrefix},
		"upstream":        map[string]any{"base_url": cfg.BaseURL, "auth_base_url": cfg.AuthBaseURL, "client_version": cfg.ClientVersion, "product": cfg.Product, "channel": cfg.Channel},
		"started_at":      a.startedAt(),
		"server_time":     now.UTC().Format(time.RFC3339),
		"log_entries":     len(a.log.snapshot()),
		"credits":         a.creditsSummary(),
		"requests":        a.requests.summary(),
		"catalog_refresh": cfg.RefreshSeconds,
	}
}

func (a *app) startedAt() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bootAt.IsZero() {
		return ""
	}
	return a.bootAt.UTC().Format(time.RFC3339)
}

// --- actions ---------------------------------------------------------------

func decodeBody[T any](raw []byte) (T, error) {
	var out T
	if len(strings.TrimSpace(string(raw))) == 0 {
		return out, nil
	}
	return out, json.Unmarshal(raw, &out)
}

func (a *app) selectedAccount(req managementRequest) (*credential, bool) {
	body, _ := decodeBody[struct {
		ID string `json:"id"`
	}](req.Body)
	id := normalizeAuthID(body.ID)
	if id == "" {
		return nil, false
	}
	if account := a.poolAccount(id); account != nil {
		return account, true
	}
	if account := a.poolAccount(accountIDForAlias(id)); account != nil {
		return account, true
	}
	for _, account := range a.poolSnapshot() {
		if account.Alias == id {
			return account, true
		}
	}
	return nil, false
}

func (a *app) refreshAccountsPayload(req managementRequest) map[string]any {
	if account, ok := a.selectedAccount(req); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		next, err := a.refreshAccount(ctx, account)
		if err != nil {
			return map[string]any{"ok": false, "message": "刷新失败：" + err.Error()}
		}
		a.publishAccount(next, true)
		return map[string]any{"ok": true, "message": fmt.Sprintf("账号 %s 已刷新", next.Alias)}
	}
	refreshed, failed := 0, 0
	for _, account := range a.poolSnapshot() {
		if account.Disabled || account.Dead || account.RefreshToken == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		next, err := a.refreshAccount(ctx, account)
		cancel()
		if err != nil {
			failed++
			continue
		}
		a.publishAccount(next, true)
		refreshed++
	}
	return map[string]any{"ok": failed == 0, "message": fmt.Sprintf("已刷新 %d 个账号，失败 %d 个", refreshed, failed)}
}

func (a *app) resetAccountsPayload(req managementRequest) map[string]any {
	targets := []*credential{}
	if account, ok := a.selectedAccount(req); ok {
		targets = append(targets, account)
	} else {
		targets = a.poolSnapshot()
	}
	for _, account := range targets {
		a.credMu.Lock()
		if live := a.accounts[account.ID]; live != nil {
			live.CooldownUntil = 0
			live.Dead = false
			live.LastError = ""
		}
		a.credMu.Unlock()
	}
	a.savePool()
	return map[string]any{"ok": true, "message": fmt.Sprintf("已重置 %d 个账号的冷却状态", len(targets))}
}

func (a *app) disableAccountPayload(req managementRequest) map[string]any {
	account, ok := a.selectedAccount(req)
	if !ok {
		return map[string]any{"ok": false, "message": "账号不存在"}
	}
	body, _ := decodeBody[struct {
		Disabled *bool `json:"disabled"`
	}](req.Body)
	disabled := !account.Disabled
	if body.Disabled != nil {
		disabled = *body.Disabled
	}
	a.credMu.Lock()
	if live := a.accounts[account.ID]; live != nil {
		live.Disabled = disabled
	}
	a.credMu.Unlock()
	a.savePool()
	if next := a.poolAccount(account.ID); next != nil {
		a.publishAccount(next, true)
	}
	state := "启用"
	if disabled {
		state = "禁用"
	}
	return map[string]any{"ok": true, "message": fmt.Sprintf("账号 %s 已%s", account.Alias, state)}
}

func (a *app) deleteAccountPayload(req managementRequest) map[string]any {
	account, ok := a.selectedAccount(req)
	if !ok {
		return map[string]any{"ok": false, "message": "账号不存在"}
	}
	if account.Source == "config" {
		return map[string]any{"ok": false, "message": "主账号来自 config.yaml，请改配置而不是在这里删除"}
	}
	cfg := a.currentConfig()
	if strings.TrimSpace(cfg.AccountsDir) != "" && account.Source == "file" {
		path := strings.TrimSpace(cfg.AccountsDir) + "/" + account.Alias + ".json"
		_ = removeFile(path)
	}
	a.deleteAccount(account.ID)
	a.log.info("accounts", "控制台删除账号："+account.Alias)
	return map[string]any{"ok": true, "message": fmt.Sprintf("已从账号池移除 %s（CPA 凭据文件请在 CPA 里删除）", account.Alias)}
}

func (a *app) importAccountsPayload(req managementRequest) map[string]any {
	var body struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(req.Body, &body)
	text := strings.TrimSpace(body.Text)
	if text == "" {
		return map[string]any{"ok": false, "message": "没有可导入的内容"}
	}
	candidates := splitCredentialJSON(text)
	if len(candidates) == 0 {
		return map[string]any{"ok": false, "message": "内容不是合法的 JSON 账号文件"}
	}
	imported, skipped := 0, 0
	for _, raw := range candidates {
		account, errDecode := decodeCredential(raw)
		if errDecode != nil || account.AccessToken == "" || !looksLikeAutoClawToken(account.AccessToken) {
			skipped++
			continue
		}
		alias := sanitizeAlias(account.Alias)
		if alias == "" {
			alias = "import-" + randomHex(3)
		}
		account.ID = accountIDForAlias(alias)
		account.Alias = alias
		account.Source = "console"
		account.UpdatedAt = time.Now().UnixMilli()
		if account.Priority == 0 {
			account.Priority = a.currentConfig().CredentialPriority
		}
		a.upsertAccount(account)
		a.publishAccount(a.poolAccount(account.ID), true)
		imported++
	}
	message := fmt.Sprintf("已导入 %d 个账号", imported)
	if skipped > 0 {
		message += fmt.Sprintf("，跳过 %d 个（不是有效的 AutoClaw 凭据）", skipped)
	}
	return map[string]any{"ok": imported > 0, "message": message}
}

// splitCredentialJSON accepts a single object, an array of objects, an object
// with an "accounts" array, or one object per line (JSONL).
func splitCredentialJSON(text string) []json.RawMessage {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal([]byte(text), &list); err == nil && len(list) > 0 {
		return list
	}
	var wrapped struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(text), &wrapped); err == nil && len(wrapped.Accounts) > 0 {
		return wrapped.Accounts
	}
	var single map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &single); err == nil && len(single) > 0 {
		return []json.RawMessage{json.RawMessage(text)}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if json.Valid([]byte(line)) {
			list = append(list, json.RawMessage(line))
		}
	}
	return list
}
func (a *app) exportAccounts() managementResponse {
	accounts := a.poolSnapshot()
	out := make([]credential, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, *account)
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	name := fmt.Sprintf("%s-accounts-%s.json", providerKey, time.Now().Format("20060102-150405"))
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":        []string{"application/json"},
			"Content-Disposition": []string{`attachment; filename="` + name + `"`},
			"Cache-Control":       []string{"no-store"},
		},
		Body: raw,
	}
}

func (a *app) testPayload(req managementRequest) map[string]any {
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(req.Body, &body)
	model := trimModelPrefix(body.Model)
	if model == "" {
		model = trimModelPrefix(a.snapshotCatalog().Default)
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		prompt = "Say OK"
	}
	resolved, errModel := a.resolveModel(model)
	if errModel != nil {
		return map[string]any{"ok": false, "message": errModel.Error()}
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      resolved,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 64,
		"stream":     false,
	})
	rpc, _ := json.Marshal(executorRequest{Model: resolved, Format: formatChat, Payload: payload})
	start := time.Now()
	out, errExec := a.execute(rpc)
	if errExec != nil {
		return map[string]any{"ok": false, "message": errExec.Error()}
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return map[string]any{"ok": false, "message": "解析响应失败"}
	}
	if !env.OK || env.Error != nil {
		message := "请求失败"
		if env.Error != nil {
			message = env.Error.Message
		}
		return map[string]any{"ok": false, "message": message, "elapsed_ms": time.Since(start).Milliseconds()}
	}
	var result struct {
		Payload []byte `json:"Payload"`
	}
	_ = json.Unmarshal(env.Result, &result)
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
		Model string         `json:"model"`
	}
	_ = json.Unmarshal(result.Payload, &completion)
	content := ""
	if len(completion.Choices) > 0 {
		content = completion.Choices[0].Message.Content
	}
	return map[string]any{
		"ok":         true,
		"message":    fmt.Sprintf("模型 %s 响应正常", resolved),
		"model":      resolved,
		"model_echo": completion.Model,
		"content":    content,
		"usage":      completion.Usage,
		"elapsed_ms": time.Since(start).Milliseconds(),
	}
}

// --- config ----------------------------------------------------------------

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 10 {
		return "••••"
	}
	return value[:4] + "••••" + value[len(value)-4:]
}

func (a *app) sanitizedConfig() []configViewItem {
	cfg := a.currentConfig()
	rpm, burst := cfg.rateLimit()
	return []configViewItem{
		{Key: "data_dir", Value: cfg.DataDir},
		{Key: "base_url", Value: cfg.BaseURL},
		{Key: "auth_base_url", Value: cfg.AuthBaseURL},
		{Key: "accounts_dir", Value: cfg.AccountsDir},
		{Key: "access_token", Value: maskSecret(cfg.AccessToken)},
		{Key: "refresh_token", Value: maskSecret(cfg.RefreshToken)},
		{Key: "device_id", Value: cfg.DeviceID},
		{Key: "credential_alias", Value: cfg.CredentialAlias},
		{Key: "credential_priority", Value: strconv.Itoa(cfg.CredentialPriority)},
		{Key: "app_id", Value: cfg.AppID},
		{Key: "client_version", Value: cfg.ClientVersion},
		{Key: "client_platform", Value: cfg.ClientPlatform},
		{Key: "product", Value: cfg.Product},
		{Key: "channel", Value: cfg.Channel},
		{Key: "lang", Value: cfg.Lang},
		{Key: "user_agent", Value: cfg.UserAgent},
		{Key: "enterprise", Value: strconv.FormatBool(cfg.Enterprise)},
		{Key: "refresh_seconds", Value: strconv.Itoa(cfg.RefreshSeconds)},
		{Key: "refresh_margin_seconds", Value: strconv.Itoa(cfg.RefreshMargin)},
		{Key: "import_interval_seconds", Value: strconv.Itoa(cfg.ImportInterval)},
		{Key: "model_allowlist", Value: strings.Join(cfg.ModelAllowlist, ", ")},
		{Key: "model_denylist", Value: strings.Join(cfg.ModelDenylist, ", ")},
		{Key: "max_attempts", Value: strconv.Itoa(cfg.MaxAttempts)},
		{Key: "cooldown_seconds", Value: strconv.Itoa(cfg.CooldownSeconds)},
		{Key: "rate_limit_rpm", Value: strconv.Itoa(int(rpm))},
		{Key: "rate_limit_burst", Value: strconv.Itoa(burst)},
		{Key: "request_timeout_seconds", Value: strconv.Itoa(cfg.RequestTimeoutSeconds)},
		{Key: "max_tokens_default", Value: strconv.Itoa(cfg.MaxTokensDefault)},
		{Key: "reasoning_effort", Value: cfg.ReasoningEffort},
		{Key: "debug_dump", Value: strconv.FormatBool(cfg.DebugDump)},
		{Key: "login_callback_port", Value: strconv.Itoa(cfg.LoginCallbackPort)},
		{Key: "login_timeout_seconds", Value: strconv.Itoa(cfg.LoginTimeoutSeconds)},
	}
}

type configViewItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// removeFile is a tiny wrapper so the management handlers stay readable.
func removeFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return os.ErrInvalid
	}
	return os.Remove(path)
}

// ledgerPayload returns the points ledger of one account, defaulting to the
// first usable one so the console has something to show without a selection.
func (a *app) ledgerPayload(accountID string, limit int) map[string]any {
	accounts := a.poolSnapshot()
	var target *credential
	if accountID != "" {
		for _, account := range accounts {
			if account.ID == accountID || account.Alias == accountID {
				target = account
				break
			}
		}
	}
	if target == nil && len(accounts) > 0 {
		target = accounts[0]
	}
	if target == nil {
		return map[string]any{"ok": false, "message": "账号池是空的", "entries": []creditLedger{}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	entries, err := a.fetchLedgers(ctx, target, limit)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error(), "account": target.Alias, "entries": []creditLedger{}}
	}
	return map[string]any{
		"ok":         true,
		"account":    target.Alias,
		"account_id": target.ID,
		"entries":    entries,
	}
}
