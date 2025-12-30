package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestDiskCache(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	reportTTL := 1 * time.Hour
	unusedTTL := 24 * time.Hour
	refreshThreshold := 0.1

	cache, err := NewDiskCache(tmpDir, reportTTL, unusedTTL, refreshThreshold)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	coordinate := "pkg:maven/org.apache.commons/commons-lang3@3.12.0"
	payload := json.RawMessage(`{"vulnerabilities": []}`)
	now := time.Now()

	entry := &CacheEntry{
		Version:     1,
		Coordinate:  coordinate,
		RetrievedAt: now,
		LastUsed:    now,
		Payload:     payload,
	}

	// Test Write
	if err := cache.Write(entry); err != nil {
		t.Fatalf("failed to write entry: %v", err)
	}

	// Test Read
	readEntry, found, err := cache.Read(coordinate)
	if err != nil {
		t.Fatalf("failed to read entry: %v", err)
	}
	if !found {
		t.Fatal("entry not found")
	}
	if readEntry.Coordinate != coordinate {
		t.Errorf("expected coordinate %s, got %s", coordinate, readEntry.Coordinate)
	}

	// Test IsExpired
	if cache.IsExpired(readEntry, now.Add(30*time.Minute)) {
		t.Error("entry should not be expired")
	}
	if !cache.IsExpired(readEntry, now.Add(61*time.Minute)) {
		t.Error("entry should be expired")
	}

	// Test Touch
	touchTime := now.Add(10 * time.Minute)
	if err := cache.Touch(coordinate, touchTime); err != nil {
		t.Fatalf("failed to touch entry: %v", err)
	}
	readEntry, _, _ = cache.Read(coordinate)
	if !readEntry.LastAccessedAt.Equal(touchTime) {
		t.Errorf("expected LastAccessedAt %v, got %v", touchTime, readEntry.LastAccessedAt)
	}

	// Test MarkUsed
	usedTime := now.Add(20 * time.Minute)
	if err := cache.MarkUsed(coordinate, usedTime); err != nil {
		t.Fatalf("failed to mark used: %v", err)
	}
	readEntry, _, _ = cache.Read(coordinate)
	if !readEntry.LastUsed.Equal(usedTime) {
		t.Errorf("expected LastUsed %v, got %v", usedTime, readEntry.LastUsed)
	}

	// Test Scan for Refresh
	// refreshThreshold is 0.1 of 1 hour = 6 minutes.
	// If retrieved 55 minutes ago, remaining is 5 minutes < 6 minutes. Should refresh.
	scanTime := now.Add(55 * time.Minute)
	toRefresh, toEvict, err := cache.Scan(scanTime)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(toRefresh) != 1 || toRefresh[0] != coordinate {
		t.Errorf("expected 1 item to refresh, got %v", toRefresh)
	}
	if len(toEvict) != 0 {
		t.Errorf("expected 0 items to evict, got %v", toEvict)
	}

	// Test Scan for Eviction
	// unusedTTL is 24 hours.
	evictScanTime := usedTime.Add(25 * time.Hour)
	toRefresh, toEvict, err = cache.Scan(evictScanTime)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(toEvict) != 1 || toEvict[0] != coordinate {
		t.Errorf("expected 1 item to evict, got %v", toEvict)
	}

	// Test Evict
	if err := cache.Evict(coordinate); err != nil {
		t.Fatalf("failed to evict: %v", err)
	}
	_, found, _ = cache.Read(coordinate)
	if found {
		t.Error("entry should have been evicted")
	}
}

