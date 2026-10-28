package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AutoClaw's desktop sign-in is an OAuth dance, exactly like the upstream
// client:
//
//	POST /userapi/overseasv1/{zai|google}-oauth-url   {navigate_uri, captcha proof}
//	  -> data.oauth_url  (a real chat.z.ai / accounts.google.com authorize link)
//	browser -> https://localhost:<port>/auth/callback-{zai|google}?code&state
//	POST /userapi/overseasv1/{zai|google}-oauth-login {code, state, navigate_uri}
//	  -> data.access_token / refresh_token
//
// The gateway also fronts the oauth-url endpoints with an Aliyun captcha: a
// request without a valid proof is answered with code 631002 ("当前版本已停止
// 服务"), and a wrong proof with 630014. The widget therefore has to run
// somewhere a human can click it; the plugin console embeds it and forwards the
// resulting captchaVerifyParam here.
const (
	loginProviderZai    = "zai"
	loginProviderGoogle = "google"
)

// The desktop client binds the first free port from this list before asking for
// the authorize URL, so the redirect_uri stays inside the OAuth client's
// whitelist. The tail entries are plugin-only fallbacks.
var defaultLoginCallbackPorts = []int{18432, 19654, 19723, 53699, 18764, 18765, 18766}

type loginSession struct {
	State         string
	Provider      string
	NavigateURI   string
	UpstreamState string
	Created       time.Time
	Expires       time.Time
	Auth          *credential
	Err           string
}

func (a *app) loginProvider(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), loginProviderGoogle) {
		return loginProviderGoogle
	}
	return loginProviderZai
}

func (a *app) newLoginSession(provider, navigateURI string) *loginSession {
	cfg := a.currentConfig()
	now := time.Now()
	session := &loginSession{
		State:       randomHex(32),
		Provider:    provider,
		NavigateURI: navigateURI,
		Created:     now,
		Expires:     now.Add(time.Duration(cfg.LoginTimeoutSeconds) * time.Second),
	}
	a.loginMu.Lock()
	if a.logins == nil {
		a.logins = map[string]*loginSession{}
	}
	for state, existing := range a.logins {
		if now.After(existing.Expires) {
			delete(a.logins, state)
		}
	}
	a.logins[session.State] = session
	a.loginMu.Unlock()
	return session
}

func (a *app) loginSession(state string) *loginSession {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	session, ok := a.logins[strings.TrimSpace(state)]
	if !ok {
		return nil
	}
	cloned := *session
	return &cloned
}

// pendingLoginForProvider returns the newest unfinished session for a provider,
// which is the one the loopback callback belongs to.
func (a *app) pendingLoginForProvider(provider string) *loginSession {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	now := time.Now()
	var best *loginSession
	for _, session := range a.logins {
		if session.Provider != provider || session.Auth != nil || session.Err != "" {
			continue
		}
		if now.After(session.Expires) {
			continue
		}
		if best == nil || session.Created.After(best.Created) {
			best = session
		}
	}
	if best == nil {
		return nil
	}
	cloned := *best
	return &cloned
}

func (a *app) finishLogin(state string, account *credential, err error) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	session, ok := a.logins[strings.TrimSpace(state)]
	if !ok {
		return
	}
	if err != nil {
		session.Err = err.Error()
		return
	}
	session.Auth = account
}

// --- loopback callback -----------------------------------------------------

