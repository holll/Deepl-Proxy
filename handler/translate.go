package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	//
	// flight 提到路由层共享：/v2/translate 与 /translate 是同一条业务路径，
	// 共用一份合并表才能让两个入口的并发请求也合并成一次上游调用。
	flight := service.NewFlight[upstreamOutcome]()

	translate := r.Group("")
	translate.Use(GatewayAuthMiddleware(cfg))
	{
		translate.POST("/v2/translate", handleTranslateCompat(cfg, kr, cs, flight))
		translate.POST("/translate", handleTranslateCompat(cfg, kr, cs, flight))
		translate.GET("/v2/usage", handleUsageCompat(cfg, kr))
	}
}

// --- 翻译 API ---

// upstreamOutcome 是 keyring.Translate 的返回值打包，
// 以便交给 Flight 泛型合并。
type upstreamOutcome struct {
	result  *provider.TranslateResult
	usedKey *models.DeeplKey
	failure *service.FailureInfo
	err     error
}

func handleTranslateCompat(cfg *config.Config, kr *service.Keyring, cs *service.CacheService, flight *service.Flight[upstreamOutcome]) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 解析请求（支持 JSON 和 form-urlencoded）
		parsed, err := parseTranslateRequest(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
			return
		}

		// 先决定"由哪类 provider 服务"，再算缓存键。
		//
		// 顺序很关键：缓存键里带 provider 类别，而类别取决于本次可用的 key
		// 和请求形状。先查缓存就必须先猜一个类别——这正是旧实现
		// "deeplx 的单条译文被当作 deepl 的三条译文回放"的根因。
		keys, err := kr.GetActiveKeys()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"message": "Failed to get keys"})
			return
		}
		if len(keys) == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"message": "No available keys"})
			return
		}

		sel, ok := kr.Select(keys, len(parsed.texts), parsed.extraParams)
		if !ok {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"message": "No key can serve this request: deeplx keys accept exactly one text and ignore formality/glossary_id/context/model_type/split_sentences/preserve_formatting/outline_detection",
			})
			return
		}

		// 检查是否跳过缓存
		skipCache := service.IsNoCacheRequest(c.Request)

		// 尝试缓存命中
		var cacheKey string
		if !skipCache {
			cacheKey = service.BuildCacheKey(parsed.identity, sel.ProviderClass)
			// v1 旧键只用于兼容读取：新旧键格式不兼容，否则线上已有一批旧行会全部失联。
			//
			// 但仅限"v1 能表达"的请求。v1 时代 custom_instructions /
			// tag_handling_version / show_billed_characters 是被静默丢弃的，
			// 那时的缓存行等价于"没带这些参数"的译文；现在这些参数会被真实转发，
			// 若仍用 v1 键去读，就会把忽略指令的旧译文当成带指令的结果返回。
			// 这类请求直接跳过兼容读取，多一次上游调用换取语义正确。
			var legacyKey string
			if parsed.identity.CustomInstructions == "" &&
				parsed.identity.TagHandlingVersion == "" &&
				parsed.identity.ShowBilledCharacters == "" {
				legacyKey = service.BuildLegacyCacheKey(parsed.identity)
			}
			if hit, err := cs.Get(cacheKey, legacyKey, len(parsed.texts)); err == nil && hit != nil {
				writeCacheHeaders(c, hit.Headers, cs.CacheControlHeader(), "HIT")
				// hit.Body 已经是 []byte，直接写，不再 string→[]byte 复制一次。
				c.Data(http.StatusOK, "application/json; charset=utf-8", hit.Body)
				return
			}
		}

		// 构建 form params（DeepL 兼容）
		formValues := make(url.Values, len(parsed.extraParams)+len(parsed.tagArrayParams)+3)
		for _, t := range parsed.texts {
			formValues.Add("text", t)
		}
		formValues.Set("target_lang", strings.ToUpper(parsed.targetLang))
		if parsed.sourceLang != "" {
			formValues.Set("source_lang", strings.ToUpper(parsed.sourceLang))
		}
		for k, v := range parsed.extraParams {
			formValues.Set(k, v)
		}
		// 添加数组型 tag 参数（如 non_splitting_tags=S1&non_splitting_tags=S2）
		for k, vs := range parsed.tagArrayParams {
			for _, v := range vs {
				formValues.Add(k, v)
			}
		}

		// 构建 provider request
		req := &provider.TranslateRequest{
			Text:             parsed.texts,
			TargetLang:       parsed.targetLang,
			SourceLang:       parsed.sourceLang,
			FormParams:       formValues.Encode(),
			TagHandling:      parsed.extraParams["tag_handling"],
			NonSplittingTags: parsed.tagArrayParams["non_splitting_tags"],
			SplittingTags:    parsed.tagArrayParams["splitting_tags"],
			IgnoreTags:       parsed.tagArrayParams["ignore_tags"],
		}

		// 上游调用：ctx 一路传到 http，客户端断开即取消付费的上游请求。
		callUpstream := func(ctx context.Context) upstreamOutcome {
			result, usedKey, failure, translateErr := kr.Translate(ctx, sel, req)
			return upstreamOutcome{result: result, usedKey: usedKey, failure: failure, err: translateErr}
		}

		// 缓存击穿保护：同一 cacheKey 的并发 miss 只打一次上游。
		//
		// 不用 Do 而用带 ctx 的合并，是因为等待方可能先于 leader 取消：
		// 那样它应该立刻返回 499，而不是替别人陪跑到 15 秒超时。
		// skipCache（x-no-cache）请求不参与合并——它们本来就有意绕过缓存。
		var out upstreamOutcome
		if skipCache || cacheKey == "" {
			out = callUpstream(c.Request.Context())
		} else {
			v, _, flightErr := flight.Do(c.Request.Context(), cacheKey,
				func(leaderCtx context.Context) (upstreamOutcome, error) {
					o := callUpstream(leaderCtx)
					// 只有 leader 会执行到这里，因此缓存写入天然只发生一次。
					if o.result != nil && o.result.StatusCode == http.StatusOK {
						cs.Put(cacheKey, o.result.Body, buildResponseHeaders(o.result, o.usedKey))
					}
					return o, nil
				})
			if flightErr != nil {
				// 等待期间自己的 ctx 结束：客户端已经走了，不必再写响应体。
				if c.Request.Context().Err() != nil {
					c.AbortWithStatus(499)
					return
				}
				c.JSON(http.StatusBadGateway, gin.H{"message": "Upstream call failed"})
				return
			}
			out = v
		}

		if out.result == nil {
			msg := "All keys unavailable"
			if out.failure != nil {
				msg = "All keys unavailable, last failure: " + out.failure.KeyName
			}
			if out.err != nil && c.Request.Context().Err() != nil {
				// 客户端已经走了，没必要再写响应体。
				c.AbortWithStatus(499)
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"message": msg, "lastFailure": out.failure})
			return
		}

		respHeaders := buildResponseHeaders(out.result, out.usedKey)
		writeCacheHeaders(c, respHeaders, cs.CacheControlHeader(), "MISS")

		c.Data(out.result.StatusCode, "application/json; charset=utf-8", out.result.Body)
	}
}

