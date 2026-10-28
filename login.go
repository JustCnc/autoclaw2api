package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type loginSession struct {
	State   string
	Created time.Time
	Expires time.Time
	Auth    *credential
	Err     string
}

func (a *app) newLoginSession() *loginSession {
	cfg := a.currentConfig()
	now := time.Now()
	session := &loginSession{
		State:   randomHex(32),
		Created: now,
		Expires: now.Add(time.Duration(cfg.LoginTimeoutSeconds) * time.Second),
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

func (a *app) loginPageURL(state string) string {
	port := a.loginPort
	if port <= 0 {
		port = a.currentConfig().LoginPort
	}
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/login?state=%s", port, state)
}

func (a *app) startLogin(raw []byte) ([]byte, error) {
	if _, err := decodeRequest[authLoginStartRequest](raw); err != nil {
		return nil, err
	}
	session := a.newLoginSession()
	url := a.loginPageURL(session.State)
	if url == "" {
		return errorEnvelope("login_unavailable", "授权登录页未启动：请检查 login_port 是否被占用"), nil
	}
	a.log.info("login", "已生成授权登录页："+url)
	return okEnvelope(authLoginStartResponse{
		Provider:  providerKey,
		URL:       url,
		State:     session.State,
		ExpiresAt: session.Expires,
	})
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
		return okEnvelope(authLoginPollResponse{Status: "error", Message: "授权页已过期，请重新发起"})
	}
	return okEnvelope(authLoginPollResponse{Status: "pending", Message: "等待在授权页完成手机号验证"})
}

func (a *app) sendLoginCode(ctx context.Context, phone string) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return fmt.Errorf("手机号不能为空")
	}
	cfg := a.currentConfig()
	body, _ := json.Marshal(map[string]any{
		"phone":     phone,
		"source_id": "autoclaw",
		"device_id": a.deviceID(cfg),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AuthBaseURL+"/userapi/v1/agent-send-code", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header = a.loginHeaders(cfg)
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("发送验证码失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var envelope struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
		Data struct {
			Result bool `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("发送验证码响应不是 JSON")
	}
	if envelope.Code.String() != "0" || !envelope.Data.Result {
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
	body, _ := json.Marshal(map[string]any{
		"phone":     phone,
		"code":      codeNumber,
		"source_id": "autoclaw",
		"device_id": a.deviceID(cfg),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AuthBaseURL+"/userapi/v1/agent-login", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header = a.loginHeaders(cfg)
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("登录失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var envelope struct {
		Code json.Number `json:"code"`
		Msg  string      `json:"msg"`
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
	if envelope.Code.String() != "0" {
		message := firstNonEmpty(envelope.Msg, "登录失败")
		switch envelope.Code.String() {
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
		DeviceID:     a.deviceID(cfg),
		UserID:       envelope.Data.UserID.String(),
		Enterprise:   strings.TrimSpace(envelope.Data.EnterpriseID) != "",
		Source:       "login",
		Priority:     cfg.CredentialPriority,
		UpdatedAt:    time.Now().UnixMilli(),
	}
	account.normalize()
	alias = sanitizeAlias(firstNonEmpty(alias, account.UserID, strings.TrimSuffix(strings.TrimPrefix(phone, "+"), "")))
	if alias == "" {
		alias = "login-" + randomHex(3)
	}
	account.ID = accountIDForAlias(alias)
	account.Alias = alias
	a.upsertAccount(account)
	a.publishAccount(a.poolAccount(account.ID), true)
	a.log.info("login", "已通过手机号验证添加账号："+alias)
	return account, nil
}

// startLoginServer serves the local authorization page. It is deliberately
// loopback-only; the page itself is the only writable surface.
func (a *app) startLoginServer() {
	cfg := a.currentConfig()
	port := cfg.LoginPort
	if port <= 0 {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", a.handleLoginPage)
	mux.HandleFunc("/api/send-code", a.handleSendCode)
	mux.HandleFunc("/api/login", a.handleLoginSubmit)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	bind := strings.TrimSpace(cfg.LoginBind)
	if bind == "" {
		bind = "127.0.0.1"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		a.log.warn("login", fmt.Sprintf("授权登录页端口 %d 不可用：%s", port, err.Error()))
		return
	}
	a.loginMu.Lock()
	a.loginServer = server
	a.loginPort = port
	a.loginBindAddr = bind
	a.loginMu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			a.log.warn("login", "授权登录页已停止："+err.Error())
		}
	}()
	a.log.info("login", fmt.Sprintf("授权登录页已监听 %s:%d", bind, port))
}

// ensureLoginServer keeps one listener alive across plugin reconfigure calls.
// CPA registers and reconfigures the plugin several times per start; tearing the
// page down each time drops in-flight logins and spams the log.
func (a *app) ensureLoginServer() {
	cfg := a.currentConfig()
	port := cfg.LoginPort
	if port <= 0 {
		a.stopLoginServer()
		return
	}
	bind := cfg.loginBind()
	a.loginMu.Lock()
	same := a.loginServer != nil && a.loginPort == port && a.loginBindAddr == bind
	a.loginMu.Unlock()
	if same {
		return
	}
	a.stopLoginServer()
	a.startLoginServer()
}

func (a *app) stopLoginServer() {
	a.loginMu.Lock()
	server := a.loginServer
	a.loginServer = nil
	a.loginPort = 0
	a.loginBindAddr = ""
	a.loginMu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Shutdown(ctx)
		cancel()
	}
}

func validLoginState(state string) bool {
	if len(state) != 64 {
		return false
	}
	for _, r := range state {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func (a *app) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if !validLoginState(state) {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, strings.ReplaceAll(loginPageHTML, "__STATE__", state))
}

func (a *app) handleSendCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		State string `json:"state"`
		Phone string `json:"phone"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil {
		writeJSONError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if a.loginSession(req.State) == nil {
		writeJSONError(w, http.StatusBadRequest, "登录会话不存在或已过期")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := a.sendLoginCode(ctx, req.Phone); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		State string `json:"state"`
		Phone string `json:"phone"`
		Code  string `json:"code"`
		Alias string `json:"alias"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil {
		writeJSONError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if a.loginSession(req.State) == nil {
		writeJSONError(w, http.StatusBadRequest, "登录会话不存在或已过期")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	account, err := a.completePhoneLogin(ctx, req.Phone, req.Code, req.Alias)
	if err != nil {
		a.finishLogin(req.State, nil, err)
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.finishLogin(req.State, account, nil)
	writeJSON(w, map[string]any{"ok": true, "alias": account.Alias})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": message})
}
