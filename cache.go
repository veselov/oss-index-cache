package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type CacheEntry struct {
	Version        int             `json:"version"`
	Coordinate     string          `json:"coordinate"`
	RetrievedAt    time.Time       `json:"retrieved_at"`
	LastAccessedAt time.Time       `json:"last_accessed_at"`
	Payload        json.RawMessage `json:"payload"`
}

type DiskCache struct {
	dir         string
	reportTTL   time.Duration
	unusedTTL   time.Duration
	refreshThreshold float64
	mu          sync.Mutex            // guards per-key locking map
	locks       map[string]*sync.Mutex // per key
}

func NewDiskCache(dir string, reportTTL, unusedTTL time.Duration, refreshThreshold float64) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if refreshThreshold <= 0 { refreshThreshold = 0.05 }
	return &DiskCache{dir: dir, reportTTL: reportTTL, unusedTTL: unusedTTL, refreshThreshold: refreshThreshold, locks: make(map[string]*sync.Mutex)}, nil
}

func (c *DiskCache) keyToPath(coord string) string {
	sum := sha256.Sum256([]byte(coord))
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(c.dir, name)
}

func (c *DiskCache) lockKey(coord string) func() {
	c.mu.Lock()
	m, ok := c.locks[coord]
	if !ok {
		m = &sync.Mutex{}
		c.locks[coord] = m
	}
	c.mu.Unlock()
	m.Lock()
	return func() { m.Unlock() }
}

func (c *DiskCache) Read(coord string) (*CacheEntry, bool, error) {
	path := c.keyToPath(coord)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) { return nil, false, nil }
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil { return nil, false, err }
	var e CacheEntry
	if err := json.Unmarshal(b, &e); err != nil { return nil, false, err }
	// update last accessed in memory; caller must call Touch to persist update to avoid heavy writes per read
	return &e, true, nil
}

func (c *DiskCache) Write(e *CacheEntry) error {
	unlock := c.lockKey(e.Coordinate)
	defer unlock()
	path := c.keyToPath(e.Coordinate)
	// atomic write: temp then rename
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil { return err }
	if err := os.WriteFile(tmp, b, 0o600); err != nil { return err }
	return os.Rename(tmp, path)
}

func (c *DiskCache) Touch(coord string, when time.Time) error {
	unlock := c.lockKey(coord)
	defer unlock()
	path := c.keyToPath(coord)
	b, err := os.ReadFile(path)
	if err != nil { return err }
	var e CacheEntry
	if err := json.Unmarshal(b, &e); err != nil { return err }
	e.LastAccessedAt = when
	buf, err := json.MarshalIndent(&e, "", "  ")
	if err != nil { return err }
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil { return err }
	return os.Rename(tmp, path)
}

func (c *DiskCache) IsExpired(e *CacheEntry, now time.Time) bool {
	return e.RetrievedAt.Add(c.reportTTL).Before(now)
}

// Scan performs a full directory scan and returns coordinates needing refresh or eviction based on TTLs.
func (c *DiskCache) Scan(now time.Time) (toRefresh []string, toEvict []string, err error) {
	entries := []CacheEntry{}
	err = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil { return err }
		if d.IsDir() { return nil }
		if filepath.Ext(path) != ".json" { return nil }
		b, err := os.ReadFile(path)
		if err != nil { return err }
		var e CacheEntry
		if err := json.Unmarshal(b, &e); err != nil { return err }
		entries = append(entries, e)
		return nil
	})
	if err != nil { return nil, nil, err }
	for _, e := range entries {
		// eviction: last_accessed older than unusedTTL
		if e.LastAccessedAt.Add(c.unusedTTL).Before(now) {
			toEvict = append(toEvict, e.Coordinate)
			continue
		}
		// refresh: below threshold of remaining TTL
		age := now.Sub(e.RetrievedAt)
		if age < 0 { age = 0 }
		if c.reportTTL > 0 {
			remaining := c.reportTTL - age
			threshold := c.refreshThreshold
			if threshold <= 0 { threshold = 0.05 }
			if remaining < time.Duration(float64(c.reportTTL)*threshold) {
				toRefresh = append(toRefresh, e.Coordinate)
			}
		}
	}
	return toRefresh, toEvict, nil
}

func (c *DiskCache) Evict(coord string) error {
	unlock := c.lockKey(coord)
	defer unlock()
	path := c.keyToPath(coord)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("evict %s: %w", coord, err)
	}
	return nil
}
