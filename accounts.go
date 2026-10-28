package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// credential is one AutoClaw account. Field names double as the auth-record
// format so files stay inspectable by hand.
type credential struct {
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	Alias         string `json:"alias,omitempty"`
	ID            string `json:"id,omitempty"`
	Source        string `json:"source,omitempty"`
	Enterprise    bool   `json:"enterprise,omitempty"`
	ExpiresAt     int64  `json:"expires_at,omitempty"` // unix milliseconds
	Priority      int    `json:"priority,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	Dead          bool   `json:"dead,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

func (c *credential) clone() *credential {
	if c == nil {
		return nil
	}
	copied := *c
	return &copied
}

func (c *credential) expiresAtTime() time.Time {
	if c == nil || c.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.ExpiresAt)
}

func (c *credential) needsRefresh(margin time.Duration) bool {
	if c == nil || c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(margin).After(c.expiresAtTime())
}

func (c *credential) cooling(now time.Time) bool {
	return c != nil && c.CooldownUntil > now.UnixMilli()
}

// credentialInput also accepts the desktop client's own camelCase fields and a
// bare bearer token so hand-written account files are forgiving.
type credentialInput struct {
	Type         string `json:"type"`
	Provider     string `json:"provider"`
	ID           string `json:"id"`
	Label        string `json:"label"`
	Alias        string `json:"alias"`
	AccessToken  string `json:"access_token"`
	AccessTokenC string `json:"accessToken"`
	Token        string `json:"token"`
	AuthToken    string `json:"auth_token"`
	RefreshToken string `json:"refresh_token"`
	RefreshTokC  string `json:"refreshToken"`
	DeviceID     string `json:"device_id"`
	DeviceIDC    string `json:"deviceId"`
	UserID       string `json:"user_id"`
	UserIDC      string `json:"userId"`
	Enterprise   bool   `json:"enterprise"`
	ExpiresAt    int64  `json:"expires_at"`
	Priority     int    `json:"priority"`
	Disabled     bool   `json:"disabled"`
	Source       string `json:"source"`
	UpdatedAt    int64  `json:"updated_at"`
}

func decodeCredential(raw []byte) (*credential, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty credential")
	}
	var in credentialInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	account := &credential{
		AccessToken:  normalizeToken(firstNonEmpty(in.AccessToken, in.AccessTokenC, in.Token, in.AuthToken)),
		RefreshToken: normalizeToken(firstNonEmpty(in.RefreshToken, in.RefreshTokC)),
		DeviceID:     strings.TrimSpace(firstNonEmpty(in.DeviceID, in.DeviceIDC)),
		UserID:       strings.TrimSpace(firstNonEmpty(in.UserID, in.UserIDC)),
		Alias:        strings.TrimSpace(firstNonEmpty(in.Alias, in.Label)),
		ID:           strings.TrimSpace(in.ID),
		Source:       strings.TrimSpace(in.Source),
		Enterprise:   in.Enterprise,
		ExpiresAt:    in.ExpiresAt,
		Priority:     in.Priority,
		Disabled:     in.Disabled,
		UpdatedAt:    in.UpdatedAt,
	}
	account.normalize()
	return account, nil
}

func (c *credential) normalize() {
	c.AccessToken = normalizeToken(c.AccessToken)
	c.RefreshToken = normalizeToken(c.RefreshToken)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	c.UserID = strings.TrimSpace(c.UserID)
	c.ID = strings.TrimSpace(c.ID)
	c.Alias = strings.TrimSpace(c.Alias)
	if claims, ok := jwtClaims(c.AccessToken); ok {
		if c.ExpiresAt <= 0 {
			c.ExpiresAt = claims.Exp * 1000
		}
		if c.UserID == "" && claims.UserID != 0 {
			c.UserID = fmt.Sprintf("%d", claims.UserID)
		}
		if c.DeviceID == "" {
			c.DeviceID = claims.DeviceID
		}
	}
}

type jwtClaimSet struct {
	Exp      int64  `json:"exp"`
	UserID   int64  `json:"user_id"`
	DeviceID string `json:"device_id"`
	JTI      string `json:"jti"`
}

func jwtClaims(token string) (jwtClaimSet, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return jwtClaimSet{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return jwtClaimSet{}, false
		}
	}
	var claims jwtClaimSet
	if json.Unmarshal(payload, &claims) != nil {
		return jwtClaimSet{}, false
	}
	return claims, true
}

// poolFile is the plugin's private account cache.
type poolFile struct {
	Accounts []*credential `json:"accounts"`
}

