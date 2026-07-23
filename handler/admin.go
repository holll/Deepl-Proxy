package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/service"

	"github.com/gin-gonic/gin"
)

// --- 管理后台会话 ---

func handleAdminLogin(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
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

func handleAdminLogout(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Set-Cookie", clearSessionCookie(c))
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"ok": true, "authenticated": false})
	}
}

func handleAdminSession(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		authenticated := verifySessionCookie(c, cfg) ||
			c.GetHeader("Authorization") == "Bearer "+cfg.Auth.AdminToken
		c.JSON(http.StatusOK, gin.H{"authenticated": authenticated})
	}
}

// --- Key CRUD ---

func handleAdminKeys(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys, err := database.GetAllKeys(kr.DB())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"keys": keys})
	}
}

func handleAdminKeyCreate(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Name     string `json:"name"`
			AuthKey  string `json:"auth_key"`
			Endpoint string `json:"endpoint"`
			Provider string `json:"provider"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid body"})
			return
		}
		if body.Name == "" || body.Endpoint == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and endpoint required"})
			return
		}
		if body.Provider == "" {
			body.Provider = "deepl"
		}

		id, err := kr.AddKey(body.Name, body.AuthKey, body.Endpoint, body.Provider)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"ok": true, "id": id})
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

		err = kr.UpdateKeyByID(
			id,
			nullableStrBody(body, "name"),
			nullableStrBody(body, "auth_key"),
			nullableStrBody(body, "endpoint"),
			nullableStrBody(body, "provider"),
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
		if err := kr.DeleteKeyByID(id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func handleUsageRefresh(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		results := kr.RefreshUsage()
		c.JSON(http.StatusOK, gin.H{"results": results})
	}
}

// --- 缓存管理 ---

func handleCacheList(kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		search := c.Query("search")
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

		entries, total, err := database.ListCacheEntries(kr.DB(), search, limit, offset)
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
		if err := database.DeleteCacheByKey(kr.WriteDB(), key); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// previewBody 从缓存 body 中提取可读预览。
// body 是 DeepL 兼容 JSON: {"translations":[{"detected_source_language":"EN","text":"..."}]}
func previewBody(body string) string {
	if body == "" {
		return "(empty)"
	}
	var v struct {
		Translations []struct {
			Text string `json:"text"`
		} `json:"translations"`
	}
	if json.Unmarshal([]byte(body), &v) == nil && len(v.Translations) > 0 {
		t := v.Translations[0].Text
		//if len(t) > 80 {
		//	return t[:80] + "..."
		//}
		return t
	}
	// fallback: raw truncate
	if len(body) > 80 {
		return body[:80] + "..."
	}
	return body
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
