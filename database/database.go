package database

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"deepl-proxy/models"

	_ "modernc.org/sqlite"
)

// Init opens the SQLite database, ensures directories exist, and runs migrations.
func Init(dbPath string) *sql.DB {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Fatalf("create db directory: %v", err)
	}

	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatalf("ping sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	migrate(db)
	return db
}

func migrate(db *sql.DB) {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS deepl_keys (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT,
			auth_key        TEXT NOT NULL DEFAULT '',
			endpoint        TEXT NOT NULL,
			provider        TEXT NOT NULL DEFAULT 'deepl',
			status          TEXT NOT NULL DEFAULT 'active',
			disable_type    TEXT,
			disabled_until  INTEGER,
			last_error_code TEXT,
			last_error_message TEXT,
			last_used_at    INTEGER,
			last_checked_at INTEGER,
			character_count INTEGER,
			character_limit INTEGER,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_deepl_keys_status_id ON deepl_keys(status, id)`,
		`CREATE TABLE IF NOT EXISTS deepl_translation_cache (
			cache_key   TEXT PRIMARY KEY,
			body        TEXT NOT NULL,
			headers_json TEXT,
			created_at  INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_translation_cache_expires_at ON deepl_translation_cache(expires_at)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			log.Printf("migration warning: %v", err)
		}
	}
}

// --- deepl_keys CRUD ---

func InsertKey(db *sql.DB, name, authKey, endpoint, provider string) (int64, error) {
	now := time.Now().UnixMilli()
	res, err := db.Exec(
		"INSERT INTO deepl_keys (name,auth_key,endpoint,provider,status,created_at,updated_at) VALUES (?,?,?,?,'active',?,?)",
		name, authKey, endpoint, provider, now, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func UpdateKey(db *sql.DB, id int64, name, authKey, endpoint, provider, status *string) error {
	now := time.Now().UnixMilli()
	if status != nil && *status == "active" {
		_, err := db.Exec(
			"UPDATE deepl_keys SET name=COALESCE(?,name), auth_key=COALESCE(?,auth_key), endpoint=COALESCE(?,endpoint), provider=COALESCE(?,provider), status=?, disable_type=NULL, disabled_until=NULL, updated_at=? WHERE id=?",
			name, authKey, endpoint, provider, *status, now, id,
		)
		return err
	}
	_, err := db.Exec(
		"UPDATE deepl_keys SET name=COALESCE(?,name), auth_key=COALESCE(?,auth_key), endpoint=COALESCE(?,endpoint), provider=COALESCE(?,provider), status=COALESCE(?,status), updated_at=? WHERE id=?",
		name, authKey, endpoint, provider, status, now, id,
	)
	return err
}

func DeleteKey(db *sql.DB, id int64) error {
	_, err := db.Exec("DELETE FROM deepl_keys WHERE id=?", id)
	return err
}

func GetActiveKeys(db *sql.DB) ([]models.DeeplKey, error) {
	return queryKeys(db, "SELECT * FROM deepl_keys WHERE status='active' ORDER BY id ASC")
}

func GetAllKeys(db *sql.DB) ([]models.DeeplKey, error) {
	return queryKeys(db, "SELECT id,name,endpoint,provider,status,disable_type,disabled_until,last_error_code,last_error_message,last_used_at,last_checked_at,character_count,character_limit,created_at,updated_at FROM deepl_keys ORDER BY id ASC")
}

func queryKeys(db *sql.DB, query string, args ...any) ([]models.DeeplKey, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var keys []models.DeeplKey
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}

		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}

		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = vals[i]
		}

		k := models.DeeplKey{
			ID:               int64Val(m["id"]),
			Name:             strVal(m["name"]),
			AuthKey:          strVal(m["auth_key"]),
			Endpoint:         strVal(m["endpoint"]),
			Provider:         strVal(m["provider"]),
			Status:           strVal(m["status"]),
			DisableType:      nullableStr(m["disable_type"]),
			DisabledUntil:    nullableInt64(m["disabled_until"]),
			LastErrorCode:    nullableStr(m["last_error_code"]),
			LastErrorMessage: nullableStr(m["last_error_message"]),
			LastUsedAt:       nullableInt64(m["last_used_at"]),
			LastCheckedAt:    nullableInt64(m["last_checked_at"]),
			CharacterCount:   nullableInt64(m["character_count"]),
			CharacterLimit:   nullableInt64(m["character_limit"]),
			CreatedAt:        int64Val(m["created_at"]),
			UpdatedAt:        int64Val(m["updated_at"]),
		}
		if k.Provider == "" {
			k.Provider = "deepl"
		}
		k.DisabledUntilISO = isoOrNil(k.DisabledUntil)
		k.LastUsedAtISO = isoOrNil(k.LastUsedAt)
		k.LastCheckedAtISO = isoOrNil(k.LastCheckedAt)
		k.CreatedAtISO = msToISO(k.CreatedAt)
		k.UpdatedAtISO = msToISO(k.UpdatedAt)

		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// --- 缓存操作 ---

