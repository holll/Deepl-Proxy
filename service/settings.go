package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/database"
)

// 运行期设置的键名。全部存在 app_settings 表里，由管理后台维护；
// 这些字符串同时是 API 字段名与前端字段名，改动会丢已有配置。
const (
	SettingServerPort       = "server.port"
	SettingAllowOrigin      = "server.allow_origin"
	SettingAdminToken       = "auth.admin_token"
	SettingGatewayToken     = "auth.gateway_token"
	SettingCookieSecret     = "auth.admin_cookie_secret"
	SettingSessionTTL       = "auth.session_ttl"
	SettingCacheTTLDays     = "cache.ttl_days"
	SettingCleanupInterval  = "cache.cleanup_interval"
	SettingCleanupBatch     = "cache.cleanup_batch_size"
	SettingCleanupMaxRounds = "cache.cleanup_max_rounds"
	SettingLegacyKeyOff     = "cache.legacy_key_disabled"

	// webhook 覆盖项的存储前缀；对外 API 仍用不带前缀的 webhook.xxx 字段名。
	webhookPrefix = "webhook."

	legacyConfigFile = "config.yaml"
)

// SettingKeys 返回全部可写设置（不含数据库路径——它必须在打开数据库之前确定）。
func SettingKeys() []string {
	out := []string{
		SettingServerPort,
		SettingAllowOrigin,
		SettingAdminToken,
		SettingGatewayToken,
		SettingCookieSecret,
		SettingSessionTTL,
		SettingCacheTTLDays,
		SettingCleanupInterval,
		SettingCleanupBatch,
		SettingCleanupMaxRounds,
		SettingLegacyKeyOff,
	}
	for _, k := range WebhookSettingKeys() {
		out = append(out, webhookPrefix+k)
	}
	return out
}

// Settings 是运行期配置的唯一来源。
//
// 配置优先级：数据库 app_settings > 启动基线（内置默认值 + 启动类环境变量）。
// 任何修改都会先校验、再落库、最后原子替换内存快照，并通知订阅者热更新。
type Settings struct {
	db   *sql.DB
	base *config.Config // 启动基线，只读

	mu        sync.Mutex
	overrides map[string]string
	listeners []func(*config.Config)

	cur atomic.Pointer[config.Config]
}

// NewSettings 从数据库载入设置；首次启动时尝试把旧的 config.yaml 迁进来。
func NewSettings(ctx context.Context, db *sql.DB, base *config.Config) (*Settings, error) {
	s := &Settings{db: db, base: base}
	all, err := database.GetAppSettings(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("读取设置: %w", err)
	}
	overrides := make(map[string]string)
	for k, v := range all {
		if strings.TrimSpace(v) != "" {
			overrides[k] = v
		}
	}

	if len(overrides) == 0 {
		imported, ok, err := config.ImportLegacy(legacyConfigFile)
		switch {
		case err != nil:
			log.Printf("settings: 旧配置文件解析失败，已忽略: %v", err)
		case ok:
			log.Printf("settings: 检测到 %s，正在一次性迁移到数据库（迁移后该文件不再生效，建议删除）", legacyConfigFile)
			overrides = overridesFromConfig(imported)
			now := time.Now().UnixMilli()
			for k, v := range overrides {
				if err := database.SetAppSetting(ctx, db, k, v, now); err != nil {
					return nil, fmt.Errorf("迁移设置: %w", err)
				}
			}
			log.Printf("settings: 已迁移 %d 项配置到数据库", len(overrides))
		}
	}

	s.overrides = overrides
	cfg := resolveSettings(base, overrides)
	s.cur.Store(cfg)
	return s, nil
}

// Get 返回当前配置快照。调用方不得修改返回值。
func (s *Settings) Get() *config.Config {
	if s == nil {
		return config.Defaults()
	}
	if p := s.cur.Load(); p != nil {
		return p
	}
	return config.Defaults()
}

// SetupRequired 表示还没设置过管理员令牌，需要先走首次初始化。
func (s *Settings) SetupRequired() bool {
	return strings.TrimSpace(s.Get().Auth.AdminToken) == ""
}

// Overrides 返回数据库里显式覆盖的键值（不含默认值）。
func (s *Settings) Overrides() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.overrides))
	for k, v := range s.overrides {
		out[k] = v
	}
	return out
}

