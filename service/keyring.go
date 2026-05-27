package service

import (
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"time"

	"deepl-proxy/database"
	"deepl-proxy/models"
	"deepl-proxy/provider"
)

// Keyring 管理上游 key 的轮询、熔断和用量快照。
type Keyring struct {
	db         *sql.DB
	sampleRate float64
}

func NewKeyring(db *sql.DB) *Keyring {
	return &Keyring{
		db:         db,
		sampleRate: 0.05,
	}
}

// DB returns the underlying database connection.
func (kr *Keyring) DB() *sql.DB { return kr.db }

// SetSampleRate 设置用量采样率 (0.0-1.0)
func (kr *Keyring) SetSampleRate(rate float64) {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	kr.sampleRate = rate
}

// GetActiveKeys 获取所有 active 状态的 key
func (kr *Keyring) GetActiveKeys() ([]models.DeeplKey, error) {
	keys, err := database.GetActiveKeys(kr.db)
	if err != nil {
		return nil, err
	}

	// 过滤过期的临时/月度禁用
	now := time.Now().UnixMilli()
	var active []models.DeeplKey
	for _, k := range keys {
		if k.DisabledUntil != nil && *k.DisabledUntil > 0 && *k.DisabledUntil <= now {
			// 恢复为 active
			if err := database.UpdateKey(kr.db, k.ID, nil, nil, nil, nil, strPtr("active")); err != nil {
				log.Printf("keyring: reactivate key %d failed: %v", k.ID, err)
				continue
			}
			k.Status = "active"
			k.DisabledUntil = nil
			k.DisableType = nil
		}
		if k.Status == "active" {
			active = append(active, k)
		}
	}
	return active, nil
}

// Translate 轮询 active key 进行翻译，失败时自动熔断。
func (kr *Keyring) Translate(keys []models.DeeplKey, req *provider.TranslateRequest) (*provider.TranslateResult, *models.DeeplKey, *FailureInfo, error) {
	var lastFailure *FailureInfo

	for _, k := range keys {
		prov := provider.DetectProvider(k.Provider)
		result, err := prov.Translate(k.Endpoint, k.AuthKey, req)

		if err == nil && result.StatusCode < 500 {
			// 成功
			if kr.shouldSampleUsage() && prov.SupportsUsage() {
				go kr.sampleUsage(k)
			}
			return result, &k, nil, nil
		}

		// 失败 -> 分类并熔断
		errType := "network_error"
		statusCode := 0
		body := ""
		if result != nil {
			statusCode = result.StatusCode
			body = result.Body
		}

		siteType := "deeplx"
		if k.Provider == "deepl" || k.Provider == "" {
			siteType = provider.DetectSiteType(k.Endpoint)
		}

		if err == nil {
			// HTTP 错误
			errType = provider.ClassifyError(statusCode, body, siteType)
		}

		kr.applyDisable(k, errType, statusCode, body, err)
		lastFailure = &FailureInfo{
			KeyName: k.Name,
			Status:  statusCode,
			Body:    truncateStr(body, 160),
			Err:     err,
		}
	}

	msg := "All keys unavailable"
	if lastFailure != nil {
		msg = fmt.Sprintf("All keys unavailable, last failure: %s", lastFailure.KeyName)
	}
	return nil, nil, lastFailure, fmt.Errorf("%s", msg)
}

// FailureInfo 记录最后一次上游失败的信息
type FailureInfo struct {
	KeyName string
	Status  int
	Body    string
	Err     error
}

func (kr *Keyring) applyDisable(k models.DeeplKey, errType string, statusCode int, body string, err error) {
	now := time.Now().UnixMilli()
	code := fmt.Sprintf("%d", statusCode)
	msg := body
	if err != nil {
		msg = err.Error()
	}

	switch errType {
	case "monthly":
		_ = database.DisableKeyMonthly(kr.db, k.ID, code, msg, now)
	case "temporary":
		_ = database.DisableKeyTemporary(kr.db, k.ID, code, msg, 5*60*1000, now)
	case "permanent":
		_ = database.DisableKeyPermanent(kr.db, k.ID, code, msg, now)
	default:
		// network_error
		_ = database.DisableKeyTemporary(kr.db, k.ID, code, msg, 60*1000, now)
	}
}

func (kr *Keyring) shouldSampleUsage() bool {
	return rand.Float64() < kr.sampleRate
}

func (kr *Keyring) sampleUsage(k models.DeeplKey) {
	prov := provider.DetectProvider(k.Provider)
	if !prov.SupportsUsage() {
		return
	}
	info, err := prov.FetchUsage(k.Endpoint, k.AuthKey)
	if err != nil {
		log.Printf("keyring: sample usage for key %s failed: %v", k.Name, err)
		return
	}
	if info.Ok && (info.CharacterCount != nil || info.CharacterLimit != nil) {
		now := time.Now().UnixMilli()
		if err := database.UpdateUsageSnapshot(kr.db, k.ID, info.CharacterCount, info.CharacterLimit, now); err != nil {
			log.Printf("keyring: update usage snapshot for key %s failed: %v", k.Name, err)
		}
	}
}

// RefreshUsage 手动刷新所有 active key 的用量
func (kr *Keyring) RefreshUsage() []UsageRefreshResult {
	keys, err := kr.GetActiveKeys()
	if err != nil {
		return []UsageRefreshResult{{KeyName: "", Ok: false, Error: err.Error()}}
	}

	var results []UsageRefreshResult
	for _, k := range keys {
		prov := provider.DetectProvider(k.Provider)
		if !prov.SupportsUsage() {
			results = append(results, UsageRefreshResult{
				KeyName: k.Name,
				Ok:      false,
				Skipped: "provider does not support usage",
			})
			continue
		}
		info, err := prov.FetchUsage(k.Endpoint, k.AuthKey)
		if err != nil {
			results = append(results, UsageRefreshResult{
				KeyName: k.Name,
				Ok:      false,
				Error:   err.Error(),
			})
			continue
		}
		if info.Ok && (info.CharacterCount != nil || info.CharacterLimit != nil) {
			now := time.Now().UnixMilli()
			if dbErr := database.UpdateUsageSnapshot(kr.db, k.ID, info.CharacterCount, info.CharacterLimit, now); dbErr != nil {
				results = append(results, UsageRefreshResult{KeyName: k.Name, Ok: false, Error: dbErr.Error()})
				continue
			}
		}
		data := map[string]any{}
		if info.CharacterCount != nil {
			data["character_count"] = *info.CharacterCount
		}
		if info.CharacterLimit != nil {
			data["character_limit"] = *info.CharacterLimit
		}
		results = append(results, UsageRefreshResult{
			KeyName: k.Name,
			Ok:      info.Ok,
			Status:  info.Status,
			Data:    data,
		})
	}
	return results
}

// UsageRefreshResult 用量刷新结果
type UsageRefreshResult struct {
	KeyName string         `json:"key"`
	Ok      bool           `json:"ok"`
	Skipped string         `json:"skipped,omitempty"`
	Status  int            `json:"status,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func strPtr(s string) *string { return &s }
func truncateStr(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
