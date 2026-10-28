package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type app struct {
	mu      sync.Mutex
	cfg     Config
	stop    context.CancelFunc
	done    chan struct{}
	started bool
	bootAt  time.Time

	state *stateStore
	log   *logRing

	// published maps an account id to the fingerprint last written to its CPA
	// auth record, so an unchanged record is not rewritten on every tick.
	published map[string]string

	catalogMu sync.Mutex
	catalog   catalogSnapshot

	clientMu sync.Mutex
	client   *http.Client

	credMu   sync.Mutex
	accounts map[string]*credential
	refresh  map[string]*refreshFlight

	deviceMu sync.Mutex
	device   string

	limitMu sync.Mutex
	limits  map[string]*tokenBucket

	requests *requestLog

	creditMu sync.Mutex
	credits  map[string]creditCacheEntry

	billMu       sync.Mutex
	billQueue    chan billWatch
	claimedBills map[string]bool

	rewardMu    sync.Mutex
	rewardLocks map[string]*sync.Mutex

	loginMu     sync.Mutex
	logins      map[string]*loginSession
	loginCb     *http.Server
	loginCbMu   sync.Mutex
	loginCbPort int
}

var plugin = &app{log: newLogRing(600), requests: newRequestLog(300)}

func (a *app) currentConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *app) stateStore() *stateStore {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

func (a *app) configure(raw []byte) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	a.mu.Lock()
	previous := a.cfg
	a.cfg = cfg
	a.mu.Unlock()

	a.resetHTTPClient()

	a.mu.Lock()
	if a.state == nil || previous.DataDir != cfg.DataDir {
		store, errStore := newStateStore(cfg.DataDir)
		if errStore != nil {
			a.mu.Unlock()
			return fmt.Errorf("init state store: %w", errStore)
		}
		a.state = store
	}
	a.mu.Unlock()

	if a.requests == nil {
		a.requests = newRequestLog(cfg.RequestLogLimit)
	} else {
		a.requests.resize(cfg.RequestLogLimit)
	}

	a.credMu.Lock()
	loaded := len(a.accounts) > 0
	a.credMu.Unlock()
	if !loaded {
		a.loadPool()
	}

	if len(a.snapshotCatalog().Models) == 0 {
		if cached, ok := a.loadCachedCatalog(); ok {
			a.setCatalog(cached)
		}
	}
	if len(a.snapshotCatalog().Models) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if errCatalog := a.refreshCatalog(ctx); errCatalog != nil {
			a.log.warn("catalog", "首次模型发现失败："+errCatalog.Error())
		}
		cancel()
	}

	if configured := a.credentialFromConfig(cfg); configured != nil {
		a.upsertAccount(configured)
	}
	a.scanAccountsDir(cfg)
	for _, account := range a.poolSnapshot() {
		a.publishAccount(account, false)
	}
	a.ensureUniqueDevices()
	a.ensureHostRecords()
	a.ensureLoginCallback()
	a.startBackground()
	return nil
}

func (a *app) startBackground() {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	a.done = make(chan struct{})
	a.started = true
	a.bootAt = time.Now()
	done := a.done
	a.mu.Unlock()

	go a.catalogLoop(ctx)
	go a.maintenanceLoop(ctx)
	go a.rewardLoop(done)
	go func() {
		<-ctx.Done()
		close(done)
	}()
}

func (a *app) shutdown() {
	a.stopLoginCallback()
	a.mu.Lock()
	cancel := a.stop
	done := a.done
	a.stop = nil
	a.started = false
	a.done = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			a.log.warn("plugin", "后台任务未在 5 秒内退出，继续关闭")
		}
	}
	a.resetHTTPClient()
}

func (a *app) maintenanceLoop(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			interval := a.currentConfig().importInterval()
			a.maintainOnce(ctx)
			timer.Reset(interval)
		}
	}
}

