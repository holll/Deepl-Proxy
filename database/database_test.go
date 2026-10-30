package database

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"deepl-proxy/models"
)

func newTestDB(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	writeDB, readDB := Init(path)
	t.Cleanup(func() {
		_ = writeDB.Close()
		_ = readDB.Close()
	})
	return writeDB, readDB
}

func TestCacheRoundTrip(t *testing.T) {
	writeDB, readDB := newTestDB(t)
	now := time.Now().UnixMilli()

	want := &models.TranslateCache{
		CacheKey:    "k-roundtrip",
		Body:        []byte(`{"translations":[{"text":"你好"}]}`),
		HeadersJSON: `{"Content-Type":["application/json"]}`,
		CreatedAt:   now,
		ExpiresAt:   now + 60_000,
	}
	if err := UpsertCache(writeDB, want); err != nil {
		t.Fatalf("UpsertCache: %v", err)
	}

	got, err := GetCacheByKey(readDB, want.CacheKey, now)
	if err != nil {
		t.Fatalf("GetCacheByKey: %v", err)
	}
	if got == nil {
		t.Fatal("GetCacheByKey returned nil for an existing, unexpired row")
	}
	if string(got.Body) != string(want.Body) {
		t.Errorf("body = %q, want %q", got.Body, want.Body)
	}
	if got.HeadersJSON != want.HeadersJSON {
		t.Errorf("headers = %q, want %q", got.HeadersJSON, want.HeadersJSON)
	}
}

func TestGetCacheByKeyMissReturnsNil(t *testing.T) {
	writeDB, readDB := newTestDB(t)
	_ = writeDB

	got, err := GetCacheByKey(readDB, "does-not-exist", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("miss should not be an error: %v", err)
	}
	if got != nil {
		t.Fatalf("miss returned a row: %+v", got)
	}
}

// 过期行必须查不到，即使物理上还在表里。
func TestGetCacheByKeyHidesExpiredRows(t *testing.T) {
	writeDB, readDB := newTestDB(t)
	now := time.Now().UnixMilli()

	row := &models.TranslateCache{
		CacheKey: "k-expired", Body: []byte("x"), HeadersJSON: "{}",
		CreatedAt: now - 10_000, ExpiresAt: now - 1,
	}
	if err := UpsertCache(writeDB, row); err != nil {
		t.Fatalf("UpsertCache: %v", err)
	}

	got, err := GetCacheByKey(readDB, row.CacheKey, now)
	if err != nil {
		t.Fatalf("GetCacheByKey: %v", err)
	}
	if got != nil {
		t.Fatal("expired row was returned")
	}
}

// ON CONFLICT DO UPDATE 必须保留首次写入的 created_at。
func TestUpsertPreservesCreatedAt(t *testing.T) {
	writeDB, readDB := newTestDB(t)
	first := time.Now().UnixMilli() - 5_000

	row := &models.TranslateCache{
		CacheKey: "k-created", Body: []byte("v1"), HeadersJSON: "{}",
		CreatedAt: first, ExpiresAt: first + 60_000,
	}
	if err := UpsertCache(writeDB, row); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	row.Body = []byte("v2")
	row.CreatedAt = first + 5_000
	row.ExpiresAt = first + 120_000
	if err := UpsertCache(writeDB, row); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := GetCacheByKey(readDB, row.CacheKey, first)
	if err != nil || got == nil {
		t.Fatalf("GetCacheByKey: %v (row=%v)", err, got)
	}
	if string(got.Body) != "v2" {
		t.Errorf("body = %q, want v2", got.Body)
	}
	if got.CreatedAt != first {
		t.Errorf("created_at = %d, want %d (must not be overwritten)", got.CreatedAt, first)
	}
	if got.ExpiresAt != first+120_000 {
		t.Errorf("expires_at = %d, want %d", got.ExpiresAt, first+120_000)
	}
}