func TestDiskCacheScanMultiple(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache_scan_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	reportTTL := 1 * time.Hour
	unusedTTL := 24 * time.Hour
	cache, _ := NewDiskCache(tmpDir, reportTTL, unusedTTL, 0.1)

	now := time.Now()

	// 1. Fresh item
	cache.Write(&CacheEntry{Coordinate: "pkg:1", RetrievedAt: now, LastUsed: now})
	// 2. Item to refresh (retrieved 55m ago, TTL 1h, threshold 0.1=6m -> 5m left < 6m)
	cache.Write(&CacheEntry{Coordinate: "pkg:2", RetrievedAt: now.Add(-55 * time.Minute), LastUsed: now})
	// 3. Item to evict (last used 25h ago)
	cache.Write(&CacheEntry{Coordinate: "pkg:3", RetrievedAt: now.Add(-26 * time.Hour), LastUsed: now.Add(-25 * time.Hour)})

	toRefresh, toEvict, err := cache.Scan(now)
	if err != nil {
		t.Fatal(err)
	}

	if len(toRefresh) != 1 || toRefresh[0] != "pkg:2" {
		t.Errorf("expected [pkg:2] to refresh, got %v", toRefresh)
	}
	if len(toEvict) != 1 || toEvict[0] != "pkg:3" {
		t.Errorf("expected [pkg:3] to evict, got %v", toEvict)
	}
}

func TestDiskCacheWritePreserveLastUsed(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache_write_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cache, _ := NewDiskCache(tmpDir, time.Hour, time.Hour, 0.1)
	coord := "pkg:maven/test/test@1.0"
	lastUsed := time.Now().Add(-10 * time.Minute).Truncate(time.Second)

	// Initial write with LastUsed
	initial := &CacheEntry{
		Coordinate:  coord,
		RetrievedAt: time.Now().Add(-20 * time.Minute),
		LastUsed:    lastUsed,
	}
	cache.Write(initial)

	// Update without LastUsed (it's zero)
	update := &CacheEntry{
		Coordinate:  coord,
		RetrievedAt: time.Now(),
	}
	if err := cache.Write(update); err != nil {
		t.Fatal(err)
	}

	// Verify LastUsed was preserved
	read, _, _ := cache.Read(coord)
	if !read.LastUsed.Equal(lastUsed) {
		t.Errorf("expected LastUsed %v, got %v", lastUsed, read.LastUsed)
	}
}

func TestDiskCacheLockKey(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache_lock_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cache, _ := NewDiskCache(tmpDir, time.Hour, time.Hour, 0.1)
	coord := "pkg:maven/test/test@1.0"

	// 1. Exclusive vs Exclusive
	t.Run("ExclusiveExclusive", func(t *testing.T) {
		locked1 := make(chan struct{})
		unlocked1 := make(chan struct{})
		done1 := make(chan struct{})

		go func() {
			unlock := cache.lockKey(coord, true)
			close(locked1)
			<-unlocked1
			unlock()
			close(done1)
		}()

		<-locked1 // Wait for first lock

		locked2 := make(chan struct{})
		go func() {
			unlock := cache.lockKey(coord, true)
			close(locked2)
			unlock()
		}()

		select {
		case <-locked2:
			t.Fatal("Second exclusive lock acquired while first was held")
		case <-time.After(100 * time.Millisecond):
			// Expected to be blocked
		}

		close(unlocked1) // Release first lock
		<-done1

		select {
		case <-locked2:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Second exclusive lock not acquired after first released")
		}
	})

	// 2. Shared vs Shared
	t.Run("SharedShared", func(t *testing.T) {
		unlock1 := cache.lockKey(coord, false)
		defer unlock1()

		locked2 := make(chan struct{})
		go func() {
			unlock := cache.lockKey(coord, false)
			close(locked2)
			unlock()
		}()

		select {
		case <-locked2:
			// Success
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Second shared lock blocked by first shared lock")
		}
	})

	// 3. Shared vs Exclusive
	t.Run("SharedExclusive", func(t *testing.T) {
		unlock1 := cache.lockKey(coord, false)
		defer unlock1()

		locked2 := make(chan struct{})
		go func() {
			unlock := cache.lockKey(coord, true)
			close(locked2)
			unlock()
		}()

		select {
		case <-locked2:
			t.Fatal("Exclusive lock acquired while shared lock was held")
		case <-time.After(100 * time.Millisecond):
			// Expected to be blocked
		}
	})

	// 4. Exclusive vs Shared
	t.Run("ExclusiveShared", func(t *testing.T) {
		unlock1 := cache.lockKey(coord, true)

		locked2 := make(chan struct{})
		go func() {
			unlock := cache.lockKey(coord, false)
			close(locked2)
			unlock()
		}()

		select {
		case <-locked2:
			t.Fatal("Shared lock acquired while exclusive lock was held")
		case <-time.After(100 * time.Millisecond):
			// Expected to be blocked
		}

		unlock1()

		select {
		case <-locked2:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Shared lock not acquired after exclusive lock released")
		}
	})

	// 5. Multiple Shared vs Exclusive
	t.Run("MultipleSharedExclusive", func(t *testing.T) {
		unlock1 := cache.lockKey(coord, false)
		unlock2 := cache.lockKey(coord, false)

		lockedE := make(chan struct{})
		go func() {
			unlock := cache.lockKey(coord, true)
			close(lockedE)
			unlock()
		}()

		select {
		case <-lockedE:
			t.Fatal("Exclusive lock acquired while shared locks were held")
		case <-time.After(100 * time.Millisecond):
			// Blocked as expected
		}

		unlock1()

		select {
		case <-lockedE:
			t.Fatal("Exclusive lock acquired while one shared lock still held")
		case <-time.After(100 * time.Millisecond):
			// Blocked as expected
		}

		unlock2()

		select {
		case <-lockedE:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Exclusive lock not acquired after all shared locks released")
		}
	})
}