// ensureLoginCallback binds the loopback receiver and returns its base URL, e.g.
// http://localhost:19654. The listener answers only a code for a session this
// plugin started.
func (a *app) ensureLoginCallback() (string, error) {
	a.loginCbMu.Lock()
	if a.loginCb != nil {
		base := fmt.Sprintf("http://localhost:%d", a.loginCbPort)
		a.loginCbMu.Unlock()
		return base, nil
	}
	a.loginCbMu.Unlock()

	cfg := a.currentConfig()
	ports := defaultLoginCallbackPorts
	if cfg.LoginCallbackPort > 0 {
		ports = append([]int{cfg.LoginCallbackPort}, ports...)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.serveLoginCallback)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	for _, port := range ports {
		// Windows happily binds 0.0.0.0:<port> while another process owns
		// 127.0.0.1:<port>, but then the browser's localhost callback would go to
		// that other process (AutoClaw itself listens on 18432). Probe loopback
		// first so we only pick a port that actually reaches us.
		if conn, errDial := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond); errDial == nil {
			_ = conn.Close()
			continue
		}
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		a.loginCbMu.Lock()
		if a.loginCb != nil {
			a.loginCbMu.Unlock()
			_ = listener.Close()
			base := fmt.Sprintf("http://localhost:%d", a.loginCbPort)
			return base, nil
		}
		a.loginCb = server
		a.loginCbPort = port
		a.loginCbMu.Unlock()
		go func() {
			if errServe := server.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
				a.log.warn("login", "授权回调服务退出："+errServe.Error())
			}
		}()
		a.log.info("login", fmt.Sprintf("授权回调已监听 :%d/auth/callback-{zai,google}", port))
		return fmt.Sprintf("http://localhost:%d", port), nil
	}
	return "", fmt.Errorf("回调端口都被占用（%v）", ports)
}

func (a *app) stopLoginCallback() {
	a.loginCbMu.Lock()
	server := a.loginCb
	port := a.loginCbPort
	a.loginCb = nil
	a.loginCbPort = 0
	a.loginCbMu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Shutdown(ctx)
		cancel()
		a.log.info("login", fmt.Sprintf("授权回调已停止 :%d", port))
	}
}

func (a *app) loginCallbackURL(provider string) (string, error) {
	base, err := a.ensureLoginCallback()
	if err != nil {
		return "", err
	}
	return base + "/auth/callback-" + provider, nil
}

func (a *app) serveLoginCallback(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path != "/auth/callback-zai" && path != "/auth/callback-google" {
		http.NotFound(w, r)
		return
	}
	a.log.info("login", "收到浏览器回调 "+path)
	query := r.URL.Query()
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" || state == "" {
		writeCallbackPage(w, http.StatusBadRequest, false, "授权没有完成", "回调里缺少 code 或 state，请回到控制台重新发起授权。")
		return
	}
	payload, _ := json.Marshal(callbackInput{Code: code, State: state, URL: "http://localhost" + r.URL.RequestURI()})
	result, err := a.completeLoginCallback(payload)
	if err != nil {
		writeCallbackPage(w, http.StatusBadRequest, false, "授权失败", err.Error())
		return
	}
	alias, _ := result["alias"].(string)
	writeCallbackPage(w, http.StatusOK, true, "登录成功", "账号「"+alias+"」已加入账号池，可以关闭这个页面了。")
}

func writeCallbackPage(w http.ResponseWriter, status int, ok bool, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	accent := "#b91c1c"
	icon := "M12 8v5m0 3.5h.01"
	if ok {
		accent = "#15803d"
		icon = "M4 12.5l5 5L20 6.5"
	}
	fmt.Fprintf(w, `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%[1]s</title>
<style>
:root{color-scheme:light}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:#f6f7f9;color:#111827;
  font:400 14px/1.6 system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
.card{width:min(420px,calc(100vw - 32px));padding:32px;background:#fff;border:1px solid #e3e6ec;
  border-radius:12px;box-shadow:0 8px 24px rgba(15,23,42,.06);text-align:center}
.badge{width:48px;height:48px;margin:0 auto 16px;border-radius:50%%;display:flex;
  align-items:center;justify-content:center;background:%[2]s1a}
.badge svg{width:24px;height:24px;stroke:%[2]s;fill:none;stroke-width:2.2;
  stroke-linecap:round;stroke-linejoin:round}
h1{margin:0 0 8px;font-size:17px;font-weight:600}
p{margin:0;color:#5c6370;font-size:13px;word-break:break-word}
</style></head><body>
<div class="card">
  <div class="badge"><svg viewBox="0 0 24 24"><path d="%[3]s"/></svg></div>
  <h1>%[1]s</h1>
  <p>%[4]s</p>
</div></body></html>`, htmlEscape(title), accent, icon, htmlEscape(detail))
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}

// --- upstream calls --------------------------------------------------------

type captchaConfig struct {
	Enabled  bool   `json:"enabled"`
	Region   string `json:"region"`
	Prefix   string `json:"prefix"`
	SceneID  string `json:"scene_id"`
	Supplier string `json:"captcha_supplier"`
}

type oauthURLResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		OAuthURL string `json:"oauth_url"`
	} `json:"data"`
}

type oauthLoginResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		UserID       json.Number `json:"user_id"`
		UserName     string      `json:"user_name"`
		UserPhone    string      `json:"user_phone"`
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		EnterpriseID string      `json:"enterprise_id"`
	} `json:"data"`
}

func (a *app) postIdentity(ctx context.Context, path string, payload any) ([]byte, error) {
	raw, status, ctype, err := a.doIdentity(ctx, path, payload)
	if err != nil {
		return nil, err
	}
	if json.Valid(raw) {
		return raw, nil
	}
	// An edge WAF or a local proxy occasionally answers with an HTML page or an
	// empty body; one retry on a fresh connection usually returns the real JSON.
	a.resetHTTPClient()
	raw, status, ctype, err = a.doIdentity(ctx, path, payload)
	if err != nil {
		return nil, err
	}
	if json.Valid(raw) {
		return raw, nil
	}
	snippet := strings.Join(strings.Fields(string(raw)), " ")
	if snippet == "" {
		snippet = "（空响应）"
	}
	a.log.warn("login", fmt.Sprintf("身份网关返回非 JSON（HTTP %d, %s）：%s", status, ctype, truncateRunes(snippet, 200)))
	return nil, fmt.Errorf("AutoClaw 返回了非 JSON 响应（HTTP %d, %s）：%s", status, ctype, truncateRunes(snippet, 160))
}

func (a *app) doIdentity(ctx context.Context, path string, payload any) ([]byte, int, string, error) {
	cfg := a.currentConfig()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AuthBaseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, "", err
	}
	req.Header = a.loginHeaders(cfg)
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("连接 AutoClaw 失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Get("Content-Type"), err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, resp.Header.Get("Content-Type"),
			fmt.Errorf("AutoClaw 返回 HTTP %d：%s", resp.StatusCode, truncateRunes(string(raw), 200))
	}
	return raw, resp.StatusCode, resp.Header.Get("Content-Type"), nil
}

