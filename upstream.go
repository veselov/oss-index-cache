package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

type upstreamRequest struct {
	Coordinates []string `json:"coordinates"`
}

type upstreamResponseItem struct {
	Coordinate string          `json:"coordinate"`
	Payload    json.RawMessage `json:"-"` // we'll keep the raw
}

// fetchFromUpstream posts missing coordinates to Sonatype and returns a map from coordinate to raw JSON object.
func fetchFromUpstream(ctx context.Context, client *http.Client, upstreamURL, username, apiKey string, coords []string, logger *log.Logger) (map[string]json.RawMessage, int, error) {
	if len(coords) == 0 {
		return map[string]json.RawMessage{}, http.StatusOK, nil
	}
	start := time.Now()
	uHost := upstreamURL
	if u, err := url.Parse(upstreamURL); err == nil {
		uHost = u.Host + u.Path
	}
	body, err := json.Marshal(upstreamRequest{Coordinates: coords})
	if err != nil {
		return nil, 0, err
	}
	logger.Printf("upstream request start url=%s coords=%d bodyBytes=%d", uHost, len(coords), len(body))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Do not log credentials; just set them.
	req.SetBasicAuth(username, apiKey)
	resp, err := client.Do(req)
	if err != nil {
		logger.Printf("upstream request error url=%s err=%v dur=%s", uHost, err, time.Since(start))
		return nil, 0, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Printf("upstream non-2xx url=%s status=%d dur=%s", uHost, resp.StatusCode, time.Since(start))
		return nil, resp.StatusCode, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	var arr []json.RawMessage
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&arr); err != nil {
		logger.Printf("upstream decode error url=%s err=%v dur=%s", uHost, err, time.Since(start))
		return nil, 0, err
	}
	result := make(map[string]json.RawMessage, len(arr))
	for _, item := range arr {
		// Each item should have a coordinate field; keep the whole item as payload
		var tmp struct {
			Coordinate string `json:"coordinates"`
		}
		if err := json.Unmarshal(item, &tmp); err != nil {
			continue
		}
		if tmp.Coordinate == "" {
			continue
		}
		result[tmp.Coordinate] = item
	}
	logger.Printf("upstream success url=%s status=%d coords=%d dur=%s", uHost, resp.StatusCode, len(result), time.Since(start))
	return result, resp.StatusCode, nil
}