func (a *app) maintainOnce(ctx context.Context) {
	cfg := a.currentConfig()
	a.scanAccountsDir(cfg)
	a.refreshStaleAccounts(ctx)
	a.ensureHostRecords()
	if snapshot := a.snapshotCatalog(); len(snapshot.Models) == 0 || snapshot.Source != "remote" {
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := a.refreshCatalog(refreshCtx); err != nil {
			a.log.warn("catalog", "模型目录刷新失败："+err.Error())
		}
		cancel()
	}
}

func (a *app) refreshStaleAccounts(ctx context.Context) {
	margin := a.currentConfig().refreshMargin()
	for _, account := range a.poolSnapshot() {
		if account.Dead || account.Disabled || !account.needsRefresh(margin) {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if _, err := a.ensureAccountFresh(refreshCtx, account); err != nil {
			a.log.warn("accounts", fmt.Sprintf("账号 %s 主动刷新失败：%s", account.Alias, err.Error()))
		}
		cancel()
	}
}

// accountDevice returns the device id this account talks to AutoClaw with. Each
// account keeps its own so a pool never looks like one machine.
func (a *app) accountDevice(cfg Config, account *credential) string {
	if id := strings.TrimSpace(cfg.DeviceID); id != "" {
		return id
	}
	if account != nil {
		if id := strings.TrimSpace(account.DeviceID); id != "" {
			return id
		}
	}
	return a.deviceID(cfg)
}

// deviceProfile is the per-account machine identity: an id plus the labels the
// console shows. AutoClaw only accepts the id upstream.
type deviceProfile struct {
	ID   string
	Name string
	OS   string
}

func newDeviceProfile() deviceProfile {
	return deviceProfile{
		ID:   newUUID(),
		Name: "DESKTOP-" + strings.ToUpper(randomHex(3)),
		OS:   "win",
	}
}

// ensureDevice gives the account its own device identity once and keeps it.
func (c *credential) ensureDevice() deviceProfile {
	profile := deviceProfile{ID: c.DeviceID, Name: c.DeviceName, OS: c.DeviceOS}
	if profile.ID == "" {
		fresh := newDeviceProfile()
		profile = fresh
		c.DeviceID = fresh.ID
	}
	if profile.Name == "" {
		profile.Name = "DESKTOP-" + strings.ToUpper(randomHex(3))
		c.DeviceName = profile.Name
	}
	if profile.OS == "" {
		profile.OS = "win"
		c.DeviceOS = profile.OS
	}
	return profile
}

func (a *app) deviceID(cfg Config) string {
	if id := strings.TrimSpace(cfg.DeviceID); id != "" {
		return id
	}
	a.deviceMu.Lock()
	defer a.deviceMu.Unlock()
	if a.device != "" {
		return a.device
	}
	if store := a.stateStore(); store != nil {
		if id := store.deviceID(); id != "" {
			a.device = id
			return id
		}
	}
	id := newUUID()
	a.device = id
	if store := a.stateStore(); store != nil {
		_ = store.saveDeviceID(id)
	}
	return id
}

func (a *app) registration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             "AutoClaw2API",
			Version:          pluginVersion,
			Author:           "JustCnc",
			GitHubRepository: "https://github.com/JustCnc/autoclaw2api",
			Logo:             "",
			ConfigFields:     pluginConfigFields(),
		},
		Capabilities: registrationCapability{
			ModelRegistrar:        true,
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{formatChat},
			ExecutorOutputFormats: []string{formatChat},
			ManagementAPI:         true,
		},
	}
}

func decodeRequest[T any](raw []byte) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode %T: %w", out, err)
	}
	return out, nil
}

