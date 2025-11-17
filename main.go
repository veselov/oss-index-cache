package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type loggingResponseWriter struct {
	w      http.ResponseWriter
	status int
	bytes  int
}

func (lrw *loggingResponseWriter) Header() http.Header  { return lrw.w.Header() }
func (lrw *loggingResponseWriter) WriteHeader(code int) { lrw.status = code; lrw.w.WriteHeader(code) }
func (lrw *loggingResponseWriter) Write(p []byte) (int, error) {
	if lrw.status == 0 {
		lrw.status = http.StatusOK
	}
	n, err := lrw.w.Write(p)
	lrw.bytes += n
	return n, err
}

func withLogging(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{w: w}
		// Get remote IP without port
		remote := r.RemoteAddr
		if host, _, err := net.SplitHostPort(remote); err == nil {
			remote = host
		}
		ua := r.Header.Get("User-Agent")
		next.ServeHTTP(lrw, r)
		dur := time.Since(start)
		status := lrw.status
		if status == 0 {
			status = http.StatusOK
		}
		path := r.URL.Path
		// collapse whitespace in UA to keep logs short
		ua = strings.Join(strings.Fields(ua), " ")
		logger.Printf("http request method=%s path=%s remote=%s ua=%q status=%d dur=%s bytes=%d", r.Method, path, remote, ua, status, dur, lrw.bytes)
	})
}

func main() {
	// Single required argument: path to YAML config file
	flag.Usage = func() {
		log.Printf("Usage: %s -config <path.yaml>", os.Args[0])
	}
	cfgPath := flag.String("config", "", "Path to YAML configuration file")
	flag.Parse()
	if *cfgPath == "" {
		log.Fatal("-config is required")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	logger := newAppLogger(cfg)

	cache, err := NewDiskCache(cfg)
	if err != nil {
		logger.Fatalf("cache init: %v", err)
	}
	authCache := newPatCache(cfg.Configuration.Auth.AuthCacheTTL, logger)

	// Server and routes
	srv := newServer(cfg, cache, authCache, logger)
	mux := http.NewServeMux()
	srv.routes(mux)

	httpSrv := &http.Server{
		Addr:         cfg.Configuration.Server.ListenAddr,
		Handler:      withLogging(mux, logger),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Maintenance: define refresh function using upstream client
	refreshFn := func(ctx context.Context, coords []string) map[string]error {
		client := &http.Client{Timeout: cfg.Configuration.Sonatype.Timeout}
		res, _, err := fetchFromUpstream(ctx, client, cfg.Configuration.Sonatype.Upstream, cfg.Configuration.Sonatype.Username, cfg.Configuration.Sonatype.APIKey, coords, logger)
		out := make(map[string]error, len(coords))
		now := time.Now()
		if err != nil {
			for _, c := range coords {
				out[c] = err
			}
			return out
		}
		for _, c := range coords {
			payload, ok := res[c]
			if !ok {
				out[c] = err
				continue
			}
			entry := &CacheEntry{Version: 2, Coordinate: c, RetrievedAt: now, Payload: payload}
			cache.Write(entry)
		}
		return out
	}
	startMaintenance(ctx, cfg, cache, refreshFn)

	// Run HTTP server
	done := make(chan struct{})
	go func() {
		logger.Printf("listening on %s", cfg.Configuration.Server.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("server error: %v", err)
		}
		close(done)
	}()

	// Graceful shutdown on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Printf("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	cancel()
	<-done
	logger.Printf("stopped")
}
