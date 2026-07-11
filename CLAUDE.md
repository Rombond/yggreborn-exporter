# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# Architecture Overview

## High-Level Summary
This is a **Go-based Prometheus exporter** that scrapes user metrics from the YggReborn API and exposes them via the Prometheus HTTP interface. The application uses:
- Gin for HTTP routing
- Prometheus client library for metrics exposition
- FlareSolverr (Docker sidecar) to bypass Cloudflare protection
- Direct HTML parsing of the logged-in `/account` page since there is no JSON API

## Key Components

### 1. FlareSolverr — Cloudflare Bypass (Docker sidecar)
- Runs as a separate container alongside the exporter
- Uses a real Chromium browser to get a valid `cf_clearance` cookie
- The exporter calls FlareSolverr once at startup to get the cookie + User-Agent
- Cookie is reused for all subsequent API calls

### 2. Authentication Layer (`main.go`)
- `YggRebornClient` manages `cf_clearance` cookie and HTTP sessions
- No CSRF token needed for this site — direct POST auth
- POST directly to `/login` with `Identifier` and `password`
- Uses `cf_clearance` cookie from FlareSolverr + matching User-Agent
- Stores session cookies from login response
- Uses mutex for thread-safe cookie access
- Auto-authenticates on startup if credentials are provided via environment variables

### 3. Metrics Collection (`main.go`)
- `FetchMetrics()` calls `/account` with session + `cf_clearance` cookies
- Parses the HTML response and extracts upload and download values from the hero-card
- Converts values to bytes (Go units: 1 Go = 10^9 bytes)
- Prometheus gauges registered to track these metrics over time

### 4. HTTP Server (`main.go`)
- Serves `/metrics` endpoint that exposes Prometheus gauges via promhttp.Handler()
- Health check at `/health` for monitoring tools
- Runs as Gin server on configurable port (default: 9090)

### 5. Configuration (via `.env`)
```
YGGREBORN_API_BASE_URL   # API host (e.g., https://www.yggreborn.org)
YGGREBORN_EMAIL          # API credentials (required for auth)
YGGREBORN_PASSWORD       # API credentials (required for auth)
PORT                     # Server listen port (default: 9090)
METRICS_PATH             # Metrics endpoint path (default: /metrics)
SCRAPE_INTERVAL          # How often to refresh metrics (e.g., 5m)
FLARESOLVER_URL          # FlareSolverr API URL (default: http://localhost:8191)
```

## Authentication Flow

### Required Steps:
1. **Wait for FlareSolverr** — poll `/v1` until it responds
2. **Get cf_clearance** — call FlareSolverr `request.get` for `/login` page
3. **Login** — POST to `/login` with:
   - `cf_clearance` cookie from step 2
   - Matching User-Agent from FlareSolverr
   - JSON body: `{"Identifier": "...", "password": "..."}`
4. **Store session cookies** from login response for subsequent API calls

### Why FlareSolverr?
YggReborn is protected by Cloudflare. The `cf_clearance` cookie is required to pass Cloudflare's security challenge. FlareSolverr uses a real Chromium browser to solve the challenge and returns the cookie, which is then used for all API requests.

### Important Details:
- The User-Agent from FlareSolverr must match the one used in API calls
- The `cf_clearance` cookie is used for ALL requests (login + account)
- Login response sets session cookies (captured from `Set-Cookie` headers)
- Plain `http.Client` works fine once the cookie is attached

## Data Flow
1. Startup: Load config → wait for FlareSolverr → get `cf_clearance` → login → start server
2. Background: periodically scrape `/account` → parse HTML → update Prometheus gauges
3. `/metrics` requests: expose current gauge values via promhttp.Handler()
4. External Prometheus server scrapes `/metrics` to collect data

## API Endpoints

### FlareSolverr
```
POST http://flaresolverr:8191/v1
Body: {"cmd": "request.get", "url": "https://www.yggreborn.org/login"}
```

### Login
```
POST https://<base>/login
Content-Type: application/json
Cookie: cf_clearance=<from flaresolverr>
Body: {"Identifier": "user@example.com", "password": "..."}
```

### Account Page
```
GET https://<base>/account
Cookie: cf_clearance=<token>; session=<jwt>
```

## Docker Services

| Service      | Image                              | Port | Purpose                  |
|--------------|------------------------------------|------|--------------------------|
| app          | Built from Dockerfile              | 9090 | Prometheus exporter      |
| flaresolverr | ghcr.io/flaresolverr/flaresolverr:latest | 8191 | Cloudflare bypass        |

## Prometheus Metrics

| Metric                            | Type  | Description                          |
|-----------------------------------|-------|--------------------------------------|
| `yggreborn_total_uploaded_bytes`  | Gauge | Total bytes uploaded by the user     |
| `yggreborn_total_downloaded_bytes`| Gauge | Total bytes downloaded by the user   |

# Commands

## Building and Running

```bash
# In development, rebuild after code changes
docker compose -f docker-compose.dev.yml up --build
```

## Common Tasks

### View metrics locally
```bash
curl http://localhost:9090/metrics
```

### Health check
```bash
curl http://localhost:9090/health
```

# Access logs
```bash
docker compose -f docker-compose.dev.yml logs -f app
docker compose -f docker-compose.dev.yml logs -f flaresolverr
```

# Stop
```bash
docker compose -f docker-compose.dev.yml down
```
