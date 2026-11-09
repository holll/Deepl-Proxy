package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestCacheFailureDoesNotFailSuccessfulTranslation(t *testing.T) {
	for _, failure := range []string{"write pool exhausted", "cache table missing"} {
		t.Run(failure, func(t *testing.T) {
			h := newHarness(t, 1)
			h.addKey("dl", "deepl")
			// Warm Keyring independently of the later cache failure.
			status, _, body := h.translateJSON(map[string]any{"text": "warm", "target_lang": "ZH"}, nil)
			assertStatus(t, status, http.StatusOK, body)
			if failure == "write pool exhausted" {
				conn, err := h.writeDB.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
			} else {
				if _, err := h.writeDB.ExecContext(context.Background(), "DROP TABLE deepl_translation_cache"); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			status, _, body = h.translateJSON(map[string]any{"text": "uncached", "target_lang": "ZH"}, nil)
			assertStatus(t, status, http.StatusOK, body)
			if len(translationTexts(t, body)) != 1 {
				t.Fatal("successful upstream response lost")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("best effort cache stalled response: %v", elapsed)
			}
		})
	}
}
