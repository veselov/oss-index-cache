package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type CacheEntry struct {
	path           *string
	Version        int             `json:"version"`
	Coordinate     string          `json:"coordinate"`
	RetrievedAt    time.Time       `json:"retrieved_at"`
	LastAccessedAt *time.Time      `json:"last_accessed_at"`
	Payload        json.RawMessage `json:"payload"`
}

type DiskCache struct {
	dir              string
	expireTTL        time.Duration
	refreshTTL       time.Duration
	unusedTTL        time.Duration
	refreshThreshold float64
	cond             *sync.Cond
	locked           map[string]uint // Locked keys. Value: 0 -> exclusive, >0 -> shared, nil -> unlocked
	log              *log.Logger
}

func NewDiskCache(cfg *Config) (*DiskCache, error) {

	dir := cfg.Configuration.Cache.Directory
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	return &DiskCache{
		dir:        dir,
		refreshTTL: cfg.Configuration.Cache.RefreshTTL,
		expireTTL:  cfg.Configuration.Cache.ExpireTTL,
		unusedTTL:  cfg.Configuration.Cache.UnusedTTL,
		cond:       sync.NewCond(&sync.Mutex{}),
		locked:     make(map[string]uint),
	}, nil
}

func (c *DiskCache) keyToPath(coord string) *string {
	sum := sha256.Sum256([]byte(coord))
	name := hex.EncodeToString(sum[:])
	p := filepath.Join(c.dir, name+".json")
	return &p
}

func (c *DiskCache) lockKey(coord string, exclusive bool) func() {

	c.cond.L.Lock()
	defer c.cond.L.Unlock()

	for {

		lock, present := c.locked[coord]

		if !present || (!exclusive && lock > 0) {

			if exclusive {
				c.locked[coord] = 0
			} else {
				if lock == ^uint(0) {
					c.log.Panicf("Lock wrap-around for %s", coord)
				}
				c.locked[coord] = lock + 1
			}

			var once sync.Once

			return func() {
				once.Do(func() {
					c.cond.L.Lock()
					defer c.cond.L.Unlock()
					lock, present := c.locked[coord]
					if !present {
						c.log.Panicf("Lock for %s has no lock on unlock", coord)
					}
					if exclusive {
						if lock != 0 {
							c.log.Panicf("Exclusive lock %s with !0 value %d", coord, lock)
						}
						delete(c.locked, coord)
					} else {
						if lock < 1 {
							c.log.Panicf("Shared lock %s with value %d out-of-bounds", coord, lock)
						}
						if lock == 1 {
							delete(c.locked, coord)
						} else {
							c.locked[coord] = lock - 1
						}
					}
					c.cond.Broadcast()
				})
			}

		}

		c.cond.Wait()

	}

}

func (c *DiskCache) ReadLocked(path string, access bool) *CacheEntry {

	remove := func() {
		err := os.Remove(path)
		if err != nil {
			c.log.Printf("failed to remove corrupt cache entry %s: %v", path, err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.log.Printf("failed to read cache entry %s: %v", path, err)
		}
		return nil
	}

	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		c.log.Printf("failed to read cache entry %s: %v", path, err)
		return nil
	}

	var e CacheEntry
	if err := json.Unmarshal(b, &e); err != nil {
		c.log.Printf("read %s: %v", path, err)
		remove()
		return nil
	}
	e.path = &path

	if e.Version < 2 {
		c.log.Printf("invalid version %d for %s", e.Version, e.Coordinate)
		remove()
		return nil
	}

	if e.RetrievedAt.Add(c.expireTTL).Before(time.Now()) {
		c.log.Printf("expired %s", e.Coordinate)
		remove()
		return nil
	}

	if access {
		t := time.Now()
		e.LastAccessedAt = &t
		// c.Write(&e) // DEADLOCK: Read calls Write, both try to lock!
		// We should call something that doesn't re-lock or just do the write here.
		c.writeLocked(&e)
	}

	return &e
}

func (c *DiskCache) Read(coord string) *CacheEntry {

	path := c.keyToPath(coord)

	// Since ReadLocked with access=true calls writeLocked, which needs an EXCLUSIVE lock,
	// and Read calls ReadLocked(access=true), we MUST use an exclusive lock here too.
	// If we used a shared lock, writeLocked would be operating under a shared lock,
	// which is a race condition.
	unlock := c.lockKey(*path, true)
	defer unlock()

	return c.ReadLocked(*path, true)
}

func (c *DiskCache) Write(e *CacheEntry) {

	if e.path == nil {
		e.path = c.keyToPath(e.Coordinate)
	}

	unlock := c.lockKey(*e.path, true)
	defer unlock()

	c.writeLocked(e)
}

func (c *DiskCache) writeLocked(e *CacheEntry) {

	e.Version = 2

	if e.LastAccessedAt == nil {
		// write request is from a refresh operation
		old := c.ReadLocked(*e.path, false)
		if old == nil {
			// this is a race condition. While we were refreshing,
			// the entry was evicted. Probably because it was deemed unused.
			// We have two choices - put it back in and assume last access time = now
			// or not put it back in because it probably doesn't belong.
			// We choose the former because it will benefit the clients at the expense
			// of the cache size.
			now := time.Now()
			e.LastAccessedAt = &now
		} else {
			e.LastAccessedAt = old.LastAccessedAt
		}
	}

	// write atomically: temp then rename
	tmp := *e.path + ".tmp"
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		c.log.Printf("failed to marshal cache entry %s: %v", e.Coordinate, err)
		return
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		c.log.Printf("failed to write cache entry %s (%s): %v", e.Coordinate, tmp, err)
		return
	}
	err = os.Rename(tmp, *e.path)
	if err != nil {
		c.log.Printf("failed to rename cache entry %s %s->%s: %v", e.Coordinate, tmp, *e.path, err)
	}
}

func (c *DiskCache) ScanItem(now time.Time, path string, refresh func(e *CacheEntry) error) error {

	unlock := c.lockKey(path, true)
	defer unlock()

	e := c.ReadLocked(path, false)
	if e == nil {
		return nil
	}

	// Expired is handled by ReadLocked()

	// Handle unused here
	if e.LastAccessedAt.Add(c.unusedTTL).Before(now) {
		if err := os.Remove(path); err != nil {
			c.log.Printf("failed to remove unused cache entry %s: %v", path, err)
			return err
		}
		return nil
	}

	// Handle refresh
	if (e.RetrievedAt.Add(c.refreshTTL)).Before(now) {
		return refresh(e)
	}

	return nil

}

// Scan performs a full directory scan and invokes fn for every element needing refresh or eviction.
func (c *DiskCache) Scan(now time.Time, refresh func(e *CacheEntry) error) error {
	return filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {

		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if filepath.Ext(path) != ".json" {
			return nil
		}

		return c.ScanItem(now, path, refresh)

	})
}