func (a *app) loginCaptchaConfig(ctx context.Context) (captchaConfig, error) {
	cfg := a.currentConfig()
	raw, err := a.postIdentity(ctx, "/userapi/overseasv1/oauth-captcha-config", map[string]any{
		"source_id": "autoclaw",
		"device_id": a.deviceID(cfg),
	})
	if err != nil {
		return captchaConfig{}, err
	}
	var envelope struct {
		Code int           `json:"code"`
		Msg  string        `json:"msg"`
		Data captchaConfig `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return captchaConfig{}, fmt.Errorf("验证码配置响应不是 JSON")
	}
	if envelope.Code != 0 {
		return captchaConfig{}, fmt.Errorf("%s", firstNonEmpty(envelope.Msg, "读取验证码配置失败"))
	}
	return envelope.Data, nil
}

func (a *app) requestOAuthURL(ctx context.Context, provider, navigateURI, captcha string) (string, error) {
	cfg := a.currentConfig()
	payload := map[string]any{
		"navigate_uri": navigateURI,
		"source_id":    "autoclaw",
		"device_id":    a.deviceID(cfg),
	}
	if strings.TrimSpace(captcha) != "" {
		payload["ali_captcha_verify_param"] = captcha
	}
	raw, err := a.postIdentity(ctx, "/userapi/overseasv1/"+provider+"-oauth-url", payload)
	if err != nil {
		return "", err
	}
	var envelope oauthURLResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("授权链接响应不是 JSON")
	}
	switch envelope.Code {
	case 0:
	case 631002:
		return "", fmt.Errorf("AutoClaw 拒绝了请求：需要先在页面里完成安全验证（缺 captchaVerifyParam 时网关会返回这个错误）")
	case 630014:
		return "", fmt.Errorf("安全验证没有通过，请重新点击验证码")
	case 400001:
		return "", fmt.Errorf("参数被拒绝：navigate_uri 不在 OAuth 回调白名单里")
	default:
		return "", fmt.Errorf("%s（code=%d）", firstNonEmpty(envelope.Msg, "获取授权链接失败"), envelope.Code)
	}
	if envelope.Data == nil || strings.TrimSpace(envelope.Data.OAuthURL) == "" {
		return "", fmt.Errorf("授权链接响应里没有 oauth_url")
	}
	return strings.TrimSpace(envelope.Data.OAuthURL), nil
}

func (a *app) exchangeOAuthCode(ctx context.Context, provider, code, state, navigateURI string) (*credential, error) {
	cfg := a.currentConfig()
	raw, err := a.postIdentity(ctx, "/userapi/overseasv1/"+provider+"-oauth-login", map[string]any{
		"code":         code,
		"state":        state,
		"navigate_uri": navigateURI,
		"source_id":    "autoclaw",
		"device_id":    a.deviceID(cfg),
	})
	if err != nil {
		return nil, err
	}
	var envelope oauthLoginResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("授权响应格式不对：%s", truncateRunes(strings.Join(strings.Fields(string(raw)), " "), 160))
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("%s（code=%d）", firstNonEmpty(envelope.Msg, "授权失败"), envelope.Code)
	}
	if envelope.Data == nil {
		return nil, fmt.Errorf("授权响应没有数据")
	}
	access := normalizeToken(envelope.Data.AccessToken)
	refresh := normalizeToken(envelope.Data.RefreshToken)
	if access == "" {
		return nil, fmt.Errorf("授权响应没有 access_token")
	}
	account := &credential{
		AccessToken:  access,
		RefreshToken: refresh,
		UserID:       envelope.Data.UserID.String(),
		Source:       "login",
		Enterprise:   strings.TrimSpace(envelope.Data.EnterpriseID) != "",
		UpdatedAt:    time.Now().UnixMilli(),
	}
	account.normalize()
	return account, nil
}

// startOAuthLogin creates a session, asks the gateway for the authorize link and
// returns the link the user has to open.
func (a *app) startOAuthLogin(ctx context.Context, provider, captcha string) (*loginSession, string, error) {
	navigateURI, err := a.loginCallbackURL(provider)
	if err != nil {
		return nil, "", err
	}
	session := a.newLoginSession(provider, navigateURI)
	authorizeURL, err := a.requestOAuthURL(ctx, provider, navigateURI, captcha)
	if err != nil {
		return nil, "", err
	}
	// The authorize link carries the state the gateway echoes back in the
	// callback, which is how a pasted callback URL is matched to this session.
	if parsed, errParse := url.Parse(authorizeURL); errParse == nil {
		if upstream := strings.TrimSpace(parsed.Query().Get("state")); upstream != "" {
			a.loginMu.Lock()
			if stored, ok := a.logins[session.State]; ok {
				stored.UpstreamState = upstream
				session.UpstreamState = upstream
			}
			a.loginMu.Unlock()
		}
	}
	return session, authorizeURL, nil
}

// loginSessionByUpstreamState finds the session whose authorize link carried the
// state that came back in the callback.
func (a *app) loginSessionByUpstreamState(state string) *loginSession {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil
	}
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	for _, session := range a.logins {
		if session.UpstreamState == state {
			cloned := *session
			return &cloned
		}
	}
	return nil
}

// registerLoginAccount normalises an account produced by any login path and
// publishes it to the pool.
func (a *app) registerLoginAccount(account *credential, alias, source string) *credential {
	cfg := a.currentConfig()
	if account.DeviceID == "" {
		account.DeviceID = a.deviceID(cfg)
	}
	account.Source = firstNonEmpty(source, "login")
	if account.Priority == 0 {
		account.Priority = cfg.CredentialPriority
	}
	if account.Alias == "" {
		account.Alias = sanitizeAlias(firstNonEmpty(alias, account.UserID))
	}
	if account.Alias == "" {
		account.Alias = "login-" + randomHex(3)
	}
	account.ID = accountIDForAlias(account.Alias)
	account.UpdatedAt = time.Now().UnixMilli()
	a.upsertAccount(account)
	published := a.poolAccount(account.ID)
	a.publishAccount(published, true)
	return published
}

// --- CPA auth.login.* ------------------------------------------------------

// startLogin answers CPA's auth.login.start. The gateway needs a captcha proof
// that only a human can produce, so CPA's generic link flow cannot complete it;
// the plugin console hosts the widget instead.
// callbackInput accepts whatever the user can copy out of the browser address
// bar: the full callback URL, just the query string, or the two fields.
type callbackInput struct {
	URL   string `json:"url"`
	Code  string `json:"code"`
	State string `json:"state"`
}

func parseCallbackInput(raw []byte) (provider, code, state, navigateURI string, err error) {
	body := strings.TrimSpace(string(raw))
	var input callbackInput
	if strings.HasPrefix(body, "{") {
		if errJSON := json.Unmarshal(raw, &input); errJSON != nil {
			return "", "", "", "", fmt.Errorf("请求格式不对")
		}
		body = strings.TrimSpace(input.URL)
		if body == "" {
			code, state = strings.TrimSpace(input.Code), strings.TrimSpace(input.State)
		}
	}
	if body != "" {
		if strings.HasPrefix(body, "code=") || strings.HasPrefix(body, "?") {
			// A bare query string: the user copied only the part after the '?'.
			query, errQuery := url.ParseQuery(strings.TrimPrefix(body, "?"))
			if errQuery != nil {
				return "", "", "", "", fmt.Errorf("回调链接无法解析")
			}
			code, state = strings.TrimSpace(query.Get("code")), strings.TrimSpace(query.Get("state"))
		} else {
			if !strings.Contains(body, "://") {
				body = "http://" + strings.TrimPrefix(body, "//")
			}
			parsed, errParse := url.Parse(body)
			if errParse != nil {
				return "", "", "", "", fmt.Errorf("回调链接无法解析")
			}
			query := parsed.Query()
			code, state = strings.TrimSpace(query.Get("code")), strings.TrimSpace(query.Get("state"))
			if parsed.Host != "" {
				navigateURI = parsed.Scheme + "://" + parsed.Host + strings.TrimSuffix(parsed.Path, "/")
			}
			switch {
			case strings.Contains(parsed.Path, "google"):
				provider = loginProviderGoogle
			case strings.Contains(parsed.Path, "zai"):
				provider = loginProviderZai
			}
		}
	}
	if code == "" || state == "" {
		return "", "", "", "", fmt.Errorf("回调链接里没有 code 或 state，请复制浏览器地址栏里的完整链接")
	}
	if provider == "" {
		provider = loginProviderZai
	}
	return provider, code, state, navigateURI, nil
}

// completeLoginCallback finishes a login from a pasted callback URL. A browser on
// the same machine usually reaches the plugin's loopback listener directly, but a
// link opened elsewhere, or a callback port taken by the AutoClaw client itself,
// never arrives there; pasting the URL is the reliable fallback.
func (a *app) completeLoginCallback(raw []byte) (map[string]any, error) {
	provider, code, state, navigateURI, err := parseCallbackInput(raw)
	if err != nil {
		return nil, err
	}
	session := a.loginSessionByUpstreamState(state)
	if session == nil {
		session = a.pendingLoginForProvider(provider)
	}
	if session == nil {
		return nil, fmt.Errorf("没有匹配的授权会话，请先点「生成授权链接」")
	}
	if session.Auth != nil {
		return map[string]any{"ok": true, "alias": session.Auth.Alias, "account_id": session.Auth.ID, "message": "该链接已经用过了"}, nil
	}
	if navigateURI == "" {
		navigateURI = session.NavigateURI
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	account, err := a.exchangeOAuthCode(ctx, provider, code, state, navigateURI)
	if err != nil {
		a.finishLogin(session.State, nil, err)
		return nil, err
	}
	registered := a.registerLoginAccount(account, "", "login")
	a.finishLogin(session.State, registered, nil)
	a.log.info("login", "粘贴回调完成授权："+registered.Alias)
	return map[string]any{"ok": true, "alias": registered.Alias, "account_id": registered.ID, "message": "登录成功，账号已加入账号池"}, nil
}

func (a *app) startLogin(raw []byte) ([]byte, error) {
	if _, err := decodeRequest[authLoginStartRequest](raw); err != nil {
		return nil, err
	}
	return errorEnvelope("login_requires_captcha",
		"AutoClaw 授权需要先完成安全验证码：请到 CPA 的「AutoClaw2API」控制台 → 概览 → 新增账号，在页面里点击验证码生成授权链接"), nil
}

func (a *app) pollLogin(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authLoginPollRequest](raw)
	if err != nil {
		return nil, err
	}
	session := a.loginSession(req.State)
	if session == nil {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: "登录会话不存在或已过期"})
	}
	if session.Auth != nil {
		return okEnvelope(authLoginPollResponse{
			Status:  "success",
			Message: "登录成功",
			Auth:    a.authRecord(session.Auth, session.Auth.ID+".json"),
		})
	}
	if session.Err != "" {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: session.Err})
	}
	if time.Now().After(session.Expires) {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: "授权链接已过期，请重新发起"})
	}
	return okEnvelope(authLoginPollResponse{Status: "pending", Message: "等待在浏览器里完成授权"})
}

// --- phone login (console only) --------------------------------------------

func (a *app) sendLoginCode(ctx context.Context, phone string) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return fmt.Errorf("手机号不能为空")
	}
	cfg := a.currentConfig()
	raw, err := a.postIdentity(ctx, "/userapi/v1/agent-send-code", map[string]any{
		"phone":     phone,
		"source_id": "autoclaw",
		"device_id": a.deviceID(cfg),
	})
	if err != nil {
		return fmt.Errorf("发送验证码失败：%w", err)
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Result bool `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("发送验证码响应不是 JSON")
	}
	if envelope.Code != 0 || !envelope.Data.Result {
		return fmt.Errorf("发送验证码失败：%s", firstNonEmpty(envelope.Msg, "未知错误"))
	}
	return nil
}

