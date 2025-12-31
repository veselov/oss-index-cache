package main

import (
	"context"
	"time"
)

func refresher(ctx context.Context, cfg *Config, fetch func(ctx context.Context, coords []string) map[string]error) chan []string {

	work := make(chan []string)

	go func() {

		for {
			select {
			case <-ctx.Done():
				return
			case coords := <-work:

				ctx2, cancel := context.WithTimeout(ctx, cfg.Configuration.Sonatype.Timeout)
				errs := fetch(ctx2, coords)
				cancel()
				for c, e := range errs {
					if e != nil {
						cfg.log.Printf("refresh error coord=%s err=%v", c, e)
					} else {
						cfg.log.Printf("refreshed coord=%s", c)
					}
				}

			}
		}

	}()

	return work

}

// startMaintenance launches a background goroutine that periodically scans the cache
// to evict unused entries and refresh nearly expired entries by prefetching them.
func startMaintenance(ctx context.Context, cfg *Config, cache *DiskCache, fetch func(ctx context.Context, coords []string) map[string]error) {
	startMaintenanceInterval(ctx, cfg, cache, fetch, time.Minute)
}

func startMaintenanceInterval(ctx context.Context, cfg *Config, cache *DiskCache, fetch func(ctx context.Context, coords []string) map[string]error, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()

		wCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		refresher := refresher(wCtx, cfg, fetch)

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cfg.log.Printf("running maintenance scan")
				now := time.Now()
				var toRefresh []string
				err := cache.Scan(now, func(e *CacheEntry) error {
					cfg.log.Printf("Slating maintenance refresh for %s:%s", e.Coordinate, *e.path)
					toRefresh = append(toRefresh, e.Coordinate)
					if len(toRefresh) >= cfg.Configuration.Cache.BatchSize {
						refresher <- toRefresh
						toRefresh = []string{}
					}
					return nil
				})

				if len(toRefresh) > 0 {
					refresher <- toRefresh
				}

				if err != nil {
					cfg.log.Printf("maintenance scan error: %v", err)
				}
			}
		}
	}()
}
