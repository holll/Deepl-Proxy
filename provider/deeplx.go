package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// DeepLX 实现 Provider，向上游 DeepLX 服务转发请求，并转换响应格式。
type DeepLX struct{}

func (d *DeepLX) Name() string        { return "deeplx" }
func (d *DeepLX) SupportsUsage() bool { return false }

// errDeepLXMultiText DeepLX 接口只接受单段文本。
//
// 旧实现用 "\n" 把多段文本拼成一段发给上游，上游只返回一条译文，
// 却仍然按"DeepL 兼容格式"回给调用方——调用方以为拿到了 N 条译文。
// 这种静默降级比直接报错危险得多，所以这里改成显式拒绝；
// 服务端 Select 会通过 CanServe 把这类请求路由给 deepl 提供商。
var errDeepLXMultiText = errors.New("deeplx provider supports exactly one text per request")

// deepLXRequest DeepLX 上游请求体
type deepLXRequest struct {
	Text             string   `json:"text"`
	SourceLang       string   `json:"source_lang,omitempty"`
	TargetLang       string   `json:"target_lang"`
	TagHandling      string   `json:"tag_handling,omitempty"`
	NonSplittingTags []string `json:"non_splitting_tags,omitempty"`
	SplittingTags    []string `json:"splitting_tags,omitempty"`
	IgnoreTags       []string `json:"ignore_tags,omitempty"`
}

// deepLXResponse DeepLX 上游响应体
type deepLXResponse struct {
	Code         int      `json:"code"`
	ID           int64    `json:"id"`
	Message      string   `json:"message,omitempty"`
	Data         string   `json:"data"`
	Alternatives []string `json:"alternatives"`
	SourceLang   string   `json:"source_lang"`
	TargetLang   string   `json:"target_lang"`
	Method       string   `json:"method"`
}

// deepLXErrorResult 构建错误结果。
//
// 关键：不能把 resp.Header 原样挂上去。那是上游返回的完整 header map，
// 会被当作缓存头序列化进 headers_json，既臃肿又会把上游内部信息漏给调用方。
func deepLXErrorResult(statusCode int, body []byte) *TranslateResult {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json; charset=utf-8")
	return &TranslateResult{
		Body:       body,
		StatusCode: statusCode,
		Headers:    headers,
	}
}

func (d *DeepLX) Translate(ctx context.Context, endpoint, authKey string, req *TranslateRequest) (*TranslateResult, error) {
	urlStr := endpoint + "/translate"

	// DeepLX 只支持单段文本，绝不静默拼接。
	if len(req.Text) != 1 {
		return nil, errDeepLXMultiText
	}

	dlxReq := deepLXRequest{
		Text:             req.Text[0],
		SourceLang:       req.SourceLang,
		TargetLang:       strings.ToUpper(req.TargetLang),
		TagHandling:      req.TagHandling,
		NonSplittingTags: req.NonSplittingTags,
		SplittingTags:    req.SplittingTags,
		IgnoreTags:       req.IgnoreTags,
	}
	jsonBody, err := json.Marshal(dlxReq)
	if err != nil {
		return nil, fmt.Errorf("deeplx marshal: %w", err)
	}

	resp, err := doPost(ctx, urlStr, "", "application/json", jsonBody, nil)
	if err != nil {
		return nil, fmt.Errorf("deeplx request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("deeplx read: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return deepLXErrorResult(resp.StatusCode, body), fmt.Errorf("deeplx upstream status %d", resp.StatusCode)
	}

	// 解析 DeepLX 响应
	var dlxResp deepLXResponse
	if err := json.Unmarshal(body, &dlxResp); err != nil {
		return deepLXErrorResult(resp.StatusCode, body), fmt.Errorf("deeplx parse: %w", err)
	}

	if dlxResp.Code != 200 {
		return deepLXErrorResult(resp.StatusCode, body), fmt.Errorf("deeplx error code %d: %s", dlxResp.Code, dlxResp.Message)
	}

	// 转换为 DeepL 兼容格式
	sourceLang := dlxResp.SourceLang
	if sourceLang == "" {
		sourceLang = req.SourceLang
	}
	compatBody := deepLCompatTranslate(dlxResp.Data, sourceLang)

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("X-Upstream-Key-Name", dlxResp.Method)

	return &TranslateResult{
		Body:       compatBody,
		StatusCode: http.StatusOK,
		Headers:    headers,
	}, nil
}

func (d *DeepLX) FetchUsage(ctx context.Context, endpoint, authKey string) (*UsageInfo, error) {
	return &UsageInfo{
		Ok:     false,
		Status: 501,
		Text:   "Usage API not supported for deeplx provider",
	}, fmt.Errorf("usage not supported for deeplx")
}
