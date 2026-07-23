package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/models"
	"deepl-proxy/provider"
	"deepl-proxy/service"

	"github.com/gin-gonic/gin"
)

// SetupRoutes 注册所有路由
func SetupRoutes(r *gin.Engine, kr *service.Keyring, cs *service.CacheService, cfg *config.Config) {
	// CORS
	r.Use(CORSMiddleware(cfg))

	// 静态文件 (webui) - 由 embed 提供服务
	r.GET("/", func(c *gin.Context) { c.Redirect(http.StatusFound, "/webui/") })

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "now": timeNow()})
	})

	// 管理后台 API
	admin := r.Group("/admin")
	{
		admin.POST("/login", handleAdminLogin(cfg))
		admin.POST("/logout", handleAdminLogout(cfg))
		admin.GET("/session", handleAdminSession(cfg))

		authAdmin := admin.Group("")
		authAdmin.Use(AdminAuthMiddleware(cfg))
		{
			authAdmin.GET("/keys", handleAdminKeys(kr))
			authAdmin.POST("/keys", handleAdminKeyCreate(kr))
			authAdmin.PUT("/keys/:id", handleAdminKeyUpdate(kr))
			authAdmin.DELETE("/keys/:id", handleAdminKeyDelete(kr))
			authAdmin.POST("/usage-refresh", handleUsageRefresh(kr))
			authAdmin.GET("/usage-refresh", handleUsageRefresh(kr))
			authAdmin.GET("/cache", handleCacheList(kr))
			authAdmin.DELETE("/cache", handleCacheDelete(kr))
		}
	}

	// 翻译 API（需要网关认证）
	translate := r.Group("")
	translate.Use(GatewayAuthMiddleware(cfg))
	{
		translate.POST("/v2/translate", handleTranslateCompat(cfg, kr, cs))
		translate.POST("/translate", handleTranslateCompat(cfg, kr, cs))
		translate.GET("/v2/usage", handleUsageCompat(cfg, kr))
	}
}

// --- 翻译 API ---

func handleTranslateCompat(cfg *config.Config, kr *service.Keyring, cs *service.CacheService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 解析请求（支持 JSON 和 form-urlencoded）
		texts, targetLang, sourceLang, extraParams, tagArrayParams, identity, err := parseTranslateRequest(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
			return
		}

		// 检查是否跳过缓存
		skipCache := service.IsNoCacheRequest(c.Request)

		// 尝试缓存命中
		var cacheKey string
		if !skipCache {
			cacheKey, _ = service.BuildCacheKey(identity)
			if cacheKey != "" {
				if hit, err := cs.Get(cacheKey); err == nil && hit != nil {
					writeCacheHeaders(c, hit.Headers, cs.CacheControlHeader(), "HIT")
					c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(hit.Body))
					return
				}
			}
		}

		// 获取 active keys
		keys, err := kr.GetActiveKeys()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"message": "Failed to get keys"})
			return
		}
		if len(keys) == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"message": "No available keys"})
			return
		}

		// 构建 form params（DeepL 兼容）
		formValues := url.Values{}
		for _, t := range texts {
			formValues.Add("text", t)
		}
		formValues.Set("target_lang", strings.ToUpper(targetLang))
		if sourceLang != "" {
			formValues.Set("source_lang", strings.ToUpper(sourceLang))
		}
		for k, v := range extraParams {
			formValues.Set(k, v)
		}
		// 添加数组型 tag 参数（如 non_splitting_tags=S1&non_splitting_tags=S2）
		for k, vs := range tagArrayParams {
			for _, v := range vs {
				formValues.Add(k, v)
			}
		}

		// 构建 provider request
		req := &provider.TranslateRequest{
			Text:             texts,
			TargetLang:       targetLang,
			SourceLang:       sourceLang,
			FormParams:       formValues.Encode(),
			TagHandling:      extraParams["tag_handling"],
			NonSplittingTags: tagArrayParams["non_splitting_tags"],
			SplittingTags:    tagArrayParams["splitting_tags"],
			IgnoreTags:       tagArrayParams["ignore_tags"],
		}

		// 轮询 key
		result, usedKey, failure, translateErr := kr.Translate(keys, req)
		if translateErr != nil && result == nil {
			msg := "All keys unavailable"
			if failure != nil {
				msg = "All keys unavailable, last failure: " + failure.KeyName
			}
			c.JSON(http.StatusBadGateway, gin.H{"message": msg, "lastFailure": failure})
			return
		}

		// 构建响应
		respHeaders := make(http.Header)
		if result != nil {
			for k, vs := range result.Headers {
				for _, v := range vs {
					respHeaders.Add(k, v)
				}
			}
		}
		if usedKey != nil {
			respHeaders.Set("X-Upstream-Key-Name", usedKey.Name)
			siteType := provider.DetectSiteType(usedKey.Endpoint)
			if usedKey.Provider == "deeplx" {
				siteType = "deeplx"
			}
			respHeaders.Set("X-Key-Site-Type", siteType)
		}

		// 缓存成功响应
		if cacheKey != "" && result.StatusCode == http.StatusOK {
			cs.Put(cacheKey, result.Body, respHeaders)
		}

		writeCacheHeaders(c, respHeaders, cs.CacheControlHeader(), "MISS")

		if result.StatusCode != http.StatusOK {
			c.JSON(result.StatusCode, json.RawMessage(result.Body))
			return
		}
		c.Data(result.StatusCode, "application/json; charset=utf-8", []byte(result.Body))
	}
}

