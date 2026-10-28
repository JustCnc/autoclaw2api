package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the plugin-owned subtree of plugins.configs.autoclaw2api.
type Config struct {
	DataDir string `yaml:"data_dir"`

	// The managed model gateway is a path on the identity origin.
	BaseURL     string `yaml:"base_url"`
	AuthBaseURL string `yaml:"auth_base_url"`

	// Plain credentials (one account). accounts_dir adds more accounts.
	AccessToken        string `yaml:"access_token"`
	RefreshToken       string `yaml:"refresh_token"`
	DeviceID           string `yaml:"device_id"`
	CredentialAlias    string `yaml:"credential_alias"`
	CredentialPriority int    `yaml:"credential_priority"`
	AccountsDir        string `yaml:"accounts_dir"`
	ImportInterval     int    `yaml:"import_interval_seconds"`
	RefreshMargin      int    `yaml:"refresh_margin_seconds"`

	// Client fingerprint captured from AutoClaw 2.2.2. Keep these in sync with
	// the installed desktop build so the wire shape stays believable.
	AppID          string `yaml:"app_id"`
	AppKey         string `yaml:"app_key"`
	Product        string `yaml:"product"`
	Channel        string `yaml:"channel"`
	Lang           string `yaml:"lang"`
	ClientVersion  string `yaml:"client_version"`
	ClientPlatform string `yaml:"client_platform"`
	UserAgent      string `yaml:"user_agent"`
	// Enterprise sets X-Enterprise: 1. Personal accounts leave it off.
	Enterprise bool `yaml:"enterprise"`

	RefreshSeconds int      `yaml:"refresh_seconds"`
	ModelAllowlist []string `yaml:"model_allowlist"`
	ModelDenylist  []string `yaml:"model_denylist"`

	MaxAttempts           int    `yaml:"max_attempts"`
	CooldownSeconds       int    `yaml:"cooldown_seconds"`
	RateLimitRPM          int    `yaml:"rate_limit_rpm"`
	RateLimitBurst        int    `yaml:"rate_limit_burst"`
	RequestTimeoutSeconds int    `yaml:"request_timeout_seconds"`
	MaxTokensDefault      int    `yaml:"max_tokens_default"`
	ReasoningEffort       string `yaml:"reasoning_effort"`
	DebugDump             bool   `yaml:"debug_dump"`

	// Observability: the request ring feeds the 请求 tab, the credit cache
	// throttles balance lookups against the identity gateway.
	RequestLogLimit     int `yaml:"request_log_limit"`
	CreditsCacheSeconds int `yaml:"credits_cache_seconds"`

	// Daily rewards: replay the client's login call and claim the daily task
	// board, which is where the login/daily credits come from.
	DailyReward           bool `yaml:"daily_reward"`
	RewardIntervalMinutes int  `yaml:"reward_interval_minutes"`

	// Authorization login. The gateway hands the OAuth code back to a loopback
	// URL, so the plugin binds one of the ports the desktop client uses.
	LoginCallbackPort   int `yaml:"login_callback_port"`
	LoginTimeoutSeconds int `yaml:"login_timeout_seconds"`
}

const (
	defaultDataDir     = "plugin-data/autoclaw2api"
	defaultBaseURL     = "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw"
	defaultAuthBaseURL = "https://autoglm-api.autoglm.ai"
	defaultModelPath   = "/autoclaw-proxy/proxy/autoclaw-model-config"
	defaultAppID       = "100003"
	defaultAppKey      = "38d2391985e2369a5fb8227d8e6cd5e5"
	defaultProduct     = "autoclaw"
	defaultChannel     = "official"
	defaultLang        = "zh-CN"
	defaultVersion     = "2.2.2"
	defaultPlatform    = "win"
	defaultUserAgent   = "node"
	// The managed gateway rejects a request whose system prompt does not start
	// with this exact desktop preamble (HTTP 406 with an empty body).
	defaultPreamble = "You are AutoClaw. Answer the user directly and concisely."
	defaultModel    = "zai_glm-5.3-flash"
)