func TestDiskCachePersistence(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache_persist_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	coord := "pkg:maven/test/test@1.0"
	payload := json.RawMessage(`{"test": true}`)
	now := time.Now().Truncate(time.Second) // JSON marshaling might lose sub-second precision depending on how it's handled, but time.Time should be fine. Actually Go's JSON marshaling of time.Time uses RFC3339Nano.

	cache1, _ := NewDiskCache(tmpDir, time.Hour, time.Hour, 0.1)
	entry := &CacheEntry{
		Coordinate:  coord,
		RetrievedAt: now,
		Payload:     payload,
	}
	cache1.Write(entry)

	cache2, _ := NewDiskCache(tmpDir, time.Hour, time.Hour, 0.1)
	readEntry, found, err := cache2.Read(coord)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("entry not found after restart")
	}
	if readEntry.Coordinate != coord {
		t.Errorf("expected %s, got %s", coord, readEntry.Coordinate)
	}
}

func TestDiskCacheNotFound(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache_notfound_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cache, _ := NewDiskCache(tmpDir, time.Hour, time.Hour, 0.1)

	// 1. Never existed
	_, found, err := cache.Read("pkg:nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected found=false for non-existent entry")
	}

	// 2. Created then evicted
	coord := "pkg:maven/test/test@1.1"
	cache.Write(&CacheEntry{Coordinate: coord, RetrievedAt: time.Now()})

	if err := cache.Evict(coord); err != nil {
		t.Fatalf("evict failed: %v", err)
	}

	_, found, err = cache.Read(coord)
	if err != nil {
		t.Fatalf("unexpected error after evict: %v", err)
	}
	if found {
		t.Fatal("expected found=false after eviction")
	}

	// 3. Created then file deleted manually
	coord2 := "pkg:maven/test/test@1.2"
	cache.Write(&CacheEntry{Coordinate: coord2, RetrievedAt: time.Now()})

	path := cache.keyToPath(coord2)
	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to manually remove file: %v", err)
	}

	_, found, err = cache.Read(coord2)
	if err != nil {
		t.Fatalf("unexpected error after manual removal: %v", err)
	}
	if found {
		t.Fatal("expected found=false after manual file removal")
	}
}
