package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"deepl-proxy/database"
	"deepl-proxy/models"
)

func TestKeyReloadWaitHonorsCancellation(t *testing.T) {
	kr := NewKeyring(context.Background(), nil, nil)
	kr.reloadMu <- struct{}{}
	defer func() { <-kr.reloadMu }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := kr.GetActiveKeys(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reload wait: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("reload mutex ignored cancellation")
	}
}

func TestUsageWorkerStopsWithLifecycleWhileWritePoolExhausted(t *testing.T) {
	w, r, err := database.Init(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()
	conn, err := w.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fetched := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"character_count":1,"character_limit":10}`))
		close(fetched)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kr := NewKeyring(ctx, w, r)
	kr.sampleUsageAsync(context.Background(), models.DeeplKey{ID: 1, Provider: "deepl", Endpoint: srv.URL})
	select {
	case <-fetched:
	case <-time.After(time.Second):
		t.Fatal("usage was not fetched")
	}
	cancel()
	done := make(chan struct{})
	go func() { kr.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("usage database write outlived lifecycle")
	}
	kr.sampleUsageAsync(context.Background(), models.DeeplKey{})
	if len(kr.usageSem) != 0 {
		t.Fatal("closed keyring accepted new worker")
	}
}