func defaultConfig() Config {
	return Config{
		DataDir:               defaultDataDir,
		BaseURL:               defaultBaseURL,
		AuthBaseURL:           defaultAuthBaseURL,
		CredentialAlias:       "default",
		CredentialPriority:    10,
		ImportInterval:        30,
		RefreshMargin:         600,
		AppID:                 defaultAppID,
		AppKey:                defaultAppKey,
		Product:               defaultProduct,
		Channel:               defaultChannel,
		Lang:                  defaultLang,
		ClientVersion:         defaultVersion,
		ClientPlatform:        defaultPlatform,
		UserAgent:             defaultUserAgent,
		RefreshSeconds:        1800,
		MaxAttempts:           3,
		CooldownSeconds:       600,
		RateLimitRPM:          30,
		RateLimitBurst:        20,
		RequestTimeoutSeconds: 600,
		MaxTokensDefault:      131072,
		RequestLogLimit:       300,
		CreditsCacheSeconds:   60,
		DailyReward:           true,
		RewardIntervalMinutes: 30,
		LoginCallbackPort:     0,
		LoginTimeoutSeconds:   600,
	}
}

func parseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("invalid plugin config: %w", err)
		}
	}
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	c.DataDir = strings.TrimSpace(c.DataDir)
	if c.DataDir == "" {
		c.DataDir = defaultDataDir
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if _, err := url.Parse(c.BaseURL); err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	c.AuthBaseURL = strings.TrimRight(strings.TrimSpace(c.AuthBaseURL), "/")
	if c.AuthBaseURL == "" {
		c.AuthBaseURL = defaultAuthBaseURL
	}
	if _, err := url.Parse(c.AuthBaseURL); err != nil {
		return fmt.Errorf("invalid auth_base_url: %w", err)
	}

	c.AccessToken = normalizeToken(c.AccessToken)
	c.RefreshToken = normalizeToken(c.RefreshToken)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	c.CredentialAlias = sanitizeAlias(c.CredentialAlias)
	if c.CredentialAlias == "" {
		c.CredentialAlias = "default"
	}
	c.AccountsDir = strings.TrimSpace(c.AccountsDir)
	if c.CredentialPriority <= 0 {
		c.CredentialPriority = 10
	}

	c.AppID = strings.TrimSpace(c.AppID)
	if c.AppID == "" {
		c.AppID = defaultAppID
	}
	c.AppKey = strings.TrimSpace(c.AppKey)
	if c.AppKey == "" {
		c.AppKey = defaultAppKey
	}
	c.Product = strings.TrimSpace(c.Product)
	if c.Product == "" {
		c.Product = defaultProduct
	}
	c.Channel = strings.TrimSpace(c.Channel)
	if c.Channel == "" {
		c.Channel = defaultChannel
	}
	c.Lang = strings.TrimSpace(c.Lang)
	if c.Lang == "" {
		c.Lang = defaultLang
	}
	c.ClientVersion = normalizeClientVersion(c.ClientVersion)
	if c.ClientVersion == "" {
		c.ClientVersion = defaultVersion
	}
	c.ClientPlatform = strings.ToLower(strings.TrimSpace(c.ClientPlatform))
	if c.ClientPlatform == "" {
		c.ClientPlatform = defaultPlatform
	}
	c.UserAgent = strings.TrimSpace(c.UserAgent)
	if c.UserAgent == "" {
		c.UserAgent = defaultUserAgent
	}

	if c.ImportInterval <= 0 {
		c.ImportInterval = 30
	}
	if c.ImportInterval > 3600 {
		c.ImportInterval = 3600
	}
	if c.RefreshMargin <= 0 {
		c.RefreshMargin = 600
	}
	if c.RefreshSeconds <= 0 {
		c.RefreshSeconds = 1800
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.MaxAttempts > 8 {
		c.MaxAttempts = 8
	}
	if c.CooldownSeconds <= 0 {
		c.CooldownSeconds = 600
	}
	if c.CooldownSeconds > 7200 {
		c.CooldownSeconds = 7200
	}
	if c.RequestLogLimit < 0 {
		c.RequestLogLimit = 0
	}
	if c.RequestLogLimit == 0 {
		c.RequestLogLimit = 300
	}
	if c.RequestLogLimit > 5000 {
		c.RequestLogLimit = 5000
	}
	if c.CreditsCacheSeconds <= 0 {
		c.CreditsCacheSeconds = 60
	}
	if c.CreditsCacheSeconds > 3600 {
		c.CreditsCacheSeconds = 3600
	}
	if c.RewardIntervalMinutes <= 0 {
		c.RewardIntervalMinutes = 30
	}
	if c.RewardIntervalMinutes > 720 {
		c.RewardIntervalMinutes = 720
	}
	if c.RequestTimeoutSeconds <= 0 {
		c.RequestTimeoutSeconds = 600
	}
	if c.MaxTokensDefault < 0 {
		c.MaxTokensDefault = 0
	}
	c.ReasoningEffort = strings.ToLower(strings.TrimSpace(c.ReasoningEffort))
	switch c.ReasoningEffort {
	case "low", "medium", "high":
	default:
		c.ReasoningEffort = ""
	}
	if c.LoginCallbackPort < 0 || c.LoginCallbackPort > 65535 {
		c.LoginCallbackPort = 0
	}
	if c.LoginTimeoutSeconds <= 0 {
		c.LoginTimeoutSeconds = 600
	}
	return nil
}

func (c Config) requestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}

