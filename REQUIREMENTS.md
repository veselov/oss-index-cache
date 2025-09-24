# OSS Index Caching Proxy — Requirements

## 1. Purpose
Provide a lightweight caching proxy for Sonatype OSS Index component reports that:
- Authenticates incoming requests using GitLab Personal Access Tokens (PATs).
- Caches verified PATs for a configurable duration to reduce GitLab verification load.
- Caches component report responses on disk for a configurable time-to-live (TTL).
- Proactively refreshes nearly-expired entries and evicts long-unused ones.
- Proxies upstream requests to Sonatype with configured credentials, bundling only missing/expired components.

## 2. Scope
- In-scope: HTTP service exposing an endpoint equivalent to OSS Index “component-report”, basic-auth GitLab PAT verification, persistent on-disk cache with background maintenance, configuration, logging, and minimal observability.
- Out-of-scope: UI, rate limiting, distributed cache clustering, multiprocess coordination beyond single-instance file locking.

## 3. Terminology
- Component Coordinate: A Package URL (purl) identifying a component, e.g., `pkg:maven/group/artifact@version`.
- Report: The JSON metadata for a component as returned by OSS Index.
- TTL: Time during which a cached report is considered valid.
- PAT: GitLab Personal Access Token.

## 4. External APIs
- Upstream Sonatype OSS Index:
  - Endpoint: `https://ossindex.sonatype.org/api/v3/component-report`
  - Method: POST
  - Body: `{ "coordinates": [ "<purl>", ... ] }`
  - Auth: Must send configured credentials (see Configuration).
- GitLab API for PAT verification:
  - Endpoint: `GET <gitlab_base_url>/api/v4/user`
  - Auth: `PRIVATE-TOKEN: <pat>` header or `Authorization: Bearer <pat>` (implementation may support both; default PRIVATE-TOKEN).
  - Expected 200 for valid PAT; 401/403 for invalid/expired.

## 5. Functional Requirements

### 5.1 Incoming Authentication
- The proxy must require HTTP Basic Authentication.
- Username can be any non-empty string; password must be a GitLab PAT.
- On first use (or after cache expiry), verify the PAT against the configured GitLab server.
- Cache successful PAT verifications for `AUTH_CACHE_TTL` before re-verifying.
- Reject requests with 401 if PAT invalid or verification fails.

### 5.2 Proxy Endpoint
- Expose `POST /api/v3/component-report` mirroring OSS Index.
- Accept JSON body: `{ "coordinates": [ "<purl>", ... ] }`.
- Validate input; empty or missing `coordinates` yields 400.

### 5.3 Request Handling and Caching
- Parse input coordinates.
- For each coordinate:
  - If a cached report exists and is not expired, include it in the response list.
  - Otherwise, mark as missing/expired.
- Bundle all missing/expired coordinates into a single upstream POST to Sonatype.
- Authenticate the upstream request using configured credentials.
- Merge upstream results with cached ones preserving the original order of requested coordinates.
- For each upstream result received:
  - Persist/update cache entry with:
    1) last_accessed timestamp (update when serving downstream)
    2) retrieved timestamp (time fetched from Sonatype)
    3) raw metadata payload from Sonatype
- Return combined array as response with HTTP 200.
- If upstream call partially fails:
  - Return 502 if no component results can be obtained and none are cached.
  - Otherwise return 200 with combined results for successful components and include error stubs for failed ones (see Error Handling). This mirrors a best-effort model.

### 5.4 Cache Expiration Logic
- A report is expired if: `retrieved_at < now - REPORT_TTL`.
- `REPORT_TTL` is configurable.
- On each request served from cache (expired or not), update the entry’s `last_accessed_at` to `now`.

### 5.5 Background Maintenance
- See 5.6 Persistence for index description
- Maintenance worker has two tasks
  - refresh nearly-expired reports
  - evict unused reports
- Based on maintained indices, check what is next operation for either of the two
- Maintenance thread will be woken up on index updates, so it can recalculate its next move
- When evicting or refreshing, handle all eligible elements, not just the top indexed one
- Use corresponding configuration values to determine when to act

### 5.6 Persistence
- Cache must be stored on disk at a configurable directory `CACHE_DIR`.
- One file per coordinate is acceptable. Recommended structure:
  - File name: a stable hash of the coordinate (e.g., SHA-256 hex) to avoid filesystem issues.
  - Content: JSON document with fields:
    - `coordinate` (string)
    - `retrieved_at` (RFC3339 string or Unix epoch ms)
    - `last_accessed_at` (RFC3339 string or Unix epoch ms)
    - `payload` (object) — full JSON returned by Sonatype for that coordinate
  - Optionally include metadata like `version` for forwards compatibility.