// buildResponseHeaders 组装回给客户端的响应头。
// 同一份结果既用于响应也用于写缓存（写缓存前会由 CacheService 过滤敏感头）。
func buildResponseHeaders(result *provider.TranslateResult, usedKey *models.DeeplKey) http.Header {
	headers := make(http.Header, len(result.Headers)+2)
	for k, vs := range result.Headers {
		for _, v := range vs {
			headers.Add(k, v)
		}
	}
	if usedKey != nil {
		headers.Set("X-Upstream-Key-Name", usedKey.Name)
		siteType := provider.DetectSiteType(usedKey.Endpoint)
		if usedKey.Provider == "deeplx" {
			siteType = "deeplx"
		}
		headers.Set("X-Key-Site-Type", siteType)
	}
	return headers
}

// translateParams 一次解析出的全部请求参数。
//
// 旧实现先解析一遍字段，再回头遍历同一个 map 生成缓存身份，
// 文本被 toString（可能 json.Marshal）两次。现在解析一次就够，
// 缓存身份直接由已解析的字段拼出来，语义完全一致但没有第二遍。
type translateParams struct {
	texts          []string
	targetLang     string
	sourceLang     string
	extraParams    map[string]string
	tagArrayParams map[string][]string
	identity       *models.CacheIdentity
}