func (a *app) completePhoneLogin(ctx context.Context, phone, code, alias string) (*credential, error) {
	phone = strings.TrimSpace(phone)
	code = strings.TrimSpace(code)
	if phone == "" || code == "" {
		return nil, fmt.Errorf("手机号和验证码都不能为空")
	}
	codeNumber, err := strconv.Atoi(code)
	if err != nil {
		return nil, fmt.Errorf("验证码必须是数字")
	}
	cfg := a.currentConfig()
	raw, err := a.postIdentity(ctx, "/userapi/v1/agent-login", map[string]any{
		"phone":     phone,
		"code":      codeNumber,
		"source_id": "autoclaw",
		"device_id": a.deviceID(cfg),
	})
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			UserID            json.Number     `json:"user_id"`
			UserName          string          `json:"user_name"`
			UserPhone         string          `json:"user_phone"`
			AccessToken       string          `json:"access_token"`
			RefreshToken      string          `json:"refresh_token"`
			EnterpriseID      string          `json:"enterprise_id"`
			EnterpriseName    string          `json:"enterprise_name"`
			IdentitySelection json.RawMessage `json:"identity_selection"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("登录响应不是 JSON")
	}
	if envelope.Code != 0 {
		message := firstNonEmpty(envelope.Msg, "登录失败")
		switch strconv.Itoa(envelope.Code) {
		case "630201":
			message = "验证码已过期"
		case "630202":
			message = "验证码错误"
		case "630203":
			message = "操作过于频繁，请稍后再试"
		}
		return nil, fmt.Errorf("%s", message)
	}
	if len(envelope.Data.IdentitySelection) > 0 && string(envelope.Data.IdentitySelection) != "null" {
		return nil, fmt.Errorf("该账号需要在企业身份中选择；请先在 AutoClaw 客户端登录，再导出账号 JSON 导入")
	}
	access := normalizeToken(envelope.Data.AccessToken)
	refresh := normalizeToken(envelope.Data.RefreshToken)
	if access == "" {
		return nil, fmt.Errorf("登录响应没有 access_token")
	}
	account := &credential{
		AccessToken:  access,
		RefreshToken: refresh,
		UserID:       envelope.Data.UserID.String(),
		Enterprise:   strings.TrimSpace(envelope.Data.EnterpriseID) != "",
		UpdatedAt:    time.Now().UnixMilli(),
	}
	account.normalize()
	registered := a.registerLoginAccount(account, alias, "login")
	a.log.info("login", "已通过手机号验证添加账号："+registered.Alias)
	return registered, nil
}
