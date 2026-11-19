package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"deepl-proxy/config"
	"deepl-proxy/service"
)

// webhookView 是 /admin/webhook 的输出结构。
//
// 字段名用 snake_case 与前端、config.yaml 保持一致；
// 不直接序列化 config.WebhookConfig，避免以后改内部字段名时把 API 一起改坏。
type webhookView struct {
	Enabled            bool   `json:"enabled"`
	URL                string `json:"url"`
	Format             string `json:"format"`
	Nickname           string `json:"nickname"`
	TitlePrefix        string `json:"title_prefix"`
	TimeoutSeconds     int    `json:"timeout_seconds"`
	MinIntervalSeconds int    `json:"min_interval_seconds"`
	NotifyKeyDisabled  bool   `json:"notify_key_disabled"`
	NotifyTemporary    bool   `json:"notify_temporary"`
	NotifyNoKeys       bool   `json:"notify_no_keys"`
}

// webhookPatch 是 PUT /admin/webhook 的补丁体：nil 表示不改，"" 表示清除覆盖。
type webhookPatch struct {
	Enabled            *string `json:"enabled"`
	URL                *string `json:"url"`
	Format             *string `json:"format"`
	Nickname           *string `json:"nickname"`
	TitlePrefix        *string `json:"title_prefix"`
	TimeoutSeconds     *string `json:"timeout_seconds"`
	MinIntervalSeconds *string `json:"min_interval_seconds"`
	NotifyKeyDisabled  *string `json:"notify_key_disabled"`
	NotifyTemporary    *string `json:"notify_temporary"`
	NotifyNoKeys       *string `json:"notify_no_keys"`
}

func (p webhookPatch) toMap() map[string]*string {
	return map[string]*string{
		service.WebhookSettingEnabled:            p.Enabled,
		service.WebhookSettingURL:                p.URL,
		service.WebhookSettingFormat:             p.Format,
		service.WebhookSettingNickname:           p.Nickname,
		service.WebhookSettingTitlePrefix:        p.TitlePrefix,
		service.WebhookSettingTimeoutSeconds:     p.TimeoutSeconds,
		service.WebhookSettingMinIntervalSeconds: p.MinIntervalSeconds,
		service.WebhookSettingNotifyKeyDisabled:  p.NotifyKeyDisabled,
		service.WebhookSettingNotifyTemporary:    p.NotifyTemporary,
		service.WebhookSettingNotifyNoKeys:       p.NotifyNoKeys,
	}
}

func toWebhookView(cfg config.WebhookConfig) webhookView {
	return webhookView{
		Enabled:            cfg.Enabled,
		URL:                cfg.URL,
		Format:             cfg.Format,
		Nickname:           cfg.Nickname,
		TitlePrefix:        cfg.TitlePrefix,
		TimeoutSeconds:     cfg.TimeoutSeconds,
		MinIntervalSeconds: cfg.MinIntervalSeconds,
		NotifyKeyDisabled:  cfg.NotifyKeyDisabled,
		NotifyTemporary:    cfg.NotifyTemporary,
		NotifyNoKeys:       cfg.NotifyNoKeys,
	}
}

func adminWebhookResponse(wm *service.WebhookManager) gin.H {
	cfg, overrides := wm.Snapshot()
	return gin.H{
		"effective":  toWebhookView(cfg),
		"overrides":  overrides,
		"configured": cfg.Configured(),
		"env_keys":   service.WebhookEnvKeys(),
	}
}

// handleAdminWebhookGet 返回生效配置 + 数据库里显式覆盖的字段。
func handleAdminWebhookGet(wm *service.WebhookManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if wm == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "通知模块未初始化"})
			return
		}
		c.JSON(http.StatusOK, adminWebhookResponse(wm))
	}
}

// handleAdminWebhookUpdate 保存补丁：写库后立即对新的投递生效。
func handleAdminWebhookUpdate(wm *service.WebhookManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if wm == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "通知模块未初始化"})
			return
		}
		var patch webhookPatch
		if err := c.ShouldBindJSON(&patch); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON"})
			return
		}
		cfg, err := wm.Update(c.Request.Context(), patch.toMap())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		resp := adminWebhookResponse(wm)
		resp["message"] = "已保存"
		resp["effective"] = toWebhookView(cfg)
		c.JSON(http.StatusOK, resp)
	}
}

// handleAdminWebhookTest 同步发一条测试通知，把失败原因直接回给页面。
func handleAdminWebhookTest(wm *service.WebhookManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if wm == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "通知模块未初始化"})
			return
		}
		// 可选：带上表单里的值先试一次，避免"保存前测不了"。
		var patch webhookPatch
		var overrides map[string]string
		if c.Request.ContentLength > 0 {
			if err := c.ShouldBindJSON(&patch); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON"})
				return
			}
			overrides = map[string]string{}
			for k, v := range patch.toMap() {
				if v != nil {
					overrides[k] = *v
				}
			}
		}

		var err error
		if len(overrides) > 0 {
			err = wm.TestWithOverrides(c.Request.Context(), overrides)
		} else {
			err = wm.Test(c.Request.Context())
		}
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "测试通知已发送"})
	}
}