func (a *app) poolPath() string {
	return filepath.Join(a.currentConfig().DataDir, "accounts.json")
}

func (a *app) loadPool() {
	store := a.stateStore()
	if store == nil {
		return
	}
	var file poolFile
	if !store.readJSON("accounts.json", &file) {
		return
	}
	a.credMu.Lock()
	if a.accounts == nil {
		a.accounts = map[string]*credential{}
	}
	for _, account := range file.Accounts {
		if account == nil || strings.TrimSpace(account.AccessToken) == "" {
			continue
		}
		account.normalize()
		if account.ID == "" {
			account.ID = accountIDForAlias(firstNonEmpty(account.Alias, "default"))
		}
		a.accounts[account.ID] = account
	}
	a.credMu.Unlock()
	if len(file.Accounts) > 0 {
		a.log.info("accounts", fmt.Sprintf("已载入 %d 个账号", len(file.Accounts)))
	}
}

func (a *app) savePool() {
	store := a.stateStore()
	if store == nil {
		return
	}
	accounts := a.poolSnapshot()
	file := poolFile{Accounts: accounts}
	if err := store.writeJSON("accounts.json", file); err != nil {
		a.log.warn("accounts", "保存账号池失败："+err.Error())
	}
}

func sortAccounts(accounts []*credential) {
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority > accounts[j].Priority
		}
		return accounts[i].ID < accounts[j].ID
	})
}

func (a *app) poolSnapshot() []*credential {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	out := make([]*credential, 0, len(a.accounts))
	for _, account := range a.accounts {
		out = append(out, account.clone())
	}
	sortAccounts(out)
	return out
}

func (a *app) poolAccount(id string) *credential {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	if account, ok := a.accounts[strings.TrimSpace(id)]; ok {
		return account.clone()
	}
	return nil
}

func (a *app) upsertAccount(next *credential) bool {
	if next == nil || next.AccessToken == "" {
		return false
	}
	next.normalize()
	if next.ID == "" {
		next.ID = accountIDForAlias(firstNonEmpty(next.Alias, a.currentConfig().CredentialAlias))
	}
	if next.Alias == "" {
		next.Alias = aliasFromID(next.ID)
	}
	a.credMu.Lock()
	if a.accounts == nil {
		a.accounts = map[string]*credential{}
	}
	current := a.accounts[next.ID]
	changed := current == nil || current.AccessToken != next.AccessToken
	if current != nil {
		if next.Priority == 0 {
			next.Priority = current.Priority
		}
		if !next.Disabled {
			next.Disabled = current.Disabled
		}
		if !changed {
			// Same token: keep runtime cooldown/dead state. A freshly imported
			// token is a new credential and must be allowed to revive the account.
			if next.CooldownUntil == 0 {
				next.CooldownUntil = current.CooldownUntil
			}
			if !next.Dead {
				next.Dead = current.Dead
			}
		}
	}
	a.accounts[next.ID] = next
	a.credMu.Unlock()
	if changed {
		a.savePool()
	}
	return changed
}

func (a *app) deleteAccount(id string) {
	a.credMu.Lock()
	delete(a.accounts, id)
	a.credMu.Unlock()
	a.savePool()
}

func (a *app) selectAccount(preferred string, excluded map[string]bool, now time.Time) *credential {
	accounts := a.poolSnapshot()
	if preferred != "" {
		for _, account := range accounts {
			if account.ID == preferred && !account.Disabled && !account.Dead && !account.cooling(now) {
				return account
			}
		}
	}
	for _, account := range accounts {
		if account.Disabled || account.Dead || account.cooling(now) {
			continue
		}
		if excluded != nil && excluded[account.ID] {
			continue
		}
		return account
	}
	return nil
}

func (a *app) markAccountDead(id, reason string) {
	a.credMu.Lock()
	if account, ok := a.accounts[id]; ok {
		account.Dead = true
		account.LastError = truncateRunes(reason, 300)
	}
	a.credMu.Unlock()
	a.savePool()
}

func (a *app) coolDownAccount(id string, d time.Duration, reason string) {
	if id == "" || d <= 0 {
		return
	}
	a.credMu.Lock()
	if account, ok := a.accounts[id]; ok {
		account.CooldownUntil = time.Now().Add(d).UnixMilli()
		account.LastError = truncateRunes(reason, 300)
	}
	a.credMu.Unlock()
	a.savePool()
}

func (a *app) noteAccountSuccess(id string) {
	a.credMu.Lock()
	if account, ok := a.accounts[id]; ok {
		account.LastError = ""
	}
	a.credMu.Unlock()
}

