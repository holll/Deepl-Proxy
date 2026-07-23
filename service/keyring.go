package service

import (
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"deepl-proxy/database"
	"deepl-proxy/models"
	"deepl-proxy/provider"
)

// Keyring 管理上游 key 的轮询、熔断和用量快照。
type Keyring struct {
	writeDB    *sql.DB
	readDB     *sql.DB
	sampleRate float64

	// in-memory key cache
	mu         sync.RWMutex
	cachedKeys []models.DeeplKey
	dirty      bool
	cachedAt   int64 // UnixMilli — cache 创建时间，用于定期过期

	// round-robin counter
	counter atomic.Int64
}

func NewKeyring(writeDB, readDB *sql.DB) *Keyring {
	return &Keyring{
		writeDB:    writeDB,
		readDB:     readDB,
		sampleRate: 0.05,
		dirty:      true,
	}
}

// DB returns the read connection (for read-only admin queries).
func (kr *Keyring) DB() *sql.DB { return kr.readDB }

// WriteDB returns the write connection (for admin write operations).
func (kr *Keyring) WriteDB() *sql.DB { return kr.writeDB }

// invalidateKeyCache marks the in-memory key list as stale.
func (kr *Keyring) invalidateKeyCache() {
	kr.mu.Lock()
	kr.dirty = true
	kr.mu.Unlock()
}

// keyCacheTTL 内存 key 缓存最大存活时间，确保临时禁用到期后能被发现。
const keyCacheTTL int64 = 30 * 1000 // 30 秒

// GetActiveKeys returns active keys from memory cache, reloading from DB when dirty or cache expired.
func (kr *Keyring) GetActiveKeys() ([]models.DeeplKey, error) {
	now := time.Now().UnixMilli()

	kr.mu.RLock()
	if !kr.dirty && (now-kr.cachedAt) < keyCacheTTL {
		keys := make([]models.DeeplKey, len(kr.cachedKeys))
		copy(keys, kr.cachedKeys)
		kr.mu.RUnlock()
		return keys, nil
	}
	kr.mu.RUnlock()

	// upgrade to write lock for reload
	kr.mu.Lock()
	defer kr.mu.Unlock()

	// double-check
	if !kr.dirty && (now-kr.cachedAt) < keyCacheTTL {
		keys := make([]models.DeeplKey, len(kr.cachedKeys))
		copy(keys, kr.cachedKeys)
		return keys, nil
	}

	// 先恢复已过禁用期的 key
	if err := database.ReactivateExpiredKeys(kr.writeDB, now); err != nil {
		log.Printf("keyring: reactivate expired keys failed: %v", err)
	}

	keys, err := database.GetActiveKeys(kr.readDB)
	if err != nil {
		return nil, err
	}

	var active []models.DeeplKey
	for _, k := range keys {
		if k.Status == "active" {
			active = append(active, k)
		}
	}

	kr.cachedKeys = active
	kr.dirty = false
	kr.cachedAt = now

	result := make([]models.DeeplKey, len(active))
	copy(result, active)
	return result, nil
}

// --- Key CRUD wrappers (invalidate cache on write) ---

func (kr *Keyring) AddKey(name, authKey, endpoint, prov string) (int64, error) {
	id, err := database.InsertKey(kr.writeDB, name, authKey, endpoint, prov)
	if err == nil {
		kr.invalidateKeyCache()
		go kr.fetchAndSaveUsage(id, authKey, endpoint, prov)
	}
	return id, err
}

// fetchAndSaveUsage 查询指定 key 的用量并保存到数据库。
func (kr *Keyring) fetchAndSaveUsage(keyID int64, authKey, endpoint, prov string) {
	p := provider.DetectProvider(prov)
	if !p.SupportsUsage() {
		return
	}
	info, err := p.FetchUsage(endpoint, authKey)
	if err != nil {
		log.Printf("keyring: fetch usage for key %d failed: %v", keyID, err)
		return
	}
	if info.Ok && (info.CharacterCount != nil || info.CharacterLimit != nil) {
		now := time.Now().UnixMilli()
		if err := database.UpdateUsageSnapshot(kr.writeDB, keyID, info.CharacterCount, info.CharacterLimit, now); err != nil {
			log.Printf("keyring: save usage for key %d failed: %v", keyID, err)
			return
		}
		kr.invalidateKeyCache()
	}
}

// RefreshAllUsage 查询所有 key（含未启用）的用量，用于启动时初始化。
func (kr *Keyring) RefreshAllUsage() {
	keys, err := database.GetAllKeys(kr.readDB)
	if err != nil {
		log.Printf("keyring: refresh all usage failed: %v", err)
		return
	}
	for _, k := range keys {
		kr.fetchAndSaveUsage(k.ID, k.AuthKey, k.Endpoint, k.Provider)
	}
	log.Printf("keyring: refreshed usage for %d key(s)", len(keys))
}

func (kr *Keyring) UpdateKeyByID(id int64, name, authKey, endpoint, prov, status *string) error {
	err := database.UpdateKey(kr.writeDB, id, name, authKey, endpoint, prov, status)
	if err == nil {
		kr.invalidateKeyCache()
	}
	return err
}

func (kr *Keyring) DeleteKeyByID(id int64) error {
	err := database.DeleteKey(kr.writeDB, id)
	if err == nil {
		kr.invalidateKeyCache()
	}
	return err
}

// Translate 轮询 active key 进行翻译，失败时自动熔断。
// Uses round-robin starting index to distribute load evenly across keys.
func (kr *Keyring) Translate(keys []models.DeeplKey, req *provider.TranslateRequest) (*provider.TranslateResult, *models.DeeplKey, *FailureInfo, error) {
	n := len(keys)
	if n == 0 {
		return nil, nil, nil, fmt.Errorf("no keys")
	}

	start := int(kr.counter.Add(1)-1) % n

	var lastFailure *FailureInfo

	for i := 0; i < n; i++ {
		k := keys[(start+i)%n]
		prov := provider.DetectProvider(k.Provider)
		result, err := prov.Translate(k.Endpoint, k.AuthKey, req)

		if err == nil && result.StatusCode < 500 {
			if kr.shouldSampleUsage() && prov.SupportsUsage() {
				go kr.fetchAndSaveUsage(k.ID, k.AuthKey, k.Endpoint, k.Provider)
			}
			return result, &k, nil, nil
		}

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
			errType = provider.ClassifyError(statusCode, body, siteType)
		}

		// 打印上游错误详情
		if err != nil {
			log.Printf("keyring: key %s upstream error: %v", k.Name, err)
		} else {
			log.Printf("keyring: key %s upstream %d [%s]: %s", k.Name, statusCode, errType, truncateStr(body, 200))
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
		_ = database.DisableKeyMonthly(kr.writeDB, k.ID, code, msg, now)
	case "temporary":
		_ = database.DisableKeyTemporary(kr.writeDB, k.ID, code, msg, 5*60*1000, now)
	case "permanent":
		_ = database.DisableKeyPermanent(kr.writeDB, k.ID, code, msg, now)
	default:
		// network_error（超时等）：不禁用，仅记录错误信息
		return
	}

	kr.invalidateKeyCache()
}

func (kr *Keyring) shouldSampleUsage() bool {
	return rand.Float64() < kr.sampleRate
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
			if dbErr := database.UpdateUsageSnapshot(kr.writeDB, k.ID, info.CharacterCount, info.CharacterLimit, now); dbErr != nil {
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
