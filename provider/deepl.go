package provider

import (
	"fmt"
	"net/http"
)

// DeepL 实现 Provider，向上游 DeepL API（official 或 deepl-pro）转发请求。
type DeepL struct{}

func (d *DeepL) Name() string        { return "deepl" }
func (d *DeepL) SupportsUsage() bool { return true }

func (d *DeepL) Translate(endpoint, authKey string, req *TranslateRequest) (*TranslateResult, error) {
	urlStr := fmt.Sprintf("%s/v2/translate", endpoint)
	authHeader := fmt.Sprintf("DeepL-Auth-Key %s", authKey)

	// 使用 form-urlencoded 格式（与 DeepL 兼容）
	resp, err := doPost(urlStr, authHeader, "application/x-www-form-urlencoded", req.FormParams, nil)
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

func (d *DeepL) FetchUsage(endpoint, authKey string) (*UsageInfo, error) {
	urlStr := fmt.Sprintf("%s/v2/usage", endpoint)
	authHeader := fmt.Sprintf("DeepL-Auth-Key %s", authKey)

	resp, err := doGet(urlStr, authHeader)
	if err != nil {
		return nil, fmt.Errorf("deepl usage: %w", err)
	}
	body, err := readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("deepl usage read: %w", err)
	}

	info, parseErr := parseUsageResponse(body)
	return &UsageInfo{
		CharacterCount: info.CharacterCount,
		CharacterLimit: info.CharacterLimit,
		Ok:             resp.StatusCode == http.StatusOK,
		Status:         resp.StatusCode,
		Text:           body,
	}, parseErr
}