func GetCacheByKey(db *sql.DB, cacheKey string, now int64) (*models.TranslateCache, error) {
	row := db.QueryRow(
		"SELECT cache_key, body, headers_json, created_at, expires_at FROM deepl_translation_cache WHERE cache_key=? AND expires_at>?",
		cacheKey, now,
	)
	var c models.TranslateCache
	err := row.Scan(&c.CacheKey, &c.Body, &c.HeadersJSON, &c.CreatedAt, &c.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func UpsertCache(db *sql.DB, c *models.TranslateCache) error {
	_, err := db.Exec(
		"INSERT OR REPLACE INTO deepl_translation_cache (cache_key,body,headers_json,created_at,expires_at) VALUES (?,?,?,?,?)",
		c.CacheKey, c.Body, c.HeadersJSON, c.CreatedAt, c.ExpiresAt,
	)
	return err
}

func UpdateCacheExpiry(db *sql.DB, cacheKey string, expiresAt int64) error {
	_, err := db.Exec("UPDATE deepl_translation_cache SET expires_at=? WHERE cache_key=?", expiresAt, cacheKey)
	return err
}

// ListCacheEntries 列出缓存条目，支持搜索和分页。
func ListCacheEntries(db *sql.DB, search string, limit, offset int) ([]models.TranslateCache, int, error) {
	var countQuery, listQuery string
	var countArgs, listArgs []any

	if search != "" {
		like := "%" + search + "%"
		countQuery = "SELECT COUNT(*) FROM deepl_translation_cache WHERE cache_key LIKE ? OR body LIKE ?"
		listQuery = "SELECT cache_key, body, headers_json, created_at, expires_at FROM deepl_translation_cache WHERE cache_key LIKE ? OR body LIKE ? ORDER BY created_at DESC LIMIT ? OFFSET ?"
		countArgs = []any{like, like}
		listArgs = []any{like, like, limit, offset}
	} else {
		countQuery = "SELECT COUNT(*) FROM deepl_translation_cache"
		listQuery = "SELECT cache_key, body, headers_json, created_at, expires_at FROM deepl_translation_cache ORDER BY created_at DESC LIMIT ? OFFSET ?"
		countArgs = nil
		listArgs = []any{limit, offset}
	}

	var total int
	if err := db.QueryRow(countQuery, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := db.Query(listQuery, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var entries []models.TranslateCache
	for rows.Next() {
		var c models.TranslateCache
		if err := rows.Scan(&c.CacheKey, &c.Body, &c.HeadersJSON, &c.CreatedAt, &c.ExpiresAt); err != nil {
			return nil, 0, err
		}
		entries = append(entries, c)
	}
	return entries, total, rows.Err()
}

func DeleteCacheByKey(db *sql.DB, cacheKey string) error {
	_, err := db.Exec("DELETE FROM deepl_translation_cache WHERE cache_key=?", cacheKey)
	return err
}

func CleanExpiredCache(db *sql.DB, batchSize, maxRounds int) error {
	now := time.Now().UnixMilli()
	for i := 0; i < maxRounds; i++ {
		res, err := db.Exec(
			"DELETE FROM deepl_translation_cache WHERE cache_key IN (SELECT cache_key FROM deepl_translation_cache WHERE expires_at<=? LIMIT ?)",
			now, batchSize,
		)
		if err != nil {
			return err
		}
		affected, _ := res.RowsAffected()
		if affected < int64(batchSize) {
			break
		}
	}
	return nil
}

// --- D1 兼容熔断辅助 ---

func DisableKeyTemporary(db *sql.DB, keyID int64, errorCode, errorMessage string, ms int64, now int64) error {
	_, err := db.Exec(
		"UPDATE deepl_keys SET status='disabled',disable_type='temporary',disabled_until=?,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?",
		now+ms, truncate(errorCode, 100), truncate(errorMessage, 500), now, keyID,
	)
	return err
}

func DisableKeyMonthly(db *sql.DB, keyID int64, errorCode, errorMessage string, now int64) error {
	beginning := startOfNextMonthBeijing(time.UnixMilli(now)).UnixMilli()
	_, err := db.Exec(
		"UPDATE deepl_keys SET status='disabled',disable_type='monthly',disabled_until=?,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?",
		beginning, truncate(errorCode, 100), truncate(errorMessage, 500), now, keyID,
	)
	return err
}

func DisableKeyPermanent(db *sql.DB, keyID int64, errorCode, errorMessage string, now int64) error {
	_, err := db.Exec(
		"UPDATE deepl_keys SET status='dead',disable_type='permanent',disabled_until=NULL,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?",
		truncate(errorCode, 100), truncate(errorMessage, 500), now, keyID,
	)
	return err
}

func UpdateUsageSnapshot(db *sql.DB, keyID int64, charCount, charLimit *int64, now int64) error {
	_, err := db.Exec(
		"UPDATE deepl_keys SET character_count=?,character_limit=?,last_checked_at=?,updated_at=? WHERE id=?",
		nullInt64(charCount), nullInt64(charLimit), now, now, keyID,
	)
	return err
}

// --- helpers ---

func startOfNextMonthBeijing(now time.Time) time.Time {
	offset := 8 * time.Hour
	bj := now.In(time.FixedZone("CST", 8*60*60))
	_ = bj
	bjNow := now.Add(offset)
	utcNext := time.Date(bjNow.Year(), bjNow.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return utcNext.Add(-offset)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func int64Val(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	}
	return 0
}

func strVal(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return ""
}

func nullableStr(v any) *string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return &t
	}
	return nil
}

func nullableInt64(v any) *int64 {
	switch t := v.(type) {
	case nil:
		return nil
	case int64:
		if t == 0 {
			return nil
		}
		return &t
	case float64:
		val := int64(t)
		if val == 0 {
			return nil
		}
		return &val
	}
	return nil
}

func msToISO(ms int64) *string {
	if ms == 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	s := t.Format(time.RFC3339)
	return &s
}

func isoOrNil(ms *int64) *string {
	if ms == nil {
		return nil
	}
	return msToISO(*ms)
}
