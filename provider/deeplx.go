package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// DeepLX 实现 Provider，向上游 DeepLX 服务转发请求，并转换响应格式。
type DeepLX struct{}

func (d *DeepLX) Name() string        { return "deeplx" }
func (d *DeepLX) SupportsUsage() bool { return false }

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

func (d *DeepLX) Translate(endpoint, authKey string, req *TranslateRequest) (*TranslateResult, error) {
	urlStr := fmt.Sprintf("%s/translate", endpoint)

	// 构建 DeepLX 请求体：多文本用换行符拼接
	text := strings.Join(req.Text, "\n")
	if len(req.Text) == 1 {
		text = req.Text[0]
	}

	dlxReq := deepLXRequest{
		Text:             text,
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

	resp, err := doPost(urlStr, "", "application/json", string(jsonBody), nil)
	if err != nil {
		return nil, fmt.Errorf("deeplx request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("deeplx read: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &TranslateResult{
			Body:       body,
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
		}, fmt.Errorf("deeplx upstream status %d", resp.StatusCode)
	}

	// 解析 DeepLX 响应
	var dlxResp deepLXResponse
	if err := json.Unmarshal([]byte(body), &dlxResp); err != nil {
		return &TranslateResult{
			Body:       body,
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
		}, fmt.Errorf("deeplx parse: %w", err)
	}

	if dlxResp.Code != 200 {
		return &TranslateResult{
			Body:       body,
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
		}, fmt.Errorf("deeplx error code %d: %s", dlxResp.Code, dlxResp.Message)
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

func (d *DeepLX) FetchUsage(endpoint, authKey string) (*UsageInfo, error) {
	return &UsageInfo{
		Ok:     false,
		Status: 501,
		Text:   "Usage API not supported for deeplx provider",
	}, fmt.Errorf("usage not supported for deeplx")
}