func (a *app) noteAccountFailure(id string) {
	a.credMu.Lock()
	if account, ok := a.accounts[id]; ok {
		account.LastError = "上游请求失败"
	}
	a.credMu.Unlock()
}

// --- host records -----------------------------------------------------------

type credentialFile struct {
	Type         string `json:"type"`
	ID           string `json:"id,omitempty"`
	Label        string `json:"label,omitempty"`
	Alias        string `json:"alias,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	Source       string `json:"source,omitempty"`
	Enterprise   bool   `json:"enterprise,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	Priority     int    `json:"priority,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
}

func (a *app) publishAccount(account *credential, force bool) {
	if account == nil || account.AccessToken == "" {
		return
	}
	file := credentialFile{
		Type:         providerKey,
		ID:           account.ID,
		Label:        credentialLabel(account),
		Alias:        account.Alias,
		AccessToken:  account.AccessToken,
		RefreshToken: account.RefreshToken,
		DeviceID:     account.DeviceID,
		UserID:       account.UserID,
		Source:       account.Source,
		Enterprise:   account.Enterprise,
		ExpiresAt:    account.ExpiresAt,
		Priority:     account.Priority,
		Disabled:     account.Disabled,
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return
	}
	digest := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(digest[:])
	a.mu.Lock()
	if a.published == nil {
		a.published = map[string]string{}
	}
	unchanged := a.published[account.ID] == fingerprint
	a.mu.Unlock()
	if unchanged && !force {
		return
	}
	if _, err := hostCall(methodHostAuthSave, hostAuthSaveRequest{
		Name: account.ID + ".json",
		JSON: json.RawMessage(raw),
	}); err != nil {
		a.log.warn("accounts", "写入 CPA 凭据失败："+err.Error())
		return
	}
	a.mu.Lock()
	a.published[account.ID] = fingerprint
	a.mu.Unlock()
}

func credentialLabel(account *credential) string {
	if account == nil {
		return "AutoClaw"
	}
	if account.Alias != "" {
		return "AutoClaw " + account.Alias
	}
	if account.UserID != "" {
		return "AutoClaw " + account.UserID
	}
	return "AutoClaw"
}

func (a *app) authRecord(account *credential, fileName string) authData {
	id := account.ID
	if id == "" {
		id = a.currentConfig().credentialID()
	}
	if fileName == "" {
		fileName = id + ".json"
	}
	if account.Alias == "" {
		account.Alias = aliasFromID(id)
	}
	file := credentialFile{
		Type:         providerKey,
		ID:           id,
		Label:        credentialLabel(account),
		Alias:        account.Alias,
		AccessToken:  account.AccessToken,
		RefreshToken: account.RefreshToken,
		DeviceID:     account.DeviceID,
		UserID:       account.UserID,
		Source:       account.Source,
		Enterprise:   account.Enterprise,
		ExpiresAt:    account.ExpiresAt,
		Priority:     account.Priority,
		Disabled:     account.Disabled,
	}
	raw, _ := json.Marshal(file)
	metadata := map[string]any{
		"type":     providerKey,
		"kind":     "autoclaw",
		"source":   account.Source,
		"priority": account.Priority,
	}
	if account.ExpiresAt > 0 {
		metadata["expires_at"] = account.ExpiresAt
	}
	if account.UserID != "" {
		metadata["user_id"] = account.UserID
	}
	return authData{
		Provider:         providerKey,
		ID:               id,
		FileName:         fileName,
		Label:            credentialLabel(account),
		StorageJSON:      raw,
		Metadata:         metadata,
		NextRefreshAfter: a.nextRefreshAfter(account),
	}
}

func (a *app) nextRefreshAfter(account *credential) time.Time {
	if account == nil || account.ExpiresAt <= 0 {
		return time.Now().Add(30 * time.Minute)
	}
	next := account.expiresAtTime().Add(-a.currentConfig().refreshMargin())
	if next.Before(time.Now().Add(time.Minute)) {
		next = time.Now().Add(time.Minute)
	}
	return next
}

func (a *app) ensureHostRecords() {
	for _, account := range a.poolSnapshot() {
		if account.AccessToken != "" {
			a.publishAccount(account, false)
		}
	}
}

// --- account directory ------------------------------------------------------