func (c Config) refreshInterval() time.Duration {
	return time.Duration(c.RefreshSeconds) * time.Second
}

func (c Config) importInterval() time.Duration {
	return time.Duration(c.ImportInterval) * time.Second
}

func (c Config) refreshMargin() time.Duration {
	return time.Duration(c.RefreshMargin) * time.Second
}

func (c Config) rateLimit() (float64, int) {
	if c.RateLimitRPM <= 0 {
		return 0, 0
	}
	burst := c.RateLimitBurst
	if burst <= 0 {
		burst = c.RateLimitRPM / 2
	}
	if burst < 1 {
		burst = 1
	}
	return float64(c.RateLimitRPM), burst
}

func (c Config) cooldown() time.Duration {
	return time.Duration(c.CooldownSeconds) * time.Second
}

func (c Config) modelConfigURL() string {
	return c.AuthBaseURL + defaultModelPath
}

func (c Config) chatURL() string {
	return c.BaseURL + "/chat/completions"
}

func (c Config) credentialFileName() string {
	return providerKey + "-" + c.CredentialAlias + ".json"
}

func (c Config) credentialID() string {
	return providerKey + "-" + c.CredentialAlias
}

func sanitizeAlias(alias string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(alias)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizeClientVersion(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

func normalizeToken(token string) string {
	token = strings.TrimSpace(token)
	for strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[len("bearer "):])
	}
	return token
}

// accountsDirCandidates keeps container and local conventions in one place.
func accountsDirCandidates(configured string) []string {
	if strings.TrimSpace(configured) != "" {
		return []string{strings.TrimSpace(configured)}
	}
	out := []string{"/autoclaw-accounts"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".autoclaw2api", "accounts"))
	}
	return out
}

