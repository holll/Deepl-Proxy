package handler

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"deepl-proxy/database"
	"deepl-proxy/models"
	"deepl-proxy/provider"
	"deepl-proxy/service"

	"github.com/gin-gonic/gin"
)

// keyTestTimeout 手动测试上游 key 的超时。比翻译主链路宽松一点，
// 但必须有上限，避免一个卡死的上游把管理接口挂住。
const keyTestTimeout = 20 * time.Second

// --- 管理后台会话 ---

func handleAdminLogin(src ConfigSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := src.Get()
		if cfg.Auth.AdminToken == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "尚未初始化管理员令牌", "setup_required": true})
			return
		}
		var body struct {
			AdminToken string `json:"admin_token"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid body"})
			return
		}
		if body.AdminToken != cfg.Auth.AdminToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			return
		}

		cookie, err := createSessionCookie(c, cfg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
			return
		}
		c.Header("Set-Cookie", cookie)
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"ok": true, "authenticated": true})
	}
}

func handleAdminLogout(src ConfigSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Set-Cookie", clearSessionCookie(c))
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"ok": true, "authenticated": false})
	}
}

func handleAdminSession(src ConfigSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := src.Get()
		authenticated := cfg.Auth.AdminToken != "" && (verifySessionCookie(c, cfg) ||
			c.GetHeader("Authorization") == "Bearer "+cfg.Auth.AdminToken)
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"authenticated":  authenticated,
			"setup_required": cfg.Auth.AdminToken == "",
		})
	}
}

// --- Key CRUD ---

// keyOut 是后台列表用的 DTO。
//
// 内嵌 models.DeeplKey 复用已有字段（auth_key 本身带 json:"-"，不会泄漏），
// 额外补三项前端要用的派生信息：auth_key 后缀提示、站点类型、配额周期。
type keyOut struct {
	models.DeeplKey
	AuthKeyHint string `json:"auth_key_hint"`
	SiteType    string `json:"site_type"`
}

func handleAdminKeys(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 用带 auth_key 的查询，只是为了取后缀做提示；对象本身不会被序列化出去。
		keys, err := database.GetAllKeysWithAuth(c.Request.Context(), kr.DB())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out := make([]keyOut, len(keys))
		for i, k := range keys {
			out[i] = keyOut{
				DeeplKey:    k,
				AuthKeyHint: authKeyHint(k.AuthKey),
				SiteType:    siteTypeOf(k),
			}
		}
		c.JSON(http.StatusOK, gin.H{"keys": out})
	}
}

// handleAdminKeySecret 仅在管理员主动查看时返回指定 Key，不把明文放进列表响应。
func handleAdminKeySecret(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 Key ID"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), database.OperationTimeout)
		defer cancel()
		key, err := database.GetKeyByID(ctx, kr.DB(), id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(http.StatusNotFound, gin.H{"error": "该 Key 不存在"})
		case err != nil:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "暂时无法读取 Key"})
		default:
			c.JSON(http.StatusOK, gin.H{"auth_key": key.AuthKey})
		}
	}
}

// authKeyHint 只暴露后缀，用来在界面上区分 free(:fx)/第三方(:dp)，
// 不暴露 key 主体的任何字符。
func authKeyHint(authKey string) string {
	switch provider.AuthKeySuffix(authKey) {
	case provider.AuthKeySuffixFree:
		return "…:fx"
	case provider.AuthKeySuffixDeepLPro:
		return "…:dp"
	}
	if strings.TrimSpace(authKey) == "" {
		return ""
	}
	return "已配置"
}

// siteTypeOf 把 endpoint 归类成前端可读的站点类型。
func siteTypeOf(k models.DeeplKey) string {
	if k.Provider == "deeplx" {
		return "deeplx"
	}
	e := strings.ToLower(strings.TrimSpace(k.Endpoint))
	switch {
	case strings.Contains(e, "api-free.deepl.com"):
		return "deepl_free"
	case strings.Contains(e, "api.deepl-pro.com"):
		return "deepl_pro"
	case strings.Contains(e, "api.deepl.com"):
		return "deepl_official"
	default:
		return "custom"
	}
}

// resolveEndpoint 根据 auth_key 后缀推断 endpoint。
// 只有 :fx / :dp 这类明确后缀才会推断，其余返回 ok=false 由调用方报错，
// 避免把任意 key 悄悄指到官方站。
func resolveEndpoint(authKey string) (endpoint, prov string, ok bool) {
	return provider.EndpointFromAuthKey(authKey)
}

// validateEndpoint 允许 http(s) 地址；空串由调用方决定是否合法。
func validateEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// normalizeQuotaPeriod 校验配额周期入参。
func normalizeQuotaPeriod(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "auto":
		return service.QuotaPeriodAuto, true
	case "monthly":
		return service.QuotaPeriodMonthly, true
	case "yearly":
		return service.QuotaPeriodYearly, true
	default:
		return "", false
	}
}

func handleAdminKeyCreate(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Name        string `json:"name"`
			AuthKey     string `json:"auth_key"`
			Endpoint    string `json:"endpoint"`
			Provider    string `json:"provider"`
			QuotaPeriod string `json:"quota_period"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid body"})
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.AuthKey = strings.TrimSpace(body.AuthKey)
		body.Endpoint = strings.TrimSpace(body.Endpoint)
		if body.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
			return
		}

		// endpoint 留空或填 auto 时，按 auth_key 后缀识别站点。
		if provider.IsAutoEndpoint(body.Endpoint) {
			ep, prov, ok := resolveEndpoint(body.AuthKey)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "无法从 auth_key 识别站点：请填写完整 endpoint，或使用以 :fx / :dp 结尾的 key"})
				return
			}
			body.Endpoint = ep
			if strings.TrimSpace(body.Provider) == "" {
				body.Provider = prov
			}
		}
		if !validateEndpoint(body.Endpoint) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint must be a http(s) URL"})
			return
		}
		// cmp.Or 取第一个非零值，等价于 "空则默认 deepl"。
		body.Provider = cmp.Or(strings.TrimSpace(body.Provider), "deepl")
		quotaPeriod, ok := normalizeQuotaPeriod(body.QuotaPeriod)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "quota_period must be auto, monthly or yearly"})
			return
		}

		id, err := kr.AddKey(c.Request.Context(), body.Name, body.AuthKey, body.Endpoint, body.Provider, quotaPeriod)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"ok": true, "id": id, "endpoint": body.Endpoint})
	}
}