func (a *app) scanAccountsDir(cfg Config) {
	dir := strings.TrimSpace(cfg.AccountsDir)
	if dir == "" {
		for _, candidate := range accountsDirCandidates(cfg.AccountsDir) {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				dir = candidate
				break
			}
		}
	}
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		alias := sanitizeAlias(strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		if alias == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		account, err := decodeAccountFile(raw)
		if err != nil || account.AccessToken == "" {
			continue
		}
		alias = aliasFromFileName(alias)
		id := accountIDForAlias(alias)
		seen[id] = true
		account.ID = id
		account.Alias = alias
		account.Source = "file"
		account.UpdatedAt = time.Now().UnixMilli()
		if account.Priority == 0 {
			account.Priority = cfg.CredentialPriority
		}
		a.upsertAccount(account)
		a.publishAccount(a.poolAccount(id), false)
	}
	for _, account := range a.poolSnapshot() {
		if account.Source == "file" && !seen[account.ID] {
			a.deleteAccount(account.ID)
			a.log.info("accounts", "账号文件已移除："+account.ID)
		}
	}
}

// --- refresh ---------------------------------------------------------------

type refreshFlight struct {
	done chan struct{}
	cred *credential
	err  error
}

func (a *app) ensureAccountFresh(ctx context.Context, account *credential) (*credential, error) {
	if account == nil {
		return nil, fmt.Errorf("没有可用账号")
	}
	if !account.needsRefresh(a.currentConfig().refreshMargin()) {
		return account, nil
	}
	if account.RefreshToken == "" {
		return account, fmt.Errorf("账号 %s 没有 refresh_token", account.Alias)
	}
	return a.refreshAccount(ctx, account)
}

