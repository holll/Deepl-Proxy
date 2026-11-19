package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"deepl-proxy/service"
)

// settingsView 是 /admin/settings 的输出（cookie 密钥永不返回，只回是否已设置）。
type settingsView struct {
	AdminToken         string      `json:"admin_token"`
	GatewayToken       string      `json:"gateway_token"`
	CookieSecretSet    bool        `json:"cookie_secret_set"`
	SessionTTL         int         `json:"session_ttl"`
	CacheTTLDays       int         `json:"cache_ttl_days"`
	CleanupInterval    int         `json:"cleanup_interval"`
	CleanupBatchSize   int         `json:"cleanup_batch_size"`
	CleanupMaxRounds   int         `json:"cleanup_max_rounds"`
	LegacyKeyDisabled  bool        `json:"legacy_key_disabled"`
	SetupRequired      bool        `json:"setup_required"`
	RestartRequiredFor []string    `json:"restart_required_for"`
	Webhook            webhookView `json:"webhook"`
}

// settingsPatch 允许只提交要改的字段；nil 不改，空串恢复默认。
type settingsPatch struct {
	AdminToken        *string       `json:"admin_token"`
	GatewayToken      *string       `json:"gateway_token"`
	SessionTTL        *string       `json:"session_ttl"`
	CacheTTLDays      *string       `json:"cache_ttl_days"`
	CleanupInterval   *string       `json:"cleanup_interval"`
	CleanupBatchSize  *string       `json:"cleanup_batch_size"`
	CleanupMaxRounds  *string       `json:"cleanup_max_rounds"`
	LegacyKeyDisabled *string       `json:"legacy_key_disabled"`
	Webhook           *webhookPatch `json:"webhook"`
}

func (p settingsPatch) toMap() map[string]*string {
	return map[string]*string{
		service.SettingAdminToken:       p.AdminToken,
		service.SettingGatewayToken:     p.GatewayToken,
		service.SettingSessionTTL:       p.SessionTTL,
		service.SettingCacheTTLDays:     p.CacheTTLDays,
		service.SettingCleanupInterval:  p.CleanupInterval,
		service.SettingCleanupBatch:     p.CleanupBatchSize,
		service.SettingCleanupMaxRounds: p.CleanupMaxRounds,
		service.SettingLegacyKeyOff:     p.LegacyKeyDisabled,
	}
}

// 这些缓存参数在启动时读取，修改后必须重启进程才会生效。
func settingsRestartRequired() []string {
	return []string{"cache_ttl_days", "cleanup_interval", "cleanup_batch_size", "cleanup_max_rounds"}
}

func buildSettingsView(st *service.Settings) settingsView {
	cfg := st.Get()
	wh, _ := st.WebhookConfig()
	return settingsView{
		AdminToken:         cfg.Auth.AdminToken,
		GatewayToken:       cfg.Auth.GatewayToken,
		CookieSecretSet:    cfg.Auth.AdminCookieSecret != "",
		SessionTTL:         cfg.Auth.SessionTTL,
		CacheTTLDays:       cfg.Cache.TTLDays,
		CleanupInterval:    cfg.Cache.CleanupInterval,
		CleanupBatchSize:   cfg.Cache.CleanupBatchSize,
		CleanupMaxRounds:   cfg.Cache.CleanupMaxRounds,
		LegacyKeyDisabled:  cfg.Cache.LegacyKeyDisabled,
		SetupRequired:      st.SetupRequired(),
		RestartRequiredFor: settingsRestartRequired(),
		Webhook:            toWebhookView(wh),
	}
}

// handleAdminSettingsGet 返回当前生效设置（令牌明文返回，页面已有管理员会话）。
func handleAdminSettingsGet(st *service.Settings) gin.HandlerFunc {
	return func(c *gin.Context) {
		overrides := st.Overrides()
		delete(overrides, service.SettingServerPort)
		delete(overrides, service.SettingAllowOrigin)
		delete(overrides, service.SettingCookieSecret)
		c.JSON(http.StatusOK, gin.H{
			"settings":  buildSettingsView(st),
			"overrides": overrides,
			"env_keys":  service.WebhookEnvKeys(),
		})
	}
}

// handleAdminSettingsUpdate 保存设置；令牌、会话、Webhook 等即时生效，
// 部分缓存参数需要重启（响应里通过 restart_required_for 告知前端）。
func handleAdminSettingsUpdate(st *service.Settings) gin.HandlerFunc {
	return func(c *gin.Context) {
		var patch settingsPatch
		if err := c.ShouldBindJSON(&patch); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON"})
			return
		}
		if patch.AdminToken != nil && *patch.AdminToken == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "管理员令牌不能为空"})
			return
		}
		all := patch.toMap()
		if patch.Webhook != nil {
			for k, v := range patch.Webhook.toMap() {
				all[service.WebhookStorageKey(k)] = v
			}
		}
		if _, err := st.Update(c.Request.Context(), all); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":       true,
			"message":  "已保存",
			"settings": buildSettingsView(st),
		})
	}
}

// handleAdminSetup 首次初始化：设置管理员令牌后可立即进入控制台。
func handleAdminSetup(st *service.Settings) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			AdminToken   string `json:"admin_token"`
			GatewayToken string `json:"gateway_token"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON"})
			return
		}
		cfg, err := st.Setup(c.Request.Context(), body.AdminToken, body.GatewayToken, "")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		cookie, err := createSessionCookie(c, cfg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
			return
		}
		c.Header("Set-Cookie", cookie)
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"ok": true, "authenticated": true, "settings": buildSettingsView(st)})
	}
}

// handleAdminRotateSecret 重新生成会话签名密钥，使所有已登录会话失效。
func handleAdminRotateSecret(st *service.Settings) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, err := st.RotateCookieSecret(c.Request.Context()); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// 当前会话也必须重新登录，不在轮换响应中签发新 cookie。
		c.Header("Set-Cookie", clearSessionCookie(c))
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "会话密钥已轮换，请重新登录"})
	}
}
