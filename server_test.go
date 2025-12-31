package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestServerHandleComponentReport(t *testing.T) {
	// Setup mock server for GitLab and Sonatype
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v4/user") {
			// GitLab Mock
			if r.Header.Get("PRIVATE-TOKEN") == "valid-token" {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"username": "testuser"}`)
			} else if r.Header.Get("PRIVATE-TOKEN") == "slow-token" {
				time.Sleep(2 * time.Second)
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusUnauthorized)
			}
			return
		}

		if strings.HasSuffix(r.URL.Path, "/api/v3/component-report") {
			// Sonatype Mock
			var req upstreamRequest
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &req)

			if len(req.Coordinates) > 0 && req.Coordinates[0] == "slow-pkg" {
				time.Sleep(2 * time.Second)
			}

			results := []map[string]interface{}{}
			for _, c := range req.Coordinates {
				results = append(results, map[string]interface{}{
					"coordinates":     c,
					"vulnerabilities": []interface{}{},
				})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(results)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	dir, err := os.MkdirTemp("", "server-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfg := &Config{}
	cfg.Configuration.Cache.Directory = dir
	cfg.Configuration.Cache.RefreshTTL = 1 * time.Hour
	cfg.Configuration.Cache.ExpireTTL = 24 * time.Hour
	cfg.Configuration.Cache.UnusedTTL = 7 * 24 * time.Hour
	cfg.Configuration.Auth.GitLabBaseURL = mockServer.URL
	cfg.Configuration.Auth.AuthCacheTTL = 1 * time.Hour
	cfg.Configuration.Sonatype.Upstream = mockServer.URL + "/api/v3/component-report"
	cfg.Configuration.Sonatype.Username = "user"
	cfg.Configuration.Sonatype.APIKey = "key"
	cfg.Configuration.Sonatype.Timeout = 1 * time.Second
	cfg.Configuration.Server.RequestTimeout = 1 * time.Second
	cfg.log = log.New(os.Stdout, "test: ", 0)

	cache, _ := NewDiskCache(cfg)
	cache.log = cfg.log
	auth := newPatCache(cfg.Configuration.Auth.AuthCacheTTL, cfg.log)
	s := newServer(cfg, cache, auth, cfg.log)

	t.Run("SuccessRequest", func(t *testing.T) {
		reqBody, _ := json.Marshal(componentRequest{Coordinates: []string{"pkg:maven/org.apache.commons/commons-lang3@3.12.0"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v3/component-report", bytes.NewReader(reqBody))
		req.SetBasicAuth("user", "valid-token")
		w := httptest.NewRecorder()

		s.handleComponentReport(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp []json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp) != 1 {
			t.Errorf("expected 1 result, got %d", len(resp))
		}
	})

	t.Run("UnauthorizedRequest", func(t *testing.T) {
		reqBody, _ := json.Marshal(componentRequest{Coordinates: []string{"pkg:maven/org.apache.commons/commons-lang3@3.12.0"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v3/component-report", bytes.NewReader(reqBody))
		req.SetBasicAuth("user", "invalid-token")
		w := httptest.NewRecorder()

		s.handleComponentReport(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected status 401, got %d", w.Code)
		}
	})

	t.Run("BadRequest", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v3/component-report", bytes.NewReader([]byte("invalid json")))
		req.SetBasicAuth("user", "valid-token")
		w := httptest.NewRecorder()

		s.handleComponentReport(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})

	t.Run("GitLabTimeout", func(t *testing.T) {
		reqBody, _ := json.Marshal(componentRequest{Coordinates: []string{"pkg:maven/org.apache.commons/commons-lang3@3.12.0"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v3/component-report", bytes.NewReader(reqBody))
		req.SetBasicAuth("user", "slow-token")
		w := httptest.NewRecorder()

		start := time.Now()
		s.handleComponentReport(w, req)
		duration := time.Since(start)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected status 401 (timeout in verifyPAT returns error), got %d", w.Code)
		}
		if duration < cfg.Configuration.Server.RequestTimeout {
			t.Errorf("request finished too early: %v", duration)
		}
		if duration > cfg.Configuration.Server.RequestTimeout+500*time.Millisecond {
			t.Errorf("request took too long: %v", duration)
		}
	})

	t.Run("UpstreamTimeout", func(t *testing.T) {
		reqBody, _ := json.Marshal(componentRequest{Coordinates: []string{"slow-pkg"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v3/component-report", bytes.NewReader(reqBody))
		req.SetBasicAuth("user", "valid-token")
		w := httptest.NewRecorder()

		start := time.Now()
		s.handleComponentReport(w, req)
		duration := time.Since(start)

		if w.Code != http.StatusBadGateway {
			t.Errorf("expected status 502, got %d", w.Code)
		}
		// Sonatype timeout is 1s, RequestTimeout is 1s.
		if duration < cfg.Configuration.Sonatype.Timeout {
			t.Errorf("request finished too early: %v", duration)
		}
		if duration > cfg.Configuration.Sonatype.Timeout+500*time.Millisecond {
			t.Errorf("request took too long: %v", duration)
		}
	})

	t.Run("CacheHitRequest", func(t *testing.T) {
		coord := "cached-pkg"
		payload := json.RawMessage(`{"coordinates": "cached-pkg", "vulnerabilities": []}`)
		now := time.Now()
		cache.Write(&CacheEntry{
			Coordinate:     coord,
			RetrievedAt:    now,
			LastAccessedAt: &now,
			Payload:        payload,
		})

		reqBody, _ := json.Marshal(componentRequest{Coordinates: []string{coord}})
		req := httptest.NewRequest(http.MethodPost, "/api/v3/component-report", bytes.NewReader(reqBody))
		req.SetBasicAuth("user", "valid-token")
		w := httptest.NewRecorder()

		s.handleComponentReport(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp []json.RawMessage
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp) != 1 {
			t.Fatalf("expected 1 result, got %d", len(resp))
		}

		var expected, actual interface{}
		json.Unmarshal(payload, &expected)
		json.Unmarshal(resp[0], &actual)

		expectedStr, _ := json.Marshal(expected)
		actualStr, _ := json.Marshal(actual)

		if string(expectedStr) != string(actualStr) {
			t.Errorf("expected payload %s, got %s", string(expectedStr), string(actualStr))
		}
	})

	t.Run("UpstreamPersistence", func(t *testing.T) {
		coord := "new-upstream-pkg"
		reqBody, _ := json.Marshal(componentRequest{Coordinates: []string{coord}})
		req := httptest.NewRequest(http.MethodPost, "/api/v3/component-report", bytes.NewReader(reqBody))
		req.SetBasicAuth("user", "valid-token")
		w := httptest.NewRecorder()

		s.handleComponentReport(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		// Check if it's in cache now
		entry := cache.Read(coord)
		if entry == nil {
			t.Errorf("expected coordinate %s to be persisted in cache", coord)
		} else {
			if entry.Coordinate != coord {
				t.Errorf("expected cached coordinate %s, got %s", coord, entry.Coordinate)
			}
		}
	})
}
