package main

import (
	"context"
	"log"
	"time"
)

// startMaintenance launches a background goroutine that periodically scans the cache
// to evict unused entries and refresh nearly expired entries by prefetching them.
func startMaintenance(ctx context.Context, cfg *Config, cache *DiskCache, logger *log.Logger, fetch func(ctx context.Context, coords []string) map[string]error) {
	interval := time.Minute // periodic check
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now()
				toRefresh, toEvict, err := cache.Scan(now)
				if err != nil {
					logger.Printf("maintenance scan error: %v", err)
					continue
				}
				// Evict
				for _, c := range toEvict {
					if err := cache.Evict(c); err != nil {
						logger.Printf("evict error coord=%s err=%v", c, err)
					} else {
						logger.Printf("evicted coord=%s", c)
					}
				}
				// Refresh in batches of 100
				batch := 100
				for i := 0; i < len(toRefresh); i += batch {
					j := i + batch
					if j > len(toRefresh) {
						j = len(toRefresh)
					}
					coords := toRefresh[i:j]
					ctx2, cancel := context.WithTimeout(ctx, cfg.Configuration.Sonatype.Timeout)
					errs := fetch(ctx2, coords)
					cancel()
					for c, e := range errs {
						if e != nil {
							logger.Printf("refresh error coord=%s err=%v", c, e)
						} else {
							logger.Printf("refreshed coord=%s", c)
						}
					}
				}
			}
		}
	}()
}
