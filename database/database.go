package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"deepl-proxy/models"

	_ "modernc.org/sqlite"
)

// Init opens the SQLite database, ensures directories exist, and runs migrations.
// Returns separate write (MaxOpenConns=1) and read (MaxOpenConns=4) connections.
// WAL mode allows concurrent readers alongside a single writer.
// Budgets include statement preparation, pool acquisition, execution and row scanning.
const (
	OperationTimeout = 2 * time.Second
	CacheTimeout     = 200 * time.Millisecond
	CleanupTimeout   = 5 * time.Second
	InitTimeout      = 15 * time.Second
)

func Init(ctx context.Context, dbPath string) (writeDB *sql.DB, readDB *sql.DB, err error) {
	ctx, cancel := context.WithTimeout(ctx, InitTimeout)
	defer cancel()
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, fmt.Errorf("create db directory: %w", err)
	}

	// synchronous=NORMAL 是 WAL 模式下的推荐配置：不再每次提交都 fsync，
	// 崩溃最多丢掉最后一个事务，不会损坏库。默认的 FULL 让每次缓存写入
	// 都要等一次磁盘刷盘，在翻译这种"每请求一次小写"的场景里代价极高。
	// cache_size=-16384 即 16MiB 页缓存（默认仅 2MiB），temp_store=MEMORY
	// 让临时表/排序走内存。
	dsn := fmt.Sprintf(
		// modernc's busy handler can delay context interruption. Keep its own
		// lock wait below CacheTimeout; the application already serializes writers.
		"%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(100)&_pragma=foreign_keys(ON)"+
			"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-16384)&_pragma=temp_store(MEMORY)",
		dbPath,
	)

	// Write connection — single writer, serialized
	wdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite write: %w", err)
	}
	defer func() {
		if err != nil {
			_ = wdb.Close()
		}
	}()
	if err = wdb.PingContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("ping sqlite write: %w", err)
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)

	if err = migrate(ctx, wdb); err != nil {
		return nil, nil, err
	}

	// Read connection — multiple concurrent readers (WAL)
	rdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite read: %w", err)
	}
	defer func() {
		if err != nil {
			_ = rdb.Close()
		}
	}()
	if err = rdb.PingContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("ping sqlite read: %w", err)
	}
	rdb.SetMaxOpenConns(4)
	rdb.SetMaxIdleConns(4)
	rdb.SetConnMaxLifetime(0)

	return wdb, rdb, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
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
		`CREATE TABLE IF NOT EXISTS app_settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}

	// 增量列：CREATE TABLE IF NOT EXISTS 不会给已存在的旧表补列，
	// 因此新增字段必须显式做一次"缺列才加"的迁移。
	if err := ensureColumn(ctx, db, "deepl_keys", "quota_period",
		"ALTER TABLE deepl_keys ADD COLUMN quota_period TEXT NOT NULL DEFAULT 'auto'"); err != nil {
		return fmt.Errorf("migration quota_period: %w", err)
	}
	return nil
}

// ensureColumn 在列不存在时执行 ddl。
//
// 不能靠"执行失败就忽略"来判断：真正的迁移错误（磁盘满、库只读）
// 也会失败，那样会把故障吞掉。这里先读 PRAGMA table_info 再决定。
func ensureColumn(ctx context.Context, db *sql.DB, table, column, ddl string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, ddl)
	return err
}

// --- 预编译语句缓存 ---

// stmtKey 以 (连接池, SQL) 为键。写库和读库是两套连接池，
// 同一个 SQL 在两边必须是各自独立的 *sql.Stmt。
type stmtKey struct {
	db *sql.DB
	q  string
}

var stmtCache sync.Map // stmtKey -> *sql.Stmt

// prepare 复用已编译的语句。
//
// database/sql 在直接 db.Exec/db.Query 时每次调用都会重新 Prepare，
// 而 modernc.org/sqlite 每次 Prepare 都要重新编译一遍 SQL 虚拟机——
// 对"每请求至少一次查询"的热路径来说这是纯浪费。
func prepare(ctx context.Context, db *sql.DB, q string) (*sql.Stmt, error) {
	k := stmtKey{db: db, q: q}
	if v, ok := stmtCache.Load(k); ok {
		return v.(*sql.Stmt), nil
	}
	st, err := db.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	actual, loaded := stmtCache.LoadOrStore(k, st)
	if loaded {
		// 并发下别的 goroutine 先存了，丢掉自己这份，避免泄漏语句句柄。
		_ = st.Close()
		return actual.(*sql.Stmt), nil
	}
	return st, nil
}

func exec(ctx context.Context, db *sql.DB, q string, args ...any) (sql.Result, error) {
	st, err := prepare(ctx, db, q)
	if err != nil {
		return nil, err
	}
	return st.ExecContext(ctx, args...)
}

func query(ctx context.Context, db *sql.DB, q string, args ...any) (*sql.Rows, error) {
	st, err := prepare(ctx, db, q)
	if err != nil {
		return nil, err
	}
	return st.QueryContext(ctx, args...)
}

func queryRow(ctx context.Context, db *sql.DB, q string, args ...any) (*sql.Row, error) {
	st, err := prepare(ctx, db, q)
	if err != nil {
		return nil, err
	}
	return st.QueryRowContext(ctx, args...), nil
}

// --- app_settings ---

// GetAppSettings 读取全部运行时设置（webhook 等）。
//
// 表很小且只在启动/保存时读取，不做缓存；管理后台改完即可见。
func GetAppSettings(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := query(ctx, db, `SELECT key, value FROM app_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetAppSetting 写入一条设置。value 为空字符串表示删除该覆盖项，
// 让配置回落到 config.yaml / 环境变量。
func SetAppSetting(ctx context.Context, db *sql.DB, key, value string, now int64) error {
	if strings.TrimSpace(value) == "" {
		_, err := exec(ctx, db, `DELETE FROM app_settings WHERE key = ?`, key)
		return err
	}
	_, err := exec(ctx, db,
		`INSERT INTO app_settings(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now)
	return err
}

// --- deepl_keys CRUD ---

func InsertKey(ctx context.Context, db *sql.DB, name, authKey, endpoint, provider, quotaPeriod string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	now := time.Now().UnixMilli()
	if quotaPeriod == "" {
		quotaPeriod = "auto"
	}
	res, err := exec(ctx, db,
		"INSERT INTO deepl_keys (name,auth_key,endpoint,provider,quota_period,status,created_at,updated_at) VALUES (?,?,?,?,?,'active',?,?)",
		name, authKey, endpoint, provider, quotaPeriod, now, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func UpdateKey(ctx context.Context, db *sql.DB, id int64, name, authKey, endpoint, provider, quotaPeriod, status *string) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	now := time.Now().UnixMilli()
	if status != nil && *status == "active" {
		_, err := exec(ctx, db,
			"UPDATE deepl_keys SET name=COALESCE(?,name), auth_key=COALESCE(?,auth_key), endpoint=COALESCE(?,endpoint), provider=COALESCE(?,provider), quota_period=COALESCE(?,quota_period), status=?, disable_type=NULL, disabled_until=NULL, updated_at=? WHERE id=?",
			name, authKey, endpoint, provider, quotaPeriod, *status, now, id,
		)
		return err
	}
	_, err := exec(ctx, db,
		"UPDATE deepl_keys SET name=COALESCE(?,name), auth_key=COALESCE(?,auth_key), endpoint=COALESCE(?,endpoint), provider=COALESCE(?,provider), quota_period=COALESCE(?,quota_period), status=COALESCE(?,status), updated_at=? WHERE id=?",
		name, authKey, endpoint, provider, quotaPeriod, status, now, id,
	)
	return err
}

func DeleteKey(ctx context.Context, db *sql.DB, id int64) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	_, err := exec(ctx, db, "DELETE FROM deepl_keys WHERE id=?", id)
	return err
}

func GetActiveKeys(ctx context.Context, db *sql.DB) ([]models.DeeplKey, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	return queryKeys(ctx, db, "SELECT * FROM deepl_keys WHERE status='active' ORDER BY id ASC")
}

// ReactivateExpiredKeys 恢复已到期的临时或配额禁用，并返回恢复数量。
func ReactivateExpiredKeys(ctx context.Context, db *sql.DB, now int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	result, err := exec(ctx, db,
		"UPDATE deepl_keys SET status='active',disable_type=NULL,disabled_until=NULL,updated_at=? WHERE status='disabled' AND disable_type IN ('temporary','quota') AND disabled_until<=?",
		now, now,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func GetAllKeys(ctx context.Context, db *sql.DB) ([]models.DeeplKey, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	return queryKeys(ctx, db, "SELECT id,name,endpoint,provider,status,disable_type,disabled_until,last_error_code,last_error_message,last_used_at,last_checked_at,character_count,character_limit,created_at,updated_at FROM deepl_keys ORDER BY id ASC")
}

// GetAllKeysWithAuth 返回所有 key（含 auth_key），仅供内部使用（如用量查询），不暴露给 admin API。
func GetAllKeysWithAuth(ctx context.Context, db *sql.DB) ([]models.DeeplKey, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	return queryKeys(ctx, db, "SELECT * FROM deepl_keys ORDER BY id ASC")
}

// GetKeyByID 返回单个 key（含 auth_key），供"测试"/"刷新单个 key"使用。
func GetKeyByID(ctx context.Context, db *sql.DB, id int64) (*models.DeeplKey, error) {
	keys, err := queryKeys(ctx, db, "SELECT * FROM deepl_keys WHERE id=?", id)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, sql.ErrNoRows
	}
	return &keys[0], nil
}

func queryKeys(ctx context.Context, db *sql.DB, q string, args ...any) ([]models.DeeplKey, error) {
	rows, err := query(ctx, db, q, args...)
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
			QuotaPeriod:      strVal(m["quota_period"]),
			Status:           strVal(m["status"]),
			DisableType:      nullableStr(m["disable_type"]),
			DisabledUntil:    nullableInt64(m["disabled_until"]),
			LastErrorCode:    nullableStr(m["last_error_code"]),
			LastErrorMessage: nullableStr(m["last_error_message"]),
			LastUsedAt:       nullableInt64(m["last_used_at"]),
			LastCheckedAt:    nullableInt64(m["last_checked_at"]),
			// 用量列 0 是合法值（免费额度 0 / 已用 0），不能当 NULL 丢弃
			CharacterCount: nullableInt64KeepZero(m["character_count"]),
			CharacterLimit: nullableInt64KeepZero(m["character_limit"]),
			CreatedAt:      int64Val(m["created_at"]),
			UpdatedAt:      int64Val(m["updated_at"]),
		}
		if k.Provider == "" {
			k.Provider = "deepl"
		}
		if k.QuotaPeriod == "" {
			k.QuotaPeriod = "auto"
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

// GetCacheByKey 命中返回条目，未命中/已过期返回 (nil, nil)。
// 用 query + rows 而不是 QueryRow，就是为了能把 sql.ErrNoRows 正常折叠成"未命中"。
func GetCacheByKey(ctx context.Context, db *sql.DB, cacheKey string, now int64) (*models.TranslateCache, error) {
	ctx, cancel := context.WithTimeout(ctx, CacheTimeout)
	defer cancel()
	rows, err := query(ctx, db,
		"SELECT cache_key, body, headers_json, created_at, expires_at FROM deepl_translation_cache WHERE cache_key=? AND expires_at>?",
		cacheKey, now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, rows.Err()
	}
	var c models.TranslateCache
	if err := rows.Scan(&c.CacheKey, &c.Body, &c.HeadersJSON, &c.CreatedAt, &c.ExpiresAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertCache 写入缓存。
//
// 用 ON CONFLICT ... DO UPDATE 而不是 INSERT OR REPLACE：后者等价于
// DELETE + INSERT，会换掉 rowid、多写一次索引，并把 created_at 覆盖成
// 最新一次写入时间（admin 里显示"创建时间"其实变成"最后写入时间"）。
func UpsertCache(ctx context.Context, db *sql.DB, c *models.TranslateCache) error {
	ctx, cancel := context.WithTimeout(ctx, CacheTimeout)
	defer cancel()
	_, err := exec(ctx, db,
		`INSERT INTO deepl_translation_cache (cache_key,body,headers_json,created_at,expires_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(cache_key) DO UPDATE SET
		   body=excluded.body,
		   headers_json=excluded.headers_json,
		   expires_at=excluded.expires_at`,
		c.CacheKey, string(c.Body), c.HeadersJSON, c.CreatedAt, c.ExpiresAt,
	)
	return err
}

// CompareAndRenewCache 用 CAS 续期：只有 expires_at 仍是读取时的值才更新。
//
// 并发命中同一 key 时只有一个能更新成功，其余空转返回 false，
// 避免 N 个请求同时对单写连接做一次无条件 UPDATE。
func CompareAndRenewCache(ctx context.Context, db *sql.DB, cacheKey string, oldExpiresAt, newExpiresAt int64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, CacheTimeout)
	defer cancel()
	res, err := exec(ctx, db,
		"UPDATE deepl_translation_cache SET expires_at=? WHERE cache_key=? AND expires_at=?",
		newExpiresAt, cacheKey, oldExpiresAt,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListCacheEntries 列出全部缓存条目，支持分页，不执行关键词搜索。
func ListCacheEntries(ctx context.Context, db *sql.DB, limit, offset int) ([]models.TranslateCache, int, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	const countQuery = "SELECT COUNT(*) FROM deepl_translation_cache"
	const listQuery = "SELECT cache_key, body, headers_json, created_at, expires_at FROM deepl_translation_cache ORDER BY created_at DESC LIMIT ? OFFSET ?"

	var total int
	row, err := queryRow(ctx, db, countQuery)
	if err != nil {
		return nil, 0, err
	}
	if err := row.Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := query(ctx, db, listQuery, limit, offset)
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

func DeleteCacheByKey(ctx context.Context, db *sql.DB, cacheKey string) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	_, err := exec(ctx, db, "DELETE FROM deepl_translation_cache WHERE cache_key=?", cacheKey)
	return err
}

func CleanExpiredCache(ctx context.Context, db *sql.DB, batchSize, maxRounds int) error {
	ctx, cancel := context.WithTimeout(ctx, CleanupTimeout)
	defer cancel()
	now := time.Now().UnixMilli()
	for range maxRounds {
		// 用 rowid 而不是 cache_key 作为内层选择列：
		// expires_at 索引里本来就带着 rowid，这个子查询可以完全走索引，
		// 不需要回表取 cache_key；外层也直接按 rowid 删，最快。
		res, err := exec(ctx, db,
			"DELETE FROM deepl_translation_cache WHERE rowid IN (SELECT rowid FROM deepl_translation_cache WHERE expires_at<=? LIMIT ?)",
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

func DisableKeyTemporary(ctx context.Context, db *sql.DB, keyID int64, errorCode, errorMessage string, ms int64, now int64) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	_, err := exec(ctx, db,
		"UPDATE deepl_keys SET status='disabled',disable_type='temporary',disabled_until=?,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?",
		now+ms, truncate(errorCode, 100), truncate(errorMessage, 500), now, keyID,
	)
	return err
}

// DisableKeyQuota 把配额耗尽的 key 禁用到指定的重置时刻。
//
// 重置时刻由 service 按该 key 的配额周期算出（旧免费 key 每月、
// 新免费 key 每年），数据库层不关心套餐规则，只负责落库。
func DisableKeyQuota(ctx context.Context, db *sql.DB, keyID int64, until int64, errorCode, errorMessage string, now int64) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	_, err := exec(ctx, db,
		"UPDATE deepl_keys SET status='disabled',disable_type='quota',disabled_until=?,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?",
		until, truncate(errorCode, 100), truncate(errorMessage, 500), now, keyID,
	)
	return err
}

func DisableKeyPermanent(ctx context.Context, db *sql.DB, keyID int64, errorCode, errorMessage string, now int64) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	_, err := exec(ctx, db,
		"UPDATE deepl_keys SET status='dead',disable_type='permanent',disabled_until=NULL,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?",
		truncate(errorCode, 100), truncate(errorMessage, 500), now, keyID,
	)
	return err
}

func UpdateUsageSnapshot(ctx context.Context, db *sql.DB, keyID int64, charCount, charLimit *int64, now int64) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	_, err := exec(ctx, db,
		"UPDATE deepl_keys SET character_count=?,character_limit=?,last_checked_at=?,updated_at=? WHERE id=?",
		nullInt64(charCount), nullInt64(charLimit), now, now, keyID,
	)
	return err
}

// --- helpers ---

func truncate(s string, max int) string {
	return s[:min(len(s), max)]
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

// nullableInt64KeepZero 与 nullableInt64 相同，但 0 视为合法值而非 NULL。
// 用于 character_count / character_limit：0 表示"已用 0"或"无限额度"，前端需显示。
func nullableInt64KeepZero(v any) *int64 {
	switch t := v.(type) {
	case nil:
		return nil
	case int64:
		return &t
	case float64:
		val := int64(t)
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
