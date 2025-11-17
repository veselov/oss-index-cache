package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"testing"
	"time"
)

func TestRefresher(t *testing.T) {
	cfg := &Config{}
	cfg.Configuration.Sonatype.Timeout = 100 * time.Millisecond
	cfg.log = log.New(os.Stdout, "test-refresher: ", 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fetchedCoords := make(chan []string, 1)
	fetch := func(ctx context.Context, coords []string) map[string]error {
		fetchedCoords <- coords
		res := make(map[string]error)
		for _, c := range coords {
			res[c] = nil
		}
		return res
	}

	work := refresher(ctx, cfg, fetch)

	testCoords := []string{"pkg:maven/a/b@1", "pkg:maven/c/d@2"}
	work <- testCoords

	select {
	case got := <-fetchedCoords:
		if len(got) != len(testCoords) {
			t.Errorf("expected %d coords, got %d", len(testCoords), len(got))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for fetch to be called")
	}
}

func TestStartMaintenance(t *testing.T) {
	dir, err := os.MkdirTemp("", "maintenance-test-*")
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

	// Populate with pre-existing entries
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

	// 3. Entry unused (to be evicted)
	unusedTime := now.Add(-2 * time.Hour)
	cache.Write(&CacheEntry{
		Coordinate:     "unused",
		RetrievedAt:    now,
		LastAccessedAt: &unusedTime,
		Payload:        json.RawMessage(`{}`),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	refreshed := make(map[string]bool)
	refreshCalled := make(chan bool, 1)

	fetch := func(ctx context.Context, coords []string) map[string]error {
		mu.Lock()
		for _, c := range coords {
			refreshed[c] = true
		}
		mu.Unlock()
		refreshCalled <- true
		return nil
	}

	// Use a very short interval for testing
	startMaintenanceInterval(ctx, cfg, cache, fetch, 100*time.Millisecond)

	// Wait for at least one maintenance cycle
	select {
	case <-refreshCalled:
		// Success: fetch was called
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for maintenance refresh")
	}

	mu.Lock()
	defer mu.Unlock()

	if refreshed["fresh"] {
		t.Error("fresh entry should not have been refreshed")
	}
	if !refreshed["refresh"] {
		t.Error("entry needing refresh was not refreshed")
	}
	if refreshed["unused"] {
		t.Error("unused entry should have been evicted, not refreshed")
	}

	// Verify 'unused' is gone from disk
	if _, err := os.Stat(*cache.keyToPath("unused")); !os.IsNotExist(err) {
		t.Error("unused entry should have been evicted from disk")
	}
}

func TestMaintenanceVersion1AndBatching(t *testing.T) {
	dir, err := os.MkdirTemp("", "maintenance-v1-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := createTestConfig(dir)
	cfg.Configuration.Cache.RefreshTTL = 10 * time.Minute
	cfg.Configuration.Cache.UnusedTTL = 1 * time.Hour
	cfg.Configuration.Cache.BatchSize = 10 // Small batch size for testing

	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cache.log = cfg.log

	// 1. Write 105 entries that need refresh (to test batching with size 10)
	now := time.Now()
	refreshTime := now.Add(-15 * time.Minute)
	for i := 0; i < 105; i++ {
		coord := "pkg:maven/batch/test@" + string(rune('a'+i%26)) + "-" + string(rune('0'+i%10)) + "-" + string(rune(i))
		cache.Write(&CacheEntry{
			Coordinate:     coord,
			RetrievedAt:    refreshTime,
			LastAccessedAt: &now,
			Payload:        json.RawMessage(`{}`),
		})
	}

	// 2. Write some version 1 entries as requested
	v1Content := `{
  "version": 1,
  "coordinate": "pkg:maven/com.excelfore/appshack-api.server@6.4.1-20251119.161846-11",
  "retrieved_at": "2025-12-30T23:37:26.490420505Z",
  "last_accessed_at": "2025-12-30T23:37:26.490420505Z",
  "payload": {
    "coordinates": "pkg:maven/com.excelfore/appshack-api.server@6.4.1-20251119.161846-11",
    "reference": "https://ossindex.sonatype.org/component/pkg:maven/com.excelfore/appshack-api.server@6.4.1-20251119.161846-11?utm_source=go-http-client\u0026utm_medium=integration\u0026utm_content=2.0",
    "vulnerabilities": []
  }
}`
	v1Coord := "pkg:maven/com.excelfore/appshack-api.server@6.4.1-20251119.161846-11"
	err = os.WriteFile(*cache.keyToPath(v1Coord), []byte(v1Content), 0600)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	refreshCount := 0
	batches := 0

	fetch := func(ctx context.Context, coords []string) map[string]error {
		mu.Lock()
		refreshCount += len(coords)
		batches++
		mu.Unlock()
		return nil
	}

	startMaintenanceInterval(ctx, cfg, cache, fetch, 100*time.Millisecond)

	// Wait for enough refreshes
	start := time.Now()
	for {
		mu.Lock()
		count := refreshCount
		mu.Unlock()
		if count >= 105 {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("timeout waiting for batch refreshes, got %d", count)
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	if batches < 11 {
		t.Errorf("expected at least 11 batches for 105 items with batch size 10, got %d", batches)
	}
	mu.Unlock()

	// Verify version 1 entry is gone
	if _, err := os.Stat(*cache.keyToPath(v1Coord)); !os.IsNotExist(err) {
		t.Error("version 1 entry should have been evicted by ScanItem (via ReadLocked)")
	}
}