func pluginConfigFields() []configField {
	return []configField{
		{Name: "data_dir", Type: "string", Default: defaultDataDir, Description: "插件私有数据目录（账号池、模型缓存、设备 ID）"},
		{Name: "base_url", Type: "string", Default: defaultBaseURL, Description: "AutoClaw 托管模型网关基址"},
		{Name: "auth_base_url", Type: "string", Default: defaultAuthBaseURL, Description: "身份网关基址（模型目录 / token 刷新 / 登录）"},
		{Name: "access_token", Type: "string", Default: "", Description: "AutoClaw access token（不带 Bearer 前缀）"},
		{Name: "refresh_token", Type: "string", Default: "", Description: "AutoClaw refresh token"},
		{Name: "device_id", Type: "string", Default: "", Description: "设备 ID；留空则从账号/持久化状态取"},
		{Name: "credential_alias", Type: "string", Default: "default", Description: "配置里主账号的别名"},
		{Name: "credential_priority", Type: "integer", Default: 10, Description: "主账号优先级（数值越大越先用）"},
		{Name: "accounts_dir", Type: "string", Default: "", Description: "账号目录：每个 *.json 一个账号（文件名即别名）"},
		{Name: "import_interval_seconds", Type: "integer", Default: 30, Description: "扫描账号目录 / 对齐 CPA 凭据的间隔"},
		{Name: "refresh_margin_seconds", Type: "integer", Default: 600, Description: "token 到期前多久主动刷新"},
		{Name: "app_id", Type: "string", Default: defaultAppID, Description: "X-Auth-Appid"},
		{Name: "app_key", Type: "string", Default: defaultAppKey, Description: "X-Auth-Sign 的签名密钥"},
		{Name: "product", Type: "string", Default: defaultProduct, Description: "X-Product"},
		{Name: "channel", Type: "string", Default: defaultChannel, Description: "X-Channel"},
		{Name: "lang", Type: "string", Default: defaultLang, Description: "X-Lang"},
		{Name: "client_version", Type: "string", Default: defaultVersion, Description: "X-Version（AutoClaw 升级后同步修改）"},
		{Name: "client_platform", Type: "string", Default: defaultPlatform, Description: "X-Tm：win / mac"},
		{Name: "user_agent", Type: "string", Default: defaultUserAgent, Description: "上游 User-Agent"},
		{Name: "enterprise", Type: "boolean", Default: false, Description: "企业账号：发送 X-Enterprise: 1"},
		{Name: "refresh_seconds", Type: "integer", Default: 1800, Description: "模型目录刷新间隔"},
		{Name: "model_allowlist", Type: "array", Default: []string{}, Description: "仅上架这些模型 id（留空表示全部）"},
		{Name: "model_denylist", Type: "array", Default: []string{}, Description: "不上架的模型 id"},
		{Name: "max_attempts", Type: "integer", Default: 3, Description: "单次请求最多尝试几个账号"},
		{Name: "cooldown_seconds", Type: "integer", Default: 600, Description: "429/5xx 后账号冷却时长"},
		{Name: "rate_limit_rpm", Type: "integer", Default: 30, Description: "每个账号每分钟最多请求数（上游实测约 30 次/分钟；0 关闭本地限速）"},
		{Name: "rate_limit_burst", Type: "integer", Default: 20, Description: "每个账号允许的瞬时突发请求数（上游约 40）"},
		{Name: "request_timeout_seconds", Type: "integer", Default: 600, Description: "上游请求超时"},
		{Name: "max_tokens_default", Type: "integer", Default: 131072, Description: "客户端未给 max_tokens 时补的默认值，0 表示不补"},
		{Name: "reasoning_effort", Type: "enum", Default: "", EnumValues: []string{"", "low", "medium", "high"}, Description: "客户端未指定思考强度时的默认值"},
		{Name: "debug_dump", Type: "boolean", Default: false, Description: "日志记录出站头部与正文（排障用）"},
		{Name: "request_log_limit", Type: "integer", Default: 300, Description: "控制台「请求」页保留多少条请求记录（0 表示用默认 300）"},
		{Name: "credits_cache_seconds", Type: "integer", Default: 60, Description: "积分余额缓存时长，避免频繁查询上游"},
		{Name: "daily_reward", Type: "boolean", Default: true, Description: "后台每天自动触发登录奖励并领取每日积分任务"},
		{Name: "reward_interval_minutes", Type: "integer", Default: 30, Description: "后台检查积分的间隔（分钟）"},
		{Name: "login_callback_port", Type: "integer", Default: 0, Description: "OAuth 回调端口（0 = 自动从 18432/19654/19723/53699 里挑空闲端口）"},
		{Name: "login_timeout_seconds", Type: "integer", Default: 600, Description: "授权登录有效期"},
	}
}