func parseTranslateRequest(c *gin.Context) (texts []string, targetLang, sourceLang string, extraParams map[string]string, tagArrayParams map[string][]string, identity *models.CacheIdentity, err error) {
	extraParams = make(map[string]string)
	tagArrayParams = make(map[string][]string)

	ct := c.GetHeader("Content-Type")

	if strings.Contains(ct, "application/json") {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			return nil, "", "", nil, nil, nil, err
		}

		// 解析 text
		switch v := body["text"].(type) {
		case string:
			texts = []string{v}
		case []any:
			for _, item := range v {
				texts = append(texts, toString(item))
			}
		default:
			return nil, "", "", nil, nil, nil, fmtStr("text is required")
		}
		if len(texts) == 0 {
			return nil, "", "", nil, nil, nil, fmtStr("text is required")
		}

		tl, ok := body["target_lang"].(string)
		if !ok || tl == "" {
			return nil, "", "", nil, nil, nil, fmtStr("target_lang is required")
		}
		targetLang = tl

		if sl, ok := body["source_lang"].(string); ok {
			sourceLang = sl
		}

		// 提取 extra params（标量）
		extraFields := []string{"formality", "glossary_id", "context", "model_type", "split_sentences", "preserve_formatting", "tag_handling", "outline_detection"}
		for _, f := range extraFields {
			if v, ok := body[f].(string); ok {
				extraParams[f] = v
			}
		}

		// 提取数组型 tag 参数
		tagArrayFields := []string{"non_splitting_tags", "splitting_tags", "ignore_tags"}
		for _, f := range tagArrayFields {
			if arr, ok := body[f].([]any); ok {
				for _, item := range arr {
					if s, ok := item.(string); ok {
						tagArrayParams[f] = append(tagArrayParams[f], s)
					}
				}
			}
		}

		identity = normalizeJSONIdentity(body)
	} else {
		// form-urlencoded
		if err := c.Request.ParseForm(); err != nil {
			return nil, "", "", nil, nil, nil, err
		}
		form := c.Request.PostForm

		texts = form["text"]
		if len(texts) == 0 {
			return nil, "", "", nil, nil, nil, fmtStr("text is required")
		}

		targetLang = form.Get("target_lang")
		if targetLang == "" {
			return nil, "", "", nil, nil, nil, fmtStr("target_lang is required")
		}

		sourceLang = form.Get("source_lang")

		extraFields := []string{"formality", "glossary_id", "context", "model_type", "split_sentences", "preserve_formatting", "tag_handling", "outline_detection"}
		for _, f := range extraFields {
			if v := form.Get(f); v != "" {
				extraParams[f] = v
			}
		}

		// 提取数组型 tag 参数（form 中为重复字段）
		tagArrayFields := []string{"non_splitting_tags", "splitting_tags", "ignore_tags"}
		for _, f := range tagArrayFields {
			if vs := form[f]; len(vs) > 0 {
				tagArrayParams[f] = vs
			}
		}

		identity = normalizeFormIdentity(form)
	}

	return
}

