package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"deepl-proxy/models"
)

func TestPoolWaitHonorsContext(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "prepare"
		if warm {
			name = "cached statement"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := newTestDB(t)
			row := &models.TranslateCache{CacheKey: "test", Body: []byte("x"), HeadersJSON: "{}", CreatedAt: 1, ExpiresAt: 2}
			if warm {
				if err := UpsertCache(context.Background(), db, row); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = UpsertCache(ctx, db, row)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("pool wait: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("pool wait ignored deadline")
			}
		})
	}
}

func TestCacheDefaultBudgetBoundsPoolWait(t *testing.T) {
	db, _ := newTestDB(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	err = UpsertCache(context.Background(), db, &models.TranslateCache{CacheKey: "k"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default budget: %v", err)
	}
	if elapsed := time.Since(start); elapsed < CacheTimeout/2 || elapsed > time.Second {
		t.Fatalf("elapsed %v", elapsed)
	}
}

func TestCanceledDatabaseOperations(t *testing.T) {
	db, _ := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := map[string]func() error{
		"keys":       func() error { _, err := GetActiveKeys(ctx, db); return err },
		"key update": func() error { return UpdateKey(ctx, db, 1, nil, nil, nil, nil, nil, nil) },
		"disable":    func() error { return DisableKeyPermanent(ctx, db, 1, "", "", 1) },
		"usage":      func() error { return UpdateUsageSnapshot(ctx, db, 1, nil, nil, 1) },
		"cache read": func() error { _, err := GetCacheByKey(ctx, db, "k", 1); return err },
		"cache list": func() error { _, _, err := ListCacheEntries(ctx, db, 1, 0); return err },
		"cleanup":    func() error { return CleanExpiredCache(ctx, db, 1, 2) },
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestInitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w, r, err := Init(ctx, filepath.Join(t.TempDir(), "test.db"))
	if !errors.Is(err, context.Canceled) || w != nil || r != nil {
		t.Fatalf("Init: %v %v %v", w, r, err)
	}
}

func TestSQLExecutionCancellation(t *testing.T) {
	db, _ := newTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	// Exercise driver cancellation while executing, not only pool acquisition.
	var n int64
	err := db.QueryRowContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000000000) SELECT sum(x) FROM n`).Scan(&n)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expensive SQL did not cancel: %v", err)
	}
	// Connection remains usable after interruption.
	if err := db.PingContext(context.Background()); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}

func TestCacheBudgetDuringSQLiteWriteLock(t *testing.T) {
	w, r := newTestDB(t)
	lock, err := r.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer lock.ExecContext(context.Background(), "ROLLBACK")
	start := time.Now()
	err = UpsertCache(context.Background(), w, &models.TranslateCache{CacheKey: "locked", Body: []byte("x"), HeadersJSON: "{}", CreatedAt: 1, ExpiresAt: 2})
	if err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("SQLite busy wait defeated cache budget: %v (%v)", elapsed, err)
	}
}
