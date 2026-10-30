package provider

import (
	"context"
	"fmt"
	"net/http"
)

// DeepL 实现 Provider，向上游 DeepL API（official 或 deepl-pro）转发请求。
type DeepL struct{}

func (d *DeepL) Name() string        { return "deepl" }
func (d *DeepL) SupportsUsage() bool { return true }

func (d *DeepL) Translate(ctx context.Context, endpoint, authKey string, req *TranslateRequest) (*TranslateResult, error) {
	// 注意 endpoint 末尾可能带 "/"，这里不再用 Sprintf 拼，直接字符串连接即可。
	urlStr := endpoint + "/v2/translate"
	authHeader := "DeepL-Auth-Key " + authKey

	// 使用 form-urlencoded 格式（与 DeepL 兼容）
	resp, err := doPost(ctx, urlStr, authHeader, "application/x-www-form-urlencoded", []byte(req.FormParams), nil)
	if err != nil {
		return nil, fmt.Errorf("deepl request: %w", err)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("deepl read: %w", err)
	}

	if !(resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest) {
		return &TranslateResult{
			Body:       body,
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
		}, fmt.Errorf("deepl upstream status %d", resp.StatusCode)
	}

	headers := make(http.Header)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		headers.Set("Content-Type", ct)
	} else {
		headers.Set("Content-Type", "application/json; charset=utf-8")
	}

	return &TranslateResult{
		Body:       body,
		StatusCode: resp.StatusCode,
		Headers:    headers,
	}, nil
}

func (d *DeepL) FetchUsage(ctx context.Context, endpoint, authKey string) (*UsageInfo, error) {
	urlStr := endpoint + "/v2/usage"
	authHeader := "DeepL-Auth-Key " + authKey

	resp, err := doGet(ctx, urlStr, authHeader)
	if err != nil {
		return nil, fmt.Errorf("deepl usage: %w", err)
	}
	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("deepl usage read: %w", err)
	}
	bodyStr := string(body)

	info, parseErr := parseUsageResponse(body)
	// 非 200 响应（如 403）body 通常不是 JSON，parseErr != nil 且 info == nil，
	// 不能解引用 nil，直接返回空 UsageInfo 交给上层按状态码熔断。
	if parseErr != nil || info == nil {
		return &UsageInfo{
			Ok:     resp.StatusCode == http.StatusOK,
			Status: resp.StatusCode,
			Text:   bodyStr,
		}, parseErr
	}
	return &UsageInfo{
		CharacterCount: info.CharacterCount,
		CharacterLimit: info.CharacterLimit,
		Ok:             resp.StatusCode == http.StatusOK,
		Status:         resp.StatusCode,
		Text:           bodyStr,
	}, nil
}