- Reads and writes must be atomic enough to prevent corruption (write to temp file then rename).
- One index file that indexes per component last_modified time is to be kept. This is to be used by the background maintenance to determine which
    components to refresh.
- One index file that indexes per component last_accessed time is to be kept. This is to be used by the background maintenance to determine which
    components to evict.

### 5.7 Concurrency and Ordering
- The response must preserve the order of requested `coordinates`.
- Cache reads/writes must be thread-safe within a single process.
- Background refresh/eviction must not race with request handling; use locks around per-key operations.

### 5.8 Upstream Authentication
- All requests to Sonatype must include configured credentials:
  - Either Basic Auth with configured username/password or an Authorization header with a token per OSS Index requirements.
  - Configuration keys provided below.

## 6. Non-Functional Requirements
- Language: Go (to align with existing repository), but this document is implementation-agnostic.
- Performance: Should handle at least hundreds of coordinates per request and batch upstream queries efficiently.
- Reliability: Graceful degradation when upstream is down — serve cached entries that are still valid; best-effort partial results.
- Security: Do not log PATs or Sonatype credentials. Support TLS termination via reverse proxy or in-app TLS (optional).
- Observability: Structured logs; basic metrics suggested (cache hits/misses, refreshes, evictions, upstream latency, auth cache hits).
- Configuration via environment variables and/or config file. Env vars take precedence.

## 7. Configuration
All durations accept Go duration format (e.g., `5m`, `24h`). Defaults are suggestions; implementation may choose reasonable defaults.

The only argument to the go process is a configuration file path, and is required.

Configuration must be a YAML file that follows the structure outlined in `sample_config.yaml`

## 8. Data Model
Cache Entry (JSON file):
```
{
  "version": 1,
  "coordinate": "pkg:maven/group/artifact@1.2.3",
  "retrieved_at": "2025-09-23T14:31:00Z",
  "last_accessed_at": "2025-09-23T14:31:05Z",
  "payload": { /* raw Sonatype response object for this coordinate */ }
}
```

Auth Cache Entry (in-memory):
- key: PAT value (hashed in-memory if desired)
- value: { verified_at, expires_at, gitlab_user_id (optional), scope info (optional) }

## 9. Request/Response Workflow
1. Client sends POST to `/api/v3/component-report` with Basic Auth (user: anything, pass: PAT).
2. Server extracts PAT and checks in auth cache; if miss or expired, calls GitLab `GET /api/v4/user`.
   - If 200, store success in auth cache until `AUTH_CACHE_TTL`.
   - Else 401 Unauthorized to client.
3. Parse `coordinates` from body; validate.
4. For each coordinate, attempt read from disk cache:
   - If not expired, enqueue for response and update `last_accessed_at`.
   - Else add to `missing` set.
5. If `missing` not empty, POST to OSS Index with configured auth.
6. Persist each returned component as a cache file with timestamps.
7. Combine results in original order and return 200.

## 10. Error Handling
- 400 Bad Request: invalid/missing JSON or no coordinates.
- 401 Unauthorized: missing/invalid Basic Auth or invalid PAT.
- 502 Bad Gateway: upstream failure when no cached results are available.
- Partial upstream failure:
  - Return 200 with per-coordinate error object for failed items:
    ```
    { "coordinate": "<purl>", "error": { "code": "UPSTREAM_ERROR", "message": "..." } }
    ```
  - Preserve positions to match input order.
- Timeouts and retries: single retry for transient 5xx may be implemented; overall request must respect `REQUEST_TIMEOUT`.

## 11. Security Considerations
- Never log PATs or upstream credentials. Mask tokens in errors.
- Support TLS via reverse proxy (recommended) or HTTPS listener (optional future enhancement).
- File permissions on `CACHE_DIR` should restrict access (0700 by default).
- Consider hashing PATs in auth cache to reduce exposure in memory.

## 12. Operational Notes
- Startup: ensure `CACHE_DIR` exists and is writable.
- Shutdown: background worker should stop gracefully.
- Backups: cache is rebuildable; backups not required but may help warm-start.

## 13. Acceptance Criteria
- A requirements document exists in the repo at project root describing the caching proxy behavior per issue.
- The document specifies:
  - Basic Auth with GitLab PAT verification and PAT cache TTL, configurable GitLab base URL.
  - Parsing coordinates and per-coordinate cache usage.
  - Disk-based persistent cache with directory configuration.
  - Each cache entry includes last accessed timestamp, retrieved timestamp, and payload.
  - Configurable report TTL, proactive refresh below 5% remaining TTL, and eviction after 5 days unused (all configurable).
  - Upstream OSS Index requests authenticated with configured credentials.
- Document includes configuration knobs, workflows, and error handling sufficient for implementation.

### 14. Logging

Log everything reasonably through systemd-journalctl

