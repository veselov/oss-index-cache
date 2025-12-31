package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

type componentRequest struct {
	Coordinates []string `json:"coordinates"`
}

type errorStub struct {
	Coordinate string `json:"coordinate"`
	Error      struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type server struct {
	cfg    *Config
	cache  *DiskCache
	auth   *patCache
	client *http.Client
	logger *log.Logger
}

func newServer(cfg *Config, cache *DiskCache, auth *patCache, logger *log.Logger) *server {
	client := &http.Client{Timeout: cfg.Configuration.Server.RequestTimeout}
	return &server{cfg: cfg, cache: cache, auth: auth, client: client, logger: logger}
}

func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v3/component-report", s.handleComponentReport)
}

func (s *server) handleComponentReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Basic Auth
	_, pat, ok := r.BasicAuth()
	if !ok {
		s.logger.Printf("No basic auth found in the request")
		w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%s", s.cfg.Configuration.Auth.GitLabBaseURL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Configuration.Server.RequestTimeout)
	defer cancel()
	gitlabClient := &http.Client{Timeout: s.cfg.Configuration.Server.RequestTimeout}
	if err := verifyPAT(ctx, gitlabClient, s.cfg.Configuration.Auth.GitLabBaseURL, pat, s.auth); err != nil {
		s.logger.Printf("auth PAT verification failed: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Parse body
	var req componentRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil || len(req.Coordinates) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	coords := req.Coordinates
	// Prepare lookup
	now := time.Now()
	type idx struct {
		index int
		coord string
	}
	ordered := make([]idx, 0, len(coords))
	for i, c := range coords {
		ordered = append(ordered, idx{i, c})
	}
	// de-duplicate for cache lookup
	unique := map[string]struct{}{}
	for _, c := range coords {
		unique[c] = struct{}{}
	}

	cacheHits := make(map[string]json.RawMessage)
	missing := make([]string, 0)
	for c := range unique {
		if entry := s.cache.Read(c); entry != nil {
			cacheHits[c] = entry.Payload
		} else {
			missing = append(missing, c)
		}
	}
	// sort missing for stable upstream body (not required but nice)
	sort.Strings(missing)

	// Fetch missing
	upstreamResults := map[string]json.RawMessage{}
	if len(missing) > 0 {
		uctx, ucancel := context.WithTimeout(ctx, s.cfg.Configuration.Sonatype.Timeout)
		defer ucancel()
		s.logger.Printf("Requesting from upstream: %s", strings.Join(missing, ", "))
		res, status, err := fetchFromUpstream(uctx, s.client, s.cfg.Configuration.Sonatype.Upstream, s.cfg.Configuration.Sonatype.Username, s.cfg.Configuration.Sonatype.APIKey, missing, s.logger)
		if err != nil {
			// Any upstream retrieval failure should result in a 502 response per requirement.
			s.logger.Printf("upstream error status=%d err=%v", status, err)
			w.WriteHeader(http.StatusBadGateway)
			return
		} else {
			upstreamResults = res
		}
	}
	// Persist upstream results
	for coord, payload := range upstreamResults {
		entry := &CacheEntry{Version: 1, Coordinate: coord, RetrievedAt: now, LastAccessedAt: &now, Payload: payload}
		s.cache.Write(entry)
	}

	// Merge results and verify completeness before writing any response
	merged := make(map[string]json.RawMessage, len(cacheHits)+len(upstreamResults))
	for k, v := range cacheHits {
		merged[k] = v
	}
	for k, v := range upstreamResults {
		merged[k] = v
	}

	// Ensure every requested coordinate has a resolved payload; otherwise return 502
	for _, it := range ordered {
		if _, ok := merged[it.coord]; !ok {
			s.logger.Printf("incomplete response: missing coordinate %q after cache+upstream; returning 502", it.coord)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	}

	// Build response preserving order (now guaranteed complete)
	out := make([]json.RawMessage, 0, len(coords))
	for _, it := range ordered {
		out = append(out, merged[it.coord])
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(out); err != nil {
		// best effort write
		return
	}
}