func (a *app) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		lifecycle, err := decodeRequest[lifecycleRequest](request)
		if err != nil {
			return nil, err
		}
		if err := a.configure(lifecycle.ConfigYAML); err != nil {
			return errorEnvelope("config_error", err.Error()), nil
		}
		a.log.info("plugin", "插件已注册 / 配置已更新")
		return okEnvelope(a.registration())
	case methodPluginQuiesce:
		return okEnvelope(map[string]any{})
	case methodPluginShutdown:
		a.shutdown()
		a.log.info("plugin", "插件已停止")
		return okEnvelope(map[string]any{})
	case methodModelRegister, methodModelStatic, methodModelForAuth:
		return okEnvelope(modelRegistrationResponse{Provider: providerKey, Models: a.registeredModels()})
	case methodAuthIdentifier, methodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case methodAuthParse:
		return a.parseAuth(request)
	case methodAuthLoginStart:
		return a.startLogin(request)
	case methodAuthLoginPoll:
		return a.pollLogin(request)
	case methodAuthRefresh:
		return a.refreshAuth(request)
	case methodExecutorExecute:
		return a.execute(request)
	case methodExecutorExecuteStream:
		return a.executeStream(request)
	case methodExecutorCountTokens:
		return a.countTokens(request)
	case methodManagementRegister:
		return okEnvelope(a.managementRegistration())
	case methodManagementHandle:
		req, err := decodeRequest[managementRequest](request)
		if err != nil {
			return nil, err
		}
		return okEnvelope(a.handleManagement(req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// parseAuth accepts an account JSON from a CPA auth file. The format is the same
// one the export helper writes.
func (a *app) parseAuth(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authParseRequest](raw)
	if err != nil {
		return nil, err
	}
	if len(req.RawJSON) == 0 {
		return okEnvelope(authParseResponse{Handled: false})
	}
	var probe struct {
		Type         string `json:"type"`
		Provider     string `json:"provider"`
		AccessToken  string `json:"access_token"`
		AccessTokenC string `json:"accessToken"`
		Token        string `json:"token"`
	}
	if json.Unmarshal(req.RawJSON, &probe) != nil {
		return okEnvelope(authParseResponse{Handled: false})
	}
	ourProvider := strings.EqualFold(strings.TrimSpace(req.Provider), providerKey)
	declared := strings.ToLower(strings.TrimSpace(firstNonEmpty(probe.Type, probe.Provider)))
	untyped := declared == ""
	if declared != "" && !isOurProviderName(declared) {
		return okEnvelope(authParseResponse{Handled: false})
	}
	if untyped && !ourProvider {
		return okEnvelope(authParseResponse{Handled: false})
	}
	account, errDecode := decodeCredential(req.RawJSON)
	if errDecode != nil || account.AccessToken == "" || !looksLikeAutoClawToken(account.AccessToken) {
		return okEnvelope(authParseResponse{Handled: false})
	}
	alias := aliasFromFileName(firstNonEmpty(account.Alias, aliasFromFileName(req.FileName)))
	if alias == "" {
		alias = sanitizeAlias(firstNonEmpty(account.UserID, account.ID))
	}
	if alias == "" {
		alias = "auth-" + randomHex(3)
	}
	account.ID = accountIDForAlias(alias)
	account.Alias = alias
	account.Source = firstNonEmpty(account.Source, "auth-file")
	if account.Priority == 0 {
		account.Priority = a.currentConfig().CredentialPriority
	}
	account.UpdatedAt = time.Now().UnixMilli()
	a.upsertAccount(account)
	auth := a.authRecord(account, firstNonEmpty(req.FileName, account.ID+".json"))
	a.log.info("auth", "解析到账号文件："+alias)
	return okEnvelope(authParseResponse{Handled: true, Auth: auth})
}

func (a *app) refreshAuth(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authRefreshRequest](raw)
	if err != nil {
		return nil, err
	}
	account, errCred := decodeCredential(req.StorageJSON)
	if errCred != nil || account.AccessToken == "" {
		account = nil
	}
	if account != nil && account.ID == "" {
		account.ID = firstNonEmpty(normalizeAuthID(req.AuthID), a.currentConfig().credentialID())
	}
	if account == nil {
		if id := normalizeAuthID(req.AuthID); id != "" {
			account = a.poolAccount(id)
		}
	}
	if account == nil {
		return errorEnvelope("auth_error", "没有可刷新的登录态"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	next, errRefresh := a.refreshAccount(ctx, account)
	if errRefresh != nil {
		return errorEnvelope("auth_refresh_failed", errRefresh.Error()), nil
	}
	auth := a.authRecord(next, "")
	return okEnvelope(authRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter})
}

func isOurProviderName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case providerKey, "autoclaw", "autoclaw2":
		return true
	default:
		return false
	}
}
