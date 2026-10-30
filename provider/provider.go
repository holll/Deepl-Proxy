package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxUpstreamBodyBytes 单次上游响应体的上限。超过即报错。
// 没有这个上限时 io.ReadAll 会把任意大的响应读进内存——
// 在 1.8GB 的小内存机器上，一条异常响应就足以把进程打死。
const maxUpstreamBodyBytes = 4 << 20 // 4 MiB

var errBodyTooLarge = errors.New("upstream response body too large")

// TranslateResult 上游翻译结果
type TranslateResult struct {
	Body       []byte
	StatusCode int
	Headers    http.Header
}

// UsageInfo 用量信息
type UsageInfo struct {
	CharacterCount *int64 `json:"character_count"`
	CharacterLimit *int64 `json:"character_limit"`
	Ok             bool
	Status         int
	Text           string
}

// TranslateRequest 归一化的翻译请求
type TranslateRequest struct {
	Text       []string
	TargetLang string
	SourceLang string
	FormParams string // form-urlencoded body string (DeepL format)
	JSONBody   []byte // JSON body (DeepLX format)
	Extra      map[string]string

	// Tag handling（用于 DeepLX 提供商，DeepL 走 FormParams 已包含）
	TagHandling      string
	NonSplittingTags []string
	SplittingTags    []string
	IgnoreTags       []string
}

// Provider 上游翻译服务接口
type Provider interface {
	Name() string
	Translate(ctx context.Context, endpoint, authKey string, req *TranslateRequest) (*TranslateResult, error)
	FetchUsage(ctx context.Context, endpoint, authKey string) (*UsageInfo, error)
	SupportsUsage() bool
}

// DetectProvider 根据 provider 字段获取实例
func DetectProvider(provider string) Provider {
	switch strings.ToLower(provider) {
	case "deeplx":
		return &DeepLX{}
	default:
		return &DeepL{}
	}
}

// DetectSiteType 根据 endpoint 判断 DeepL 站点类型
func DetectSiteType(endpoint string) string {
	e := strings.ToLower(endpoint)
	if strings.Contains(e, "api.deepl.com") || strings.Contains(e, "api-free.deepl.com") {
		return "official"
	}
	return "deepl_pro"
}

// parseUsageResponse 解析 DeepL usage 响应
func parseUsageResponse(body []byte) (*UsageInfo, error) {
	var resp struct {
		CharacterCount *int64 `json:"character_count"`
		CharacterLimit *int64 `json:"character_limit"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &UsageInfo{
		CharacterCount: resp.CharacterCount,
		CharacterLimit: resp.CharacterLimit,
	}, nil
}

// ClassifyError 根据状态码和响应体分类错误类型
func ClassifyError(statusCode int, body, siteType string) string {
	content := strings.ToLower(body)
	if statusCode == 456 || strings.Contains(content, "quota exceeded") || strings.Contains(content, "character quota") {
		if siteType == "deepl_pro" || siteType == "deeplx" {
			return "permanent"
		}
		return "monthly"
	}
	switch statusCode {
	case 429, 500, 502, 503, 504:
		return "temporary"
	case 401, 403:
		return "permanent"
	}
	return "none"
}

// httpClient is the shared HTTP client for all provider calls.
// Timeout prevents goroutine leaks on slow upstream responses.
//
// 注意 Proxy 必须显式设置：一旦自定义 Transport，http.DefaultTransport 的
// 默认行为（含代理、HTTP/2、各类超时）就不再继承。
var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// doPost helper
func doPost(ctx context.Context, urlStr, authHeader, contentType string, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return httpClient.Do(req)
}

// doGet helper
func doGet(ctx context.Context, urlStr, authHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return httpClient.Do(req)
}

// deepLCompatTranslate 将翻译结果转为 DeepL 兼容格式
func deepLCompatTranslate(text, sourceLang string) []byte {
	detected := sourceLang
	if detected == "" {
		detected = "EN"
	}
	resp := map[string]any{
		"translations": []map[string]any{
			{
				"detected_source_language": detected,
				"text":                     text,
			},
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

// BuildDeepLForm  构建 DeepL form-urlencoded body
func BuildDeepLForm(texts []string, targetLang, sourceLang string, extra url.Values) string {
	form := url.Values{}
	for _, t := range texts {
		form.Add("text", t)
	}
	form.Set("target_lang", strings.ToUpper(targetLang))
	if sourceLang != "" {
		form.Set("source_lang", strings.ToUpper(sourceLang))
	}
	for k, vs := range extra {
		for _, v := range vs {
			form.Add(k, v)
		}
	}
	return form.Encode()
}

// readBody helper
//
// 用 LimitReader + bytes.Buffer 一次读成 []byte：
// 既避免 io.ReadAll 的多次扩容和 string(b) 的整段复制，
// 又给响应体加上硬上限。
func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()

	limit := int64(maxUpstreamBodyBytes)
	hint := int64(4 << 10)
	if resp.ContentLength > 0 && resp.ContentLength < limit {
		hint = resp.ContentLength
	}

	var buf bytes.Buffer
	buf.Grow(int(hint))
	n, err := io.Copy(&buf, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, errBodyTooLarge
	}
	return buf.Bytes(), nil
}