func TestCompareAndRenewCache(t *testing.T) {
	writeDB, readDB := newTestDB(t)
	now := time.Now().UnixMilli()

	row := &models.TranslateCache{
		CacheKey: "k-cas", Body: []byte("x"), HeadersJSON: "{}",
		CreatedAt: now, ExpiresAt: now + 1_000,
	}
	if err := UpsertCache(writeDB, row); err != nil {
		t.Fatalf("UpsertCache: %v", err)
	}

	// 旧值不匹配 → 不更新
	ok, err := CompareAndRenewCache(writeDB, row.CacheKey, now+999, now+9_000)
	if err != nil {
		t.Fatalf("renew with stale value: %v", err)
	}
	if ok {
		t.Error("CAS renewed with a mismatched expires_at")
	}

	// 旧值匹配 → 更新成功
	ok, err = CompareAndRenewCache(writeDB, row.CacheKey, now+1_000, now+9_000)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !ok {
		t.Fatal("CAS failed to renew with the matching expires_at")
	}

	got, err := GetCacheByKey(readDB, row.CacheKey, now)
	if err != nil || got == nil {
		t.Fatalf("GetCacheByKey: %v", err)
	}
	if got.ExpiresAt != now+9_000 {
		t.Errorf("expires_at = %d, want %d", got.ExpiresAt, now+9_000)
	}

	// 同一个旧值不能二次生效，说明并发命中只有一个能续期
	ok, err = CompareAndRenewCache(writeDB, row.CacheKey, now+1_000, now+99_000)
	if err != nil {
		t.Fatalf("second renew: %v", err)
	}
	if ok {
		t.Error("CAS renewed twice from the same old value")
	}
}

func TestCleanExpiredCache(t *testing.T) {
	writeDB, readDB := newTestDB(t)
	now := time.Now().UnixMilli()

	for i, expiresAt := range []int64{now - 3, now - 2, now - 1, now + 60_000} {
		row := &models.TranslateCache{
			CacheKey: "k-clean-" + string(rune('a'+i)), Body: []byte("x"), HeadersJSON: "{}",
			CreatedAt: now - 1_000, ExpiresAt: expiresAt,
		}
		if err := UpsertCache(writeDB, row); err != nil {
			t.Fatalf("UpsertCache: %v", err)
		}
	}

	if err := CleanExpiredCache(writeDB, 500, 20); err != nil {
		t.Fatalf("CleanExpiredCache: %v", err)
	}

	var remaining int
	row, err := queryRow(readDB, "SELECT COUNT(*) FROM deepl_translation_cache")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := row.Scan(&remaining); err != nil {
		t.Fatalf("count scan: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining = %d, want 1 (only the unexpired row)", remaining)
	}

	got, err := GetCacheByKey(readDB, "k-clean-d", now)
	if err != nil || got == nil {
		t.Fatal("cleanup deleted the unexpired row")
	}
}

func TestKeyLifecycle(t *testing.T) {
	writeDB, readDB := newTestDB(t)

	id, err := InsertKey(writeDB, "test", "secret", "https://api.deepl.com", "deepl")
	if err != nil {
		t.Fatalf("InsertKey: %v", err)
	}

	keys, err := GetActiveKeys(readDB)
	if err != nil {
		t.Fatalf("GetActiveKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Name != "test" {
		t.Fatalf("GetActiveKeys = %+v", keys)
	}

	// 停用后不应出现在可用列表里
	disabled := "disabled"
	if err := UpdateKey(writeDB, id, nil, nil, nil, nil, &disabled); err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	keys, err = GetActiveKeys(readDB)
	if err != nil {
		t.Fatalf("GetActiveKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("disabled key is still active: %+v", keys)
	}

	if err := DeleteKey(writeDB, id); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	keys, _ = GetAllKeys(readDB)
	if len(keys) != 0 {
		t.Fatalf("key was not deleted: %+v", keys)
	}
}