func (a *app) refreshAccount(ctx context.Context, account *credential) (*credential, error) {
	if account == nil {
		return nil, fmt.Errorf("没有可刷新的账号")
	}
	a.credMu.Lock()
	if current := a.accounts[account.ID]; current != nil && current.AccessToken != "" &&
		account.AccessToken != "" && current.AccessToken != account.AccessToken {
		cloned := current.clone()
		a.credMu.Unlock()
		return cloned, nil
	}
	if a.refresh == nil {
		a.refresh = map[string]*refreshFlight{}
	}
	if flight, ok := a.refresh[account.ID]; ok {
		a.credMu.Unlock()
		select {
		case <-flight.done:
			return flight.cred, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &refreshFlight{done: make(chan struct{})}
	a.refresh[account.ID] = flight
	a.credMu.Unlock()

	next, err := func() (*credential, error) {
		defer func() {
			a.credMu.Lock()
			if a.refresh != nil && a.refresh[account.ID] == flight {
				delete(a.refresh, account.ID)
			}
			a.credMu.Unlock()
			close(flight.done)
		}()
		refreshed, errRefresh := a.doRefresh(ctx, account)
		if errRefresh == nil && refreshed != nil {
			refreshed.ID = account.ID
			refreshed.Alias = account.Alias
			refreshed.Priority = account.Priority
			refreshed.DeviceID = firstNonEmpty(refreshed.DeviceID, account.DeviceID)
			refreshed.Source = firstNonEmpty(account.Source, refreshed.Source)
			a.upsertAccount(refreshed)
			a.publishAccount(refreshed, false)
		}
		a.credMu.Lock()
		flight.cred = refreshed
		flight.err = errRefresh
		a.credMu.Unlock()
		return refreshed, errRefresh
	}()
	if err != nil {
		a.log.warn("accounts", fmt.Sprintf("账号 %s 刷新失败：%s", account.Alias, err.Error()))
		return nil, err
	}
	a.log.info("accounts", fmt.Sprintf("账号 %s 已刷新，有效期至 %s", next.Alias, next.expiresAtTime().Local().Format("2006-01-02 15:04:05")))
	return next, nil
}

func (a *app) doRefresh(ctx context.Context, account *credential) (*credential, error) {
	cfg := a.currentConfig()
	path := "/userapi/v1/refresh"
	refreshed, code, err := a.requestTokenRefresh(ctx, cfg, path, account)
	if err == nil {
		return refreshed, nil
	}
	if code != "400002" {
		return nil, err
	}
	refreshed, _, err = a.requestTokenRefresh(ctx, cfg, "/userapi/v1/agent-refresh", account)
	return refreshed, err
}

func (a *app) requestTokenRefresh(ctx context.Context, cfg Config, path string, account *credential) (*credential, string, error) {
	body, _ := json.Marshal(map[string]any{
		"refresh_token": account.RefreshToken,
		"source_id":     "autoclaw",
		"device_id":     account.DeviceID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AuthBaseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, "", err
	}
	req.Header = a.identityHeaders(cfg, account, false)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, "", &upstreamFailure{Status: http.StatusBadGateway, Message: "刷新连接失败：" + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", err
	}
	var envelope struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", fmt.Errorf("刷新响应不是 JSON：%w", err)
	}
	code := envelope.Code.String()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || code != "0" {
		message := firstNonEmpty(envelope.Msg, strings.TrimSpace(string(raw)))
		return nil, code, &upstreamFailure{
			Status:  resp.StatusCode,
			Code:    code,
			Message: "刷新失败：" + truncateRunes(message, 300),
		}
	}
	access := normalizeToken(envelope.Data.AccessToken)
	refresh := normalizeToken(envelope.Data.RefreshToken)
	if access == "" {
		return nil, code, fmt.Errorf("刷新响应没有 access_token")
	}
	return &credential{
		AccessToken:  access,
		RefreshToken: firstNonEmpty(refresh, account.RefreshToken),
		DeviceID:     account.DeviceID,
		UserID:       account.UserID,
		Enterprise:   account.Enterprise,
		Source:       account.Source,
		UpdatedAt:    time.Now().UnixMilli(),
	}, code, nil
}

func (a *app) anyUsableCredential(ctx context.Context) (*credential, error) {
	cfg := a.currentConfig()
	if account := a.selectAccount(cfg.credentialID(), nil, time.Now()); account != nil {
		return a.ensureAccountFresh(ctx, account)
	}
	if account := a.poolAccount(cfg.credentialID()); account != nil && account.AccessToken != "" {
		return account, nil
	}
	if accounts := a.poolSnapshot(); len(accounts) > 0 {
		return a.ensureAccountFresh(ctx, accounts[0])
	}
	return nil, fmt.Errorf("没有可用账号：请在 CPA 里授权登录，或添加账号 JSON")
}

func (a *app) credentialFromConfig(cfg Config) *credential {
	if cfg.AccessToken == "" {
		return nil
	}
	account := &credential{
		AccessToken:  cfg.AccessToken,
		RefreshToken: cfg.RefreshToken,
		DeviceID:     cfg.DeviceID,
		Alias:        cfg.CredentialAlias,
		ID:           cfg.credentialID(),
		Priority:     cfg.CredentialPriority,
		Enterprise:   cfg.Enterprise,
		Source:       "config",
		UpdatedAt:    time.Now().UnixMilli(),
	}
	account.normalize()
	return account
}

func normalizeAuthID(id string) string {
	return strings.TrimSuffix(strings.TrimSpace(id), ".json")
}

func accountIDForAlias(alias string) string {
	alias = sanitizeAlias(alias)
	for strings.HasPrefix(alias, providerKey+"-") {
		alias = strings.TrimPrefix(alias, providerKey+"-")
	}
	if alias == "" {
		alias = "default"
	}
	return providerKey + "-" + alias
}

// aliasFromFileName turns an auth record file name into a pool alias without
// letting the provider prefix accumulate on every reload. CPA persists a plugin
// auth record by prefixing the provider key, so this must be idempotent.
func aliasFromFileName(name string) string {
	base := strings.TrimSuffix(strings.TrimSpace(name), ".json")
	for strings.HasPrefix(base, providerKey+"-") {
		base = strings.TrimPrefix(base, providerKey+"-")
	}
	return sanitizeAlias(base)
}

func aliasFromID(id string) string {
	return aliasFromFileName(id)
}

func looksLikeAutoClawToken(token string) bool {
	claims, ok := jwtClaims(token)
	return ok && (claims.DeviceID != "" || claims.UserID != 0)
}

func fallbackPriority(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func randomHex(bytesLen int) string {
	if bytesLen <= 0 {
		bytesLen = 8
	}
	buf := make([]byte, bytesLen)
	if _, err := readRandom(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// decodeAccountFile accepts only AutoClaw credential files. It is used by the
// optional accounts_dir scanner, where unrelated provider files may sit next to
// ours (for example a shared CPA oauth directory).
func decodeAccountFile(raw []byte) (*credential, error) {
	var probe struct {
		Type        string `json:"type"`
		Provider    string `json:"provider"`
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	declared := strings.ToLower(strings.TrimSpace(firstNonEmpty(probe.Type, probe.Provider)))
	if declared != "" && !isOurProviderName(declared) {
		return nil, fmt.Errorf("not an autoclaw credential")
	}
	account, err := decodeCredential(raw)
	if err != nil {
		return nil, err
	}
	if account.AccessToken == "" || !looksLikeAutoClawToken(account.AccessToken) {
		return nil, fmt.Errorf("not an autoclaw token")
	}
	return account, nil
}
