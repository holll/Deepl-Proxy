package service

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"deepl-proxy/database"
)

func TestKeyRecoveryWithoutTranslationTraffic(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "keys.db")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		w, r, err := database.Init(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		defer r.Close()
		kr := NewKeyring(ctx, w, r)
		now := time.Now().UnixMilli()
		rows := []struct {
			name, status, disableType string
			until                     *int64
			atStart, afterTick        string
		}{
			{"expired", "disabled", "temporary", new(now), "active", "active"},
			{"next-tick", "disabled", "temporary", new(now + 1), "disabled", "active"},
			{"quota-reset", "disabled", "quota", new(now + 1), "disabled", "active"},
			{"future", "disabled", "temporary", new(now + 60000), "disabled", "disabled"},
			{"manual", "disabled", "", nil, "disabled", "disabled"},
			{"permanent", "dead", "permanent", nil, "dead", "dead"},
		}
		ids := make([]int64, len(rows))
		for i, row := range rows {
			id, err := database.InsertKey(ctx, w, row.name, row.name, "https://api-free.deepl.com", "deepl", "auto")
			if err != nil {
				t.Fatal(err)
			}
			ids[i] = id
			if _, err := w.ExecContext(ctx, "UPDATE deepl_keys SET status=?, disable_type=NULLIF(?, ''), disabled_until=? WHERE id=?", row.status, row.disableType, row.until, id); err != nil {
				t.Fatal(err)
			}
		}
		// Simulate an existing empty snapshot without running the on-demand recovery path.
		kr.state.Store(&keySnapshot{loadedAt: now})
		done := make(chan struct{})
		go func() {
			defer close(done)
			kr.RunKeyRecovery(ctx)
		}()
		defer func() { cancel(); <-done }()

		check := func(ticked bool) {
			t.Helper()
			for i, row := range rows {
				want := row.atStart
				if ticked {
					want = row.afterTick
				}
				key, err := database.GetKeyByID(ctx, r, ids[i])
				if err != nil {
					t.Fatal(err)
				}
				if key.Status != want || (want == "active" && (key.DisableType != nil || key.DisabledUntil != nil)) {
					t.Fatalf("%s: got status %s and disable fields %v/%v; want %s with disable fields cleared if active", row.name, key.Status, key.DisableType, key.DisabledUntil, want)
				}
			}
			if !kr.state.Load().dirty {
				t.Fatal("recovery did not invalidate the active-key snapshot")
			}
		}
		synctest.Wait()
		check(false)
		kr.state.Store(&keySnapshot{loadedAt: now})
		time.Sleep(30 * time.Second)
		synctest.Wait()
		check(true)
	})
}
