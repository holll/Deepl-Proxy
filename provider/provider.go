package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// TranslateResult 上游翻译结果
type TranslateResult struct {
	Body       string
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
	Translate(endpoint, authKey string, req *TranslateRequest) (*TranslateResult, error)
	FetchUsage(endpoint, authKey string) (*UsageInfo, error)
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
	if strings.Contains(strings.ToLower(endpoint), "api.deepl.com") {
		return "official"
	}
	return "deepl_pro"
}

// parseUsageResponse 解析 DeepL usage 响应
func parseUsageResponse(body string) (*UsageInfo, error) {
	var resp struct {
		CharacterCount *int64 `json:"character_count"`
		CharacterLimit *int64 `json:"character_limit"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
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
	if statusCode == 456 || strings.Contains(content, "quota") || strings.Contains(content, "limit") {
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

// doPost helper
func doPost(urlStr string, authHeader, contentType, body string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest("POST", urlStr, strings.NewReader(body))
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
	return http.DefaultClient.Do(req)
}

// doGet helper
func doGet(urlStr string, authHeader string) (*http.Response, error) {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return http.DefaultClient.Do(req)
}

// deepLCompatTranslate 将翻译结果转为 DeepL 兼容格式
func deepLCompatTranslate(text, sourceLang string) string {
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
	return string(b)
}

// buildDeepLForm 构建 DeepL form-urlencoded body
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
func readBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// truncateStr helper
func truncateStr(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// int64Ptr helper
func int64Ptr(v int64) *int64 { return &v }
