package main

import (
	"encoding/json"
	"log"
	"os"
	"testing"
	"time"
)

func createTestConfig(dir string) *Config {
	cfg := &Config{}
	cfg.Configuration.Cache.Directory = dir
	cfg.Configuration.Cache.RefreshTTL = 1 * time.Hour
	cfg.Configuration.Cache.ExpireTTL = 24 * time.Hour
	cfg.Configuration.Cache.UnusedTTL = 7 * 24 * time.Hour
	cfg.log = log.New(os.Stdout, "test: ", 0)
	return cfg
}

func TestDiskCacheCRUD(t *testing.T) {
	dir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	coord := "pkg:maven/org.apache.commons/commons-lang3@3.12.0"
	payload := json.RawMessage(`{"vulnerabilities": []}`)
	now := time.Now()

	// Test Write
	entry := &CacheEntry{
		Coordinate:     coord,
		RetrievedAt:    now,
		LastAccessedAt: &now,
		Payload:        payload,
	}
	cache.Write(entry)

	// Test Read
	readEntry := cache.Read(coord)
	if readEntry == nil {
		t.Fatal("expected entry to be found")
	}
	if readEntry.Version != 2 {
		t.Errorf("expected version 2, got %d", readEntry.Version)
	}
	if readEntry.Coordinate != coord {
		t.Errorf("expected coord %s, got %s", coord, readEntry.Coordinate)
	}

	var expectedPayload, actualPayload interface{}
	if err := json.Unmarshal(payload, &expectedPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(readEntry.Payload, &actualPayload); err != nil {
		t.Fatal(err)
	}

	expectedStr, _ := json.Marshal(expectedPayload)
	actualStr, _ := json.Marshal(actualPayload)

	if string(expectedStr) != string(actualStr) {
		t.Errorf("expected payload %s, got %s", string(expectedStr), string(actualStr))
	}

	// Test Read Non-existent
	if nilEntry := cache.Read("non-existent"); nilEntry != nil {
		t.Error("expected nil for non-existent entry")
	}
}

func TestDiskCacheExpiration(t *testing.T) {
	dir, err := os.MkdirTemp("", "cache-test-exp-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cfg.Configuration.Cache.ExpireTTL = 1 * time.Second
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	coord := "pkg:maven/exp/test@1.0.0"
	now := time.Now().Add(-2 * time.Second) // Already expired
	entry := &CacheEntry{
		Coordinate:  coord,
		RetrievedAt: now,
		Payload:     json.RawMessage(`{}`),
	}
	// We need to set LastAccessedAt if we want Write to not try to read it back
	at := now
	entry.LastAccessedAt = &at

	cache.Write(entry)

	readEntry := cache.Read(coord)
	if readEntry != nil {
		t.Error("expected entry to be expired and nil returned")
	}

	// Verify file is removed
	path := *cache.keyToPath(coord)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected expired file to be removed from disk")
	}
}

func TestDiskCacheLocking(t *testing.T) {
	dir, err := os.MkdirTemp("", "cache-test-lock-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	key := "test-key"

	t.Run("ExclusiveLockExclusivity", func(t *testing.T) {
		unlock1 := cache.lockKey(key, true)

		locked := make(chan bool, 1)
		go func() {
			unlock2 := cache.lockKey(key, true)
			locked <- true
			unlock2()
		}()

		select {
		case <-locked:
			t.Fatal("second exclusive lock acquired while first was held")
		case <-time.After(100 * time.Millisecond):
			// Success: blocked
		}

		unlock1()

		select {
		case <-locked:
			// Success: acquired after unlock
		case <-time.After(1 * time.Second):
			t.Fatal("second exclusive lock not acquired after first was released")
		}
	})

	t.Run("SharedLockConcurrency", func(t *testing.T) {
		keyShared := "shared-key"
		unlock1 := cache.lockKey(keyShared, false)

		locked := make(chan bool, 1)
		go func() {
			unlock2 := cache.lockKey(keyShared, false)
			locked <- true
			unlock2()
		}()

		select {
		case <-locked:
			// Success: both shared locks held
		case <-time.After(500 * time.Millisecond):
			t.Fatal("shared lock blocked by another shared lock")
		}
		unlock1()
	})

	t.Run("SharedVsExclusive", func(t *testing.T) {
		keyMixed := "mixed-key"
		unlockS := cache.lockKey(keyMixed, false)

		lockedE := make(chan bool, 1)
		go func() {
			unlockE := cache.lockKey(keyMixed, true)
			lockedE <- true
			unlockE()
		}()

		select {
		case <-lockedE:
			t.Fatal("exclusive lock acquired while shared lock was held")
		case <-time.After(100 * time.Millisecond):
			// Success: blocked
		}

		unlockS()

		select {
		case <-lockedE:
			// Success
		case <-time.After(1 * time.Second):
			t.Fatal("exclusive lock not granted after shared released")
		}
	})
}

func TestDiskCacheScan(t *testing.T) {
	dir, err := os.MkdirTemp("", "cache-test-scan-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cfg.Configuration.Cache.RefreshTTL = 10 * time.Minute
	cfg.Configuration.Cache.UnusedTTL = 1 * time.Hour
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	now := time.Now()

	// 1. Fresh entry
	cache.Write(&CacheEntry{
		Coordinate:     "fresh",
		RetrievedAt:    now,
		LastAccessedAt: &now,
		Payload:        json.RawMessage(`{}`),
	})

	// 2. Entry needing refresh
	refreshTime := now.Add(-15 * time.Minute)
	cache.Write(&CacheEntry{
		Coordinate:     "refresh",
		RetrievedAt:    refreshTime,
		LastAccessedAt: &now,
		Payload:        json.RawMessage(`{}`),
	})

	// 3. Entry unused (evict)
	unusedTime := now.Add(-2 * time.Hour)
	cache.Write(&CacheEntry{
		Coordinate:     "unused",
		RetrievedAt:    now,
		LastAccessedAt: &unusedTime,
		Payload:        json.RawMessage(`{}`),
	})

	refreshed := make(map[string]bool)
	err = cache.Scan(now, func(e *CacheEntry) error {
		refreshed[e.Coordinate] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if refreshed["fresh"] {
		t.Error("fresh entry should not be in refresh list")
	}
	if !refreshed["refresh"] {
		t.Error("entry needing refresh not found")
	}
	if refreshed["unused"] {
		t.Error("unused entry should be evicted, not refreshed")
	}

	// Verify 'unused' is gone from disk
	if readUnused := cache.Read("unused"); readUnused != nil {
		t.Error("unused entry should have been evicted")
	}
}

func TestDiskCacheWriteRefreshRace(t *testing.T) {
	dir, err := os.MkdirTemp("", "cache-test-race-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	coord := "race-coord"

	// Test the case where Write is called with LastAccessedAt == nil (simulating refresh)
	// and the entry is missing from disk.
	entry := &CacheEntry{
		Coordinate:  coord,
		RetrievedAt: time.Now(),
		Payload:     json.RawMessage(`{}`),
	}

	cache.Write(entry)

	read := cache.Read(coord)
	if read == nil {
		t.Fatal("expected entry to be written even if missing during refresh-write")
	}
	if read.LastAccessedAt == nil {
		t.Fatal("LastAccessedAt should have been initialized")
	}
}

func TestDiskCacheCorruptFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "cache-test-corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	coord := "corrupt"
	path := *cache.keyToPath(coord)

	err = os.WriteFile(path, []byte("invalid json"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	read := cache.Read(coord)
	if read != nil {
		t.Error("expected nil for corrupt file")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected corrupt file to be removed")
	}
}

func TestDiskCacheVersionMigration(t *testing.T) {
	dir, err := os.MkdirTemp("", "cache-test-version-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	coord := "pkg:maven/version/test@1.0.0"
	path := *cache.keyToPath(coord)

	// Manually write a version 1 entry
	v1Entry := map[string]interface{}{
		"version":          1,
		"coordinate":       coord,
		"retrieved_at":     time.Now().Format(time.RFC3339Nano),
		"last_accessed_at": time.Now().Format(time.RFC3339Nano),
		"payload":          map[string]interface{}{},
	}
	b, _ := json.Marshal(v1Entry)
	os.WriteFile(path, b, 0600)

	// Read should return nil and remove the file
	read := cache.Read(coord)
	if read != nil {
		t.Error("expected nil for version 1 entry")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected version 1 file to be removed")
	}

	// Write should now set version 2
	newEntry := &CacheEntry{
		Coordinate:  coord,
		RetrievedAt: time.Now(),
		Payload:     json.RawMessage(`{}`),
	}
	cache.Write(newEntry)

	read2 := cache.Read(coord)
	if read2 == nil {
		t.Fatal("expected entry to be written and read back")
	}
	if read2.Version != 2 {
		t.Errorf("expected version 2, got %d", read2.Version)
	}
}