// scalarExtraFields 会原样转发给上游、并且必须参与缓存键的标量参数。
//
// custom_instructions / tag_handling_version / show_billed_characters 此前
// 既没转发也没进键：调用方传了参数却得到一个"当作没传"的结果，还会和
// 没传该参数的请求共用同一份缓存。要么转发并进键，要么明确拒绝，不能静默丢弃。
var scalarExtraFields = []string{
	"formality", "glossary_id", "context", "model_type",
	"split_sentences", "preserve_formatting", "tag_handling", "outline_detection",
	"custom_instructions", "tag_handling_version", "show_billed_characters",
}

var tagArrayFields = []string{"non_splitting_tags", "splitting_tags", "ignore_tags"}

func parseTranslateRequest(c *gin.Context) (*translateParams, error) {
	p := &translateParams{
		extraParams:    make(map[string]string, len(scalarExtraFields)),
		tagArrayParams: make(map[string][]string, len(tagArrayFields)),
	}

	ct := c.GetHeader("Content-Type")

	if strings.Contains(ct, "application/json") {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			return nil, err
		}

		// 解析 text
		switch v := body["text"].(type) {
		case string:
			p.texts = []string{v}
		case []any:
			p.texts = make([]string, 0, len(v))
			for _, item := range v {
				p.texts = append(p.texts, toString(item))
			}
		default:
			return nil, errors.New("text is required")
		}
		if len(p.texts) == 0 {
			return nil, errors.New("text is required")
		}

		tl, ok := body["target_lang"].(string)
		if !ok || tl == "" {
			return nil, errors.New("target_lang is required")
		}
		p.targetLang = tl
		if sl, ok := body["source_lang"].(string); ok {
			p.sourceLang = sl
		}

		for _, f := range scalarExtraFields {
			if v, ok := body[f].(string); ok {
				p.extraParams[f] = v
			}
		}
		for _, f := range tagArrayFields {
			if arr, ok := body[f].([]any); ok {
				for _, item := range arr {
					if s, ok := item.(string); ok {
						p.tagArrayParams[f] = append(p.tagArrayParams[f], s)
					}
				}
			}
		}
	} else {
		// form-urlencoded
		if err := c.Request.ParseForm(); err != nil {
			return nil, err
		}
		form := c.Request.PostForm

		p.texts = form["text"]
		if len(p.texts) == 0 {
			return nil, errors.New("text is required")
		}

		p.targetLang = form.Get("target_lang")
		if p.targetLang == "" {
			return nil, errors.New("target_lang is required")
		}
		p.sourceLang = form.Get("source_lang")

		for _, f := range scalarExtraFields {
			if v := form.Get(f); v != "" {
				p.extraParams[f] = v
			}
		}
		for _, f := range tagArrayFields {
			if vs := form[f]; len(vs) > 0 {
				p.tagArrayParams[f] = vs
			}
		}
	}

	p.identity = buildIdentity(p)
	return p, nil
}

// buildIdentity 从已解析的字段直接拼出缓存身份，不再回扫原始请求。
func buildIdentity(p *translateParams) *models.CacheIdentity {
	return &models.CacheIdentity{
		Text:               p.texts,
		TargetLang:         strings.ToUpper(p.targetLang),
		SourceLang:         upperOrNull(p.sourceLang),
		Formality:          p.extraParams["formality"],
		GlossaryID:         p.extraParams["glossary_id"],
		Context:            p.extraParams["context"],
		ModelType:          p.extraParams["model_type"],
		SplitSentences:     p.extraParams["split_sentences"],
		PreserveFormatting: p.extraParams["preserve_formatting"],
		TagHandling:        p.extraParams["tag_handling"],
		OutlineDetection:   p.extraParams["outline_detection"],

		CustomInstructions:   p.extraParams["custom_instructions"],
		TagHandlingVersion:   p.extraParams["tag_handling_version"],
		ShowBilledCharacters: p.extraParams["show_billed_characters"],

		NonSplittingTags: p.tagArrayParams["non_splitting_tags"],
		SplittingTags:    p.tagArrayParams["splitting_tags"],
		IgnoreTags:       p.tagArrayParams["ignore_tags"],
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

		ctx := c.Request.Context()
		for _, k := range nonOfficial {
			info, err := provider.DetectProvider(k.Provider).FetchUsage(ctx, k.Endpoint, k.AuthKey)
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

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func upperOrNull(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s)
}