func normalizeJSONIdentity(body map[string]any) *models.CacheIdentity {
	identity := &models.CacheIdentity{
		TargetLang: upperOrNull(getStr(body, "target_lang")),
		SourceLang: upperOrNull(getStr(body, "source_lang")),
	}
	switch v := body["text"].(type) {
	case string:
		identity.Text = []string{v}
	case []any:
		for _, item := range v {
			identity.Text = append(identity.Text, toString(item))
		}
	}
	identity.Formality = getStr(body, "formality")
	identity.GlossaryID = getStr(body, "glossary_id")
	identity.Context = getStr(body, "context")
	identity.ModelType = getStr(body, "model_type")
	identity.SplitSentences = getStr(body, "split_sentences")
	identity.PreserveFormatting = getStr(body, "preserve_formatting")
	identity.TagHandling = getStr(body, "tag_handling")
	identity.OutlineDetection = getStr(body, "outline_detection")
	identity.NonSplittingTags = getStrArr(body, "non_splitting_tags")
	identity.SplittingTags = getStrArr(body, "splitting_tags")
	identity.IgnoreTags = getStrArr(body, "ignore_tags")
	return identity
}

func normalizeFormIdentity(form url.Values) *models.CacheIdentity {
	return &models.CacheIdentity{
		Text:               form["text"],
		TargetLang:         strings.ToUpper(form.Get("target_lang")),
		SourceLang:         upperOrNull(form.Get("source_lang")),
		Formality:          form.Get("formality"),
		GlossaryID:         form.Get("glossary_id"),
		Context:            form.Get("context"),
		ModelType:          form.Get("model_type"),
		SplitSentences:     form.Get("split_sentences"),
		PreserveFormatting: form.Get("preserve_formatting"),
		TagHandling:        form.Get("tag_handling"),
		OutlineDetection:   form.Get("outline_detection"),
		NonSplittingTags:   form["non_splitting_tags"],
		SplittingTags:      form["splitting_tags"],
		IgnoreTags:         form["ignore_tags"],
	}
}

// --- Usage API ---

func handleUsageCompat(cfg *config.Config, kr *service.Keyring) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys, err := kr.GetActiveKeys()
		if err != nil || len(keys) == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"message": "No available keys"})
			return
		}

		// 过滤非官方 key
		var nonOfficial []models.DeeplKey
		for _, k := range keys {
			if k.Provider == "deeplx" {
				continue
			}
			if provider.DetectSiteType(k.Endpoint) != "official" {
				nonOfficial = append(nonOfficial, k)
			}
		}
		if len(nonOfficial) == 0 {
			c.JSON(http.StatusNotImplemented, gin.H{"message": "Usage API not supported for official site keys"})
			return
		}

		for _, k := range nonOfficial {
			info, err := provider.DetectProvider(k.Provider).FetchUsage(k.Endpoint, k.AuthKey)
			if err == nil && info.Ok {
				resp := gin.H{}
				if info.CharacterCount != nil {
					resp["character_count"] = *info.CharacterCount
				}
				if info.CharacterLimit != nil {
					resp["character_limit"] = *info.CharacterLimit
				}
				c.JSON(http.StatusOK, resp)
				return
			}
		}
		c.JSON(http.StatusBadGateway, gin.H{"message": "Usage query failed"})
	}
}

// --- helpers ---

func writeCacheHeaders(c *gin.Context, headers http.Header, cacheControl, cacheStatus string) {
	c.Header("X-Cache-Status", cacheStatus)
	c.Header("Cache-Control", cacheControl)
	for k, vs := range headers {
		kl := strings.ToLower(k)
		if kl == "x-upstream-key-name" || kl == "x-key-site-type" {
			for _, v := range vs {
				c.Header(k, v)
			}
		}
	}
}

func timeNow() string { return time.Now().UTC().Format(time.RFC3339) }

func fmtStr(s string) error { return &strError{s: s} }

type strError struct{ s string }

func (e *strError) Error() string { return e.s }

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func getStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func getStrArr(m map[string]any, key string) []string {
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func upperOrNull(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s)
}