// AddListener 注册配置变更回调（用于通知器热更新）。必须在并发使用前调用。
func (s *Settings) AddListener(fn func(*config.Config)) {
	if s == nil || fn == nil {
		return
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

// Update 应用补丁：nil 表示不改，空字符串表示清除覆盖（回落到基线）。
func (s *Settings) Update(ctx context.Context, patch map[string]*string) (*config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]string, len(s.overrides))
	for k, v := range s.overrides {
		next[k] = v
	}
	allowed := make(map[string]bool, len(SettingKeys()))
	for _, k := range SettingKeys() {
		allowed[k] = true
	}

	changed := make(map[string]string)
	for k, v := range patch {
		if !allowed[k] {
			return nil, fmt.Errorf("未知配置项 %q", k)
		}
		if v == nil {
			continue
		}
		val := normalizeSetting(k, *v)
		if val == "" {
			delete(next, k)
		} else {
			next[k] = val
		}
		changed[k] = val
	}

	if len(changed) == 0 {
		return s.Get(), nil
	}
	cfg := resolveSettings(s.base, next)
	// 旧配置中的会话密钥不能阻止 Webhook 独立更新，反之亦然。
	var validateGeneral, validateWebhook bool
	for key := range changed {
		if strings.HasPrefix(key, webhookPrefix) {
			validateWebhook = true
		} else {
			validateGeneral = true
		}
	}
	if validateGeneral {
		if err := validateSettings(cfg, s.SetupRequired()); err != nil {
			return nil, err
		}
	}
	if validateWebhook {
		if err := ValidateWebhookConfig(cfg.Webhook); err != nil {
			return nil, err
		}
	}

	now := time.Now().UnixMilli()
	for k, v := range changed {
		if err := database.SetAppSetting(ctx, s.db, k, v, now); err != nil {
			return nil, fmt.Errorf("保存设置失败: %w", err)
		}
	}

	s.overrides = next
	s.cur.Store(cfg)
	s.notify(cfg)
	return cfg, nil
}

// Setup 完成首次初始化：写入管理员令牌与网关令牌，并生成会话签名密钥。
func (s *Settings) Setup(ctx context.Context, adminToken, gatewayToken, allowOrigin string) (*config.Config, error) {
	if !s.SetupRequired() {
		return nil, fmt.Errorf("已初始化，请直接登录")
	}
	adminToken = strings.TrimSpace(adminToken)
	if len(adminToken) < 8 {
		return nil, fmt.Errorf("管理员令牌至少 8 个字符")
	}
	secret, err := randomSecret()
	if err != nil {
		return nil, err
	}
	patch := map[string]*string{
		SettingAdminToken:   &adminToken,
		SettingCookieSecret: &secret,
	}
	if v := strings.TrimSpace(gatewayToken); v != "" {
		patch[SettingGatewayToken] = &v
	}
	if v := strings.TrimSpace(allowOrigin); v != "" {
		patch[SettingAllowOrigin] = &v
	}
	return s.Update(ctx, patch)
}

// RotateCookieSecret 重新生成会话签名密钥（会让所有已登录会话失效）。
func (s *Settings) RotateCookieSecret(ctx context.Context) (*config.Config, error) {
	secret, err := randomSecret()
	if err != nil {
		return nil, err
	}
	return s.Update(ctx, map[string]*string{SettingCookieSecret: &secret})
}

// GenerateAdminToken 生成 32 字符的管理员令牌，便于本地重置后直接复制使用。
func GenerateAdminToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ResetAdminToken 供本地命令行使用：强制写入新的管理员令牌并轮换会话密钥。
//
// 与 Update 的差别：这是"忘记密码"路径，不做当前令牌校验；但仍校验长度，
// 并把会话密钥一并轮换，让所有旧会话立即失效。只能通过本机 CLI 调用，
// 不暴露任何 HTTP 端点。
func (s *Settings) ResetAdminToken(ctx context.Context, token string) (*config.Config, error) {
	token = strings.TrimSpace(token)
	if len(token) < 8 {
		return nil, fmt.Errorf("管理员令牌至少 8 个字符")
	}
	secret, err := randomSecret()
	if err != nil {
		return nil, err
	}
	return s.Update(ctx, map[string]*string{
		SettingAdminToken:   &token,
		SettingCookieSecret: &secret,
	})
}

