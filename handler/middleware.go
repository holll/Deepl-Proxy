package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"deepl-proxy/config"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	reset  = "\033[0m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

func statusColor(status int) string {
	switch {
	case status >= 500:
		return red
	case status >= 400:
		return yellow
	case status >= 200:
		return green
	default:
		return cyan
	}
}

func latencyColor(d time.Duration) string {
	switch {
	case d > time.Second:
		return red
	case d > 200*time.Millisecond:
		return yellow
	default:
		return green
	}
}

func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// ===== request-id（优先复用，没有就生成）=====
		reqID := sanitizeRequestID(c.GetHeader("X-Request-ID"))
		if reqID == "" {
			reqID = c.GetString("request_id")
			if reqID == "" {
				reqID = uuid.NewString()
			}
		}

		// 写回 header，方便前后端/链路追踪
		c.Writer.Header().Set("X-Request-ID", reqID)
		c.Set("request_id", reqID)

		c.Next()

		now := time.Now()
		cost := now.Sub(start).Truncate(time.Microsecond)
		status := c.Writer.Status()

		cacheStatus := c.Writer.Header().Get("X-Cache-Status")
		cacheTag := ""
		if cacheStatus != "" {
			cacheTag = " | " + cacheStatus
		}

		fmt.Printf(
			"%s | %s | %s %s | %s%d%s | %s%v%s | %s%s\n",
			now.Format("15:04:05"),
			shortID(reqID),
			c.Request.Method,
			c.Request.URL.Path,

			statusColor(status),
			status,
			reset,

			latencyColor(cost),
			cost,
			reset,

			c.ClientIP(),
			cacheTag,
		)
	}
}

// CORSMiddleware 处理 CORS
func CORSMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		allowOrigin := cfg.Server.AllowOrigin

		if allowOrigin == "*" {
			c.Header("Access-Control-Allow-Origin", "*")
		} else if origin == allowOrigin {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
		}

		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,x-no-cache")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// GatewayAuthMiddleware 验证网关 GATEWAY_TOKEN
func GatewayAuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg.Auth.GatewayToken == "" {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"message": "GATEWAY_TOKEN is not configured"})
			return
		}

		auth := c.GetHeader("Authorization")
		if auth == "Bearer "+cfg.Auth.GatewayToken || auth == "DeepL-Auth-Key "+cfg.Auth.GatewayToken {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"message": "Authorization failed"})
	}
}

// AdminAuthMiddleware 验证管理员身份（支持 Bearer token + Cookie session）
func AdminAuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg.Auth.AdminToken == "" {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "ADMIN_TOKEN is not configured"})
			return
		}

		// 1. 尝试 Bearer token
		auth := c.GetHeader("Authorization")
		if auth == "Bearer "+cfg.Auth.AdminToken {
			c.Next()
			return
		}

		// 2. 尝试 Cookie session
		if verifySessionCookie(c, cfg) {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
	}
}

// --- 管理后台会话 ---

const sessionCookieName = "admin_session"

func createSessionCookie(c *gin.Context, cfg *config.Config) (string, error) {
	ttl := cfg.Auth.SessionTTL
	if ttl <= 0 {
		ttl = 43200
	}
	now := time.Now().UnixMilli()
	payload := map[string]any{"iat": now, "exp": now + int64(ttl)*1000}
	payloadJSON, _ := json.Marshal(payload)

	b64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sig, err := signPayload(b64, cfg)
	if err != nil {
		return "", err
	}
	value := b64 + "." + sig

	// 根据入站协议决定 Secure 标志（支持 nginx HTTPS 终端 → HTTP 后端）
	proto := c.GetHeader("X-Forwarded-Proto")
	secure := ""
	if proto == "https" || c.Request.TLS != nil {
		secure = "; Secure"
	}
	cookie := fmt.Sprintf("%s=%s; Path=/; HttpOnly%s; SameSite=Lax; Max-Age=%d", sessionCookieName, value, secure, ttl)
	return cookie, nil
}

func clearSessionCookie(c *gin.Context) string {
	proto := c.GetHeader("X-Forwarded-Proto")
	secure := ""
	if proto == "https" || c.Request.TLS != nil {
		secure = "; Secure"
	}
	return fmt.Sprintf("%s=; Path=/; HttpOnly%s; SameSite=Lax; Max-Age=0", sessionCookieName, secure)
}

func verifySessionCookie(c *gin.Context, cfg *config.Config) bool {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil {
		return false
	}

	idx := strings.LastIndex(cookie, ".")
	if idx <= 0 {
		return false
	}
	payload := cookie[:idx]
	sig := cookie[idx+1:]

	expected, err := signPayload(payload, cfg)
	if err != nil {
		return false
	}
	// 用常数时间比较，避免按字节泄露 HMAC 前缀（== 会在第一个不同字节处返回）。
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return false
	}

	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return false
	}

	exp, ok := data["exp"].(float64)
	if !ok {
		return false
	}
	return time.Now().UnixMilli() < int64(exp)
}

// maxRequestIDLen 限制外部传入的 X-Request-ID 长度。
// 这个值会被写进响应头和每一行访问日志，不设上限就是一个免费的
// 日志投毒 / 内存放大入口。
const maxRequestIDLen = 64

// sanitizeRequestID 去掉控制字符并截断到 maxRequestIDLen。
func sanitizeRequestID(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > maxRequestIDLen {
		s = s[:maxRequestIDLen]
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < 0x20 || ch > 0x7e {
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// shortID 取前 8 个字符用于日志前缀。
// 修复点：旧代码直接 reqID[:8]，而 X-Request-ID 完全由调用方控制，
// 传一个空串或不足 8 字节的值就会切出越界 panic（只能靠 gin.Recovery 兜底）。
func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func signPayload(payload string, cfg *config.Config) (string, error) {
	secret := cfg.Auth.AdminCookieSecret
	if secret == "" {
		secret = cfg.Auth.AdminToken
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return fmt.Sprintf("%x", mac.Sum(nil)), nil
}