func handleAdminKeyUpdate(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id"})
			return
		}

		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid body"})
			return
		}

		endpointPtr := nullableStrBody(body, "endpoint")
		authKeyPtr := nullableStrBody(body, "auth_key")
		quotaPtr := nullableStrBody(body, "quota_period")
		if quotaPtr != nil {
			q, ok := normalizeQuotaPeriod(*quotaPtr)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "quota_period must be auto, monthly or yearly"})
				return
			}
			quotaPtr = &q
		}

		// 换了 auth_key 且没显式给 endpoint（或给了 auto）时，按新后缀重新识别站点。
		if authKeyPtr != nil && (endpointPtr == nil || provider.IsAutoEndpoint(*endpointPtr)) {
			if ep, prov, ok := resolveEndpoint(*authKeyPtr); ok {
				endpointPtr = &ep
				if nullableStrBody(body, "provider") == nil {
					p := prov
					body["provider"] = p
				}
			} else if endpointPtr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "无法从 auth_key 识别站点：请填写完整 endpoint"})
				return
			}
		}
		if endpointPtr != nil && !validateEndpoint(*endpointPtr) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint must be a http(s) URL"})
			return
		}

		err = kr.UpdateKeyByID(
			c.Request.Context(),
			id,
			nullableStrBody(body, "name"),
			authKeyPtr,
			endpointPtr,
			nullableStrBody(body, "provider"),
			quotaPtr,
			nullableStrBody(body, "status"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func handleAdminKeyDelete(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id"})
			return
		}
		if err := kr.DeleteKeyByID(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// handleAdminKeyTest 用一条最小请求实测某个 key。
func handleAdminKeyTest(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), keyTestTimeout)
		defer cancel()
		res, err := kr.TestKey(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Key not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

func handleUsageRefresh(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		results := kr.RefreshUsage(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"results": results})
	}
}

// --- 缓存管理 ---

func handleCacheList(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 缓存搜索已禁用；旧客户端传入的 search 参数不再参与查询。
		limit := 50
		offset := 0
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		if v := c.Query("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}

		entries, total, err := database.ListCacheEntries(c.Request.Context(), kr.DB(), limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// 提取 body 预览（尝试解析 JSON 提取翻译文本）
		type entryOut struct {
			CacheKey  string `json:"cache_key"`
			Preview   string `json:"preview"`
			BodySize  int    `json:"body_size"`
			CreatedAt int64  `json:"created_at"`
			ExpiresAt int64  `json:"expires_at"`
			Expired   bool   `json:"expired"`
		}
		now := time.Now().UnixMilli()
		out := make([]entryOut, len(entries))
		for i, e := range entries {
			preview := previewBody(e.Body)
			out[i] = entryOut{
				CacheKey:  e.CacheKey,
				Preview:   preview,
				BodySize:  len(e.Body),
				CreatedAt: e.CreatedAt,
				ExpiresAt: e.ExpiresAt,
				Expired:   e.ExpiresAt <= now,
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"entries": out,
			"total":   total,
			"limit":   limit,
			"offset":  offset,
		})
	}
}

func handleCacheDelete(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key query param required"})
			return
		}
		if err := database.DeleteCacheByKey(c.Request.Context(), kr.WriteDB(), key); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// previewBody 从缓存 body 中提取可读预览。
// body 是 DeepL 兼容 JSON: {"translations":[{"detected_source_language":"EN","text":"..."}]}
func previewBody(body []byte) string {
	if len(body) == 0 {
		return "(empty)"
	}
	var v struct {
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	if json.Unmarshal(body, &v) == nil && len(v.Translations) > 0 {
		return v.Translations[0].Text
	}
	// fallback: raw truncate，按 rune 截断避免切碎多字节字符产生非法 UTF-8
	if r := []rune(string(body)); len(r) > 80 {
		return string(r[:80]) + "..."
	}
	return string(body)
}

// --- helpers ---

func nullableStrBody(body map[string]any, key string) *string {
	v, ok := body[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return &t
	case nil:
		return nil
	default:
		b, _ := json.Marshal(v)
		s := string(b)
		return &s
	}
}