// WebhookConfig 返回当前生效的 webhook 配置与显式覆盖项。
func (s *Settings) WebhookConfig() (config.WebhookConfig, map[string]string) {
	cfg := s.Get()
	all := s.Overrides()
	ov := make(map[string]string)
	for k, v := range all {
		if strings.HasPrefix(k, webhookPrefix) {
			ov[strings.TrimPrefix(k, webhookPrefix)] = v
		}
	}
	return cfg.Webhook, ov
}

// WebhookStorageKey 把 webhook 字段名转成 app_settings 的存储键。
func WebhookStorageKey(name string) string { return webhookPrefix + name }

// UpdateWebhook 以 webhook 字段名保存补丁（供 /admin/webhook 使用）。
func (s *Settings) UpdateWebhook(ctx context.Context, patch map[string]*string) (config.WebhookConfig, error) {
	prefixed := make(map[string]*string, len(patch))
	for k, v := range patch {
		prefixed[webhookPrefix+k] = v
	}
	cfg, err := s.Update(ctx, prefixed)
	if err != nil {
		return config.WebhookConfig{}, err
	}
	return cfg.Webhook, nil
}

func (s *Settings) notify(cfg *config.Config) {
	for _, fn := range s.listeners {
		fn(cfg)
	}
}

func resolveSettings(base *config.Config, overrides map[string]string) *config.Config {
	cfg := *base // 复制结构体；内部字段都是值类型，足够安全
	get := func(k string) string {
		if v, ok := overrides[k]; ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	if v := get(SettingServerPort); v != "" {
		cfg.Server.Port = v
	}
	if v := get(SettingAllowOrigin); v != "" {
		cfg.Server.AllowOrigin = v
	}
	if v := get(SettingAdminToken); v != "" {
		cfg.Auth.AdminToken = v
	}
	if v := get(SettingGatewayToken); v != "" {
		cfg.Auth.GatewayToken = v
	}
	if v := get(SettingCookieSecret); v != "" {
		cfg.Auth.AdminCookieSecret = v
	}
	if n, err := strconv.Atoi(get(SettingSessionTTL)); err == nil && n > 0 {
		cfg.Auth.SessionTTL = n
	}
	if n, err := strconv.Atoi(get(SettingCacheTTLDays)); err == nil && n >= 0 {
		cfg.Cache.TTLDays = n
	}
	if n, err := strconv.Atoi(get(SettingCleanupInterval)); err == nil && n > 0 {
		cfg.Cache.CleanupInterval = n
	}
	if n, err := strconv.Atoi(get(SettingCleanupBatch)); err == nil && n > 0 {
		cfg.Cache.CleanupBatchSize = n
	}
	if n, err := strconv.Atoi(get(SettingCleanupMaxRounds)); err == nil && n > 0 {
		cfg.Cache.CleanupMaxRounds = n
	}
	if v := get(SettingLegacyKeyOff); v != "" {
		cfg.Cache.LegacyKeyDisabled = parseBool(v)
	}

	webhookOverrides := make(map[string]string)
	for k, v := range overrides {
		if strings.HasPrefix(k, webhookPrefix) {
			webhookOverrides[strings.TrimPrefix(k, webhookPrefix)] = v
		}
	}
	cfg.Webhook = ResolveWebhookConfig(cfg.Webhook, webhookOverrides)
	return &cfg
}

func validateSettings(cfg *config.Config, allowEmptyAdminToken bool) error {
	if !allowEmptyAdminToken && strings.TrimSpace(cfg.Auth.AdminToken) == "" {
		return fmt.Errorf("管理员令牌不能为空")
	}
	if n, err := strconv.Atoi(cfg.Server.Port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("端口必须是 1–65535 之间的数字")
	}
	if strings.TrimSpace(cfg.Server.AllowOrigin) == "" {
		return fmt.Errorf("允许来源不能为空（不做跨域限制可填 *）")
	}
	if cfg.Auth.SessionTTL < 60 || cfg.Auth.SessionTTL > 2592000 {
		return fmt.Errorf("会话有效期需在 60–2592000 秒之间")
	}
	if cfg.Cache.TTLDays < 0 || cfg.Cache.TTLDays > 3650 {
		return fmt.Errorf("缓存天数需在 0–3650 之间")
	}
	if cfg.Cache.CleanupInterval < 10 || cfg.Cache.CleanupInterval > 86400 {
		return fmt.Errorf("清理间隔需在 10–86400 秒之间")
	}
	if cfg.Cache.CleanupBatchSize < 1 || cfg.Cache.CleanupBatchSize > 10000 {
		return fmt.Errorf("单批清理条数需在 1–10000 之间")
	}
	if cfg.Cache.CleanupMaxRounds < 1 || cfg.Cache.CleanupMaxRounds > 1000 {
		return fmt.Errorf("最大清理轮数需在 1–1000 之间")
	}
	if secret := strings.TrimSpace(cfg.Auth.AdminCookieSecret); secret != "" && len(secret) < 16 {
		return fmt.Errorf("会话签名密钥长度不能少于 16")
	}
	return nil
}

// overridesFromConfig 把一份完整配置摊平成覆盖项（仅用于旧配置迁移）。
func overridesFromConfig(cfg *config.Config) map[string]string {
	webhook := func(k, v string) (string, string) { return webhookPrefix + k, v }
	out := map[string]string{
		SettingServerPort:       cfg.Server.Port,
		SettingAllowOrigin:      cfg.Server.AllowOrigin,
		SettingAdminToken:       cfg.Auth.AdminToken,
		SettingGatewayToken:     cfg.Auth.GatewayToken,
		SettingCookieSecret:     cfg.Auth.AdminCookieSecret,
		SettingSessionTTL:       strconv.Itoa(cfg.Auth.SessionTTL),
		SettingCacheTTLDays:     strconv.Itoa(cfg.Cache.TTLDays),
		SettingCleanupInterval:  strconv.Itoa(cfg.Cache.CleanupInterval),
		SettingCleanupBatch:     strconv.Itoa(cfg.Cache.CleanupBatchSize),
		SettingCleanupMaxRounds: strconv.Itoa(cfg.Cache.CleanupMaxRounds),
		SettingLegacyKeyOff:     boolString(cfg.Cache.LegacyKeyDisabled),
	}
	pairs := [][2]string{
		{WebhookSettingEnabled, boolString(cfg.Webhook.Enabled)},
		{WebhookSettingURL, cfg.Webhook.URL},
		{WebhookSettingFormat, cfg.Webhook.Format},
		{WebhookSettingNickname, cfg.Webhook.Nickname},
		{WebhookSettingTitlePrefix, cfg.Webhook.TitlePrefix},
		{WebhookSettingTimeoutSeconds, strconv.Itoa(cfg.Webhook.TimeoutSeconds)},
		{WebhookSettingMinIntervalSeconds, strconv.Itoa(cfg.Webhook.MinIntervalSeconds)},
		{WebhookSettingNotifyKeyDisabled, boolString(cfg.Webhook.NotifyKeyDisabled)},
		{WebhookSettingNotifyTemporary, boolString(cfg.Webhook.NotifyTemporary)},
		{WebhookSettingNotifyNoKeys, boolString(cfg.Webhook.NotifyNoKeys)},
	}
	for _, p := range pairs {
		k, v := webhook(p[0], p[1])
		out[k] = v
	}
	// 空值不写库，避免把"未设置"变成显式空字符串。
	for k, v := range out {
		if strings.TrimSpace(v) == "" {
			delete(out, k)
		}
	}
	return out
}

// normalizeSetting 统一存储形态：布尔存 true/false，整数去掉空白的原样。
func normalizeSetting(key, value string) string {
	v := strings.TrimSpace(value)
	switch key {
	case SettingLegacyKeyOff, webhookPrefix + WebhookSettingEnabled,
		webhookPrefix + WebhookSettingNotifyKeyDisabled,
		webhookPrefix + WebhookSettingNotifyTemporary,
		webhookPrefix + WebhookSettingNotifyNoKeys:
		if v == "" {
			return ""
		}
		return boolString(parseBool(v))
	}
	return v
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// parseBool 接受常见的布尔写法，非法值按 false 处理（后续校验会兜住）。
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机密钥失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
