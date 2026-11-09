package database

import (
	"context"
	"reflect"
	"testing"

	"deepl-proxy/models"
)

func TestListCacheEntriesPagination(t *testing.T) {
	writeDB, readDB := newTestDB(t)
	for i, key := range []string{"oldest", "middle", "newest"} {
		if err := UpsertCache(context.Background(), writeDB, &models.TranslateCache{
			CacheKey: key, Body: []byte(key), HeadersJSON: "{}",
			CreatedAt: int64(i + 1), ExpiresAt: int64(i + 10),
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name          string
		limit, offset int
		want          []string
	}{
		{"first page", 2, 0, []string{"newest", "middle"}},
		{"last page", 2, 2, []string{"oldest"}},
		{"past end", 2, 3, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, total, err := ListCacheEntries(context.Background(), readDB, tc.limit, tc.offset)
			if err != nil {
				t.Fatal(err)
			}
			if total != 3 {
				t.Fatalf("total = %d, want 3 (including expired entries)", total)
			}
			got := make([]string, 0, len(entries))
			for _, entry := range entries {
				got = append(got, entry.CacheKey)
				if string(entry.Body) != entry.CacheKey || entry.HeadersJSON != "{}" {
					t.Errorf("entry fields changed: %+v", entry)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("keys = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListCacheEntriesEmpty(t *testing.T) {
	_, readDB := newTestDB(t)
	entries, total, err := ListCacheEntries(context.Background(), readDB, 50, 0)
	if err != nil || total != 0 || len(entries) != 0 {
		t.Fatalf("empty list = %v, %d, %v", entries, total, err)
	}
}
