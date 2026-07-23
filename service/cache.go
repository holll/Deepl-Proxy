package service

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/models"
)

type CacheService struct {
	writeDB *sql.DB
	readDB  *sql.DB
	cfg     *config.Config
}

func NewCacheService(writeDB, readDB *sql.DB, cfg *config.Config) *CacheService {
	return &CacheService{writeDB: writeDB, readDB: readDB, cfg: cfg}
}

type CachedHit struct {
	Body    string
	Headers http.Header
}

//
// ========================
// TTL
// ========================
//

func (cs *CacheService) TTL() time.Duration {
	return cs.cfg.Cache.TTLDuration()
}

//
// ========================
// Cache Key
// ========================
//

func BuildCacheKey(identity *models.CacheIdentity) (string, error) {
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(payload)
	return "deepl:translate:" + hex.EncodeToString(h[:]), nil
}

//
// ========================
// GET
// ========================
//

func (cs *CacheService) Get(cacheKey string) (*CachedHit, error) {
	now := time.Now().UnixMilli()

	c, err := database.GetCacheByKey(cs.readDB, cacheKey, now)
	if err != nil || c == nil {
		return nil, err
	}

	ttl := cs.TTL().Milliseconds()

	// sliding TTL: only renew when < 50% TTL remains
	if ttl > 0 {
		remaining := c.ExpiresAt - now
		if remaining < ttl/2 {
			newExp := now + ttl
			if err := database.UpdateCacheExpiry(cs.writeDB, cacheKey, newExp); err != nil {
				log.Printf("[cache] update expiry failed: %v", err)
			}
		}
	}

	// restore headers
	headers := http.Header{}
	if c.HeadersJSON != "" {
		var parsed map[string][]string
		if err := json.Unmarshal([]byte(c.HeadersJSON), &parsed); err == nil {
			for k, vs := range parsed {
				for _, v := range vs {
					headers.Add(k, v)
				}
			}
		}
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/json; charset=utf-8")
	}

	return &CachedHit{Body: c.Body, Headers: headers}, nil
}

//
// ========================
// PUT
// ========================
//

func (cs *CacheService) Put(cacheKey string, body string, upstreamHeaders http.Header) {
	ttl := cs.TTL()
	if ttl <= 0 {
		return
	}

	now := time.Now().UnixMilli()
	expiresAt := now + ttl.Milliseconds()

	clean := sanitizeCacheHeaders(upstreamHeaders)
	clean.Set("Content-Type", "application/json; charset=utf-8")
	clean.Set("Cache-Control", cs.cacheControlHeader())

	headersJSON, _ := json.Marshal(mapFromHeader(clean))

	err := database.UpsertCache(cs.writeDB, &models.TranslateCache{
		CacheKey:    cacheKey,
		Body:        body,
		HeadersJSON: string(headersJSON),
		CreatedAt:   now,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		log.Printf("[cache] put failed: %v", err)
	}
}

//
// ========================
// Cache-Control
// ========================
//

func (cs *CacheService) CacheControlHeader() string {
	return cs.cacheControlHeader()
}

func (cs *CacheService) cacheControlHeader() string {
	ttl := cs.TTL().Seconds()
	if ttl <= 0 {
		return "no-cache"
	}
	return fmt.Sprintf("public, max-age=%d", int(ttl))
}

//
// ========================
// Cleanup
// ========================
//

func (cs *CacheService) CleanExpired() {
	if err := database.CleanExpiredCache(
		cs.writeDB,
		cs.cfg.Cache.CleanupBatchSize,
		cs.cfg.Cache.CleanupMaxRounds,
	); err != nil {
		log.Printf("[cache] cleanup failed: %v", err)
	}
}

//
// ========================
// Helpers
// ========================
//

func IsNoCacheRequest(r *http.Request) bool {
	v := strings.TrimSpace(r.Header.Get("x-no-cache"))
	return v == "1" || strings.EqualFold(v, "true")
}

func sanitizeCacheHeaders(h http.Header) http.Header {
	clean := make(http.Header)
	for k, vs := range h {
		kl := strings.ToLower(k)
		if kl == "x-upstream-key-name" || kl == "set-cookie" || kl == "authorization" {
			continue
		}
		for _, v := range vs {
			clean.Add(k, v)
		}
	}
	return clean
}

func mapFromHeader(h http.Header) map[string][]string {
	m := make(map[string][]string, len(h))
	for k, vs := range h {
		m[k] = vs
	}
	return m
}
