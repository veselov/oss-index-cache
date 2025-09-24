package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type patCacheEntry struct {
	verifiedAt time.Time
	expiresAt  time.Time
}

type patCache struct {
	mu     sync.RWMutex
	m      map[string]patCacheEntry // key = hashed PAT
	ttl    time.Duration
	logger *log.Logger
}

func newPatCache(ttl time.Duration, logger *log.Logger) *patCache {
	if logger == nil {
		panic("auth: logger must not be nil")
	}
	return &patCache{m: make(map[string]patCacheEntry), ttl: ttl, logger: logger}
}

func (c *patCache) key(pat string) string {
	// store hashed to avoid keeping raw PATs in memory
	h := sha256.Sum256([]byte(pat))
	return base64.StdEncoding.EncodeToString(h[:])
}

func (c *patCache) get(pat string) bool {
	k := c.key(pat)
	c.mu.RLock()
	e, ok := c.m[k]
	c.mu.RUnlock()
	if !ok {
		c.logger.Printf("auth cache miss")
		return false
	}
	if time.Now().Before(e.expiresAt) {
		c.logger.Printf("auth cache hit")
		return true
	}
	c.logger.Printf("auth cache expired")
	return false
}

func (c *patCache) put(pat string) {
	k := c.key(pat)
	now := time.Now()
	c.mu.Lock()
	c.m[k] = patCacheEntry{verifiedAt: now, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	c.logger.Printf("auth cache store ttl=%s", c.ttl)
}

// verifyPAT checks cache, and if miss, verifies against GitLab API
func verifyPAT(ctx context.Context, httpClient *http.Client, gitlabBaseURL, pat string, cache *patCache) error {
	if pat == "" {
		return errors.New("empty PAT")
	}
	if cache.get(pat) {
		cache.logger.Printf("auth PAT verified via cache")
		return nil
	}
	// call GitLab
	cache.logger.Printf("auth verifying PAT against GitLab base=%s", strings.TrimRight(gitlabBaseURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gitlabBaseURL, "/")+"/api/v4/user", nil)
	if err != nil {
		cache.logger.Printf("auth verify request build error: %v", err)
		return err
	}
	// Use PRIVATE-TOKEN header by default
	req.Header.Set("PRIVATE-TOKEN", pat)
	resp, err := httpClient.Do(req)
	if err != nil {
		cache.logger.Printf("auth gitlab verify error: %v", err)
		return fmt.Errorf("gitlab verify: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		cache.put(pat)
		cache.logger.Printf("auth PAT verified by GitLab")
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		cache.logger.Printf("auth invalid PAT status=%d", resp.StatusCode)
		return errors.New("invalid PAT")
	}
	cache.logger.Printf("auth gitlab verify unexpected status=%d", resp.StatusCode)
	return fmt.Errorf("gitlab verify unexpected status: %d", resp.StatusCode)
}

// extractBasicAuth extracts username and password from Authorization header. Returns PAT as password.
func extractBasicAuth(r *http.Request) (user string, pat string, ok bool) {
	user, pat, ok = r.BasicAuth()
	if ok && user != "" && pat != "" {
		return user, pat, true
	}
	// Also support Authorization: Basic manually if needed
	a := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(a), "basic ") {
		b64 := strings.TrimSpace(a[6:])
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err == nil {
			parts := strings.SplitN(string(raw), ":", 2)
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				return parts[0], parts[1], true
			}
		}
	}
	return "", "", false
}
