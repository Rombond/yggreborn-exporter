# YggReborn Exporter

A Go-based Prometheus exporter that scrapes your upload/download totals from [YggReborn](https://www.yggreborn.org) and exposes them via the Prometheus HTTP interface.

There is no JSON API: the exporter logs in, fetches the `/account` page and parses the hero-card values. YggReborn sits behind Cloudflare, so a [FlareSolverr](https://github.com/FlareSolverr/FlareSolverr) sidecar provides the `cf_clearance` cookie.

## Architecture

```
┌──────────────────┐     ┌──────────────┐     ┌─────────────┐
│ yggreborn.org    │────▶│ Exporter     │◀────│ Prometheus  │
│ (/account HTML)  │     │ (scrapes &   │     │ (collects)  │
└──────────────────┘     │ exposes /    │     └─────────────┘
         ▲               │ metrics      │
         │               └──────┬───────┘
         │  cf_clearance        │
┌────────┴─────────┐            │
│ FlareSolverr     │◀───────────┘
└──────────────────┘
```

## Requirements

- Go 1.22+
- Docker (recommended, for the FlareSolverr sidecar)

## Configuration

Create a `.env` file in the project root (see `.env.example`):

| Variable | Description | Default |
|----------|-------------|---------|
| `YGGREBORN_EMAIL` | Account email (or username) | *required* |
| `YGGREBORN_PASSWORD` | Account password | *required* |
| `YGGREBORN_API_BASE_URL` | Site host | `https://www.yggreborn.org` |
| `FLARESOLVER_URL` | FlareSolverr API URL | `http://localhost:8191` |
| `PORT` | Server listen port | `9090` |
| `METRICS_PATH` | Metrics endpoint path | `/metrics` |
| `SCRAPE_INTERVAL` | Refresh rate | `5m` |

## Quick Start

```bash
# Docker (exporter + FlareSolverr)
docker compose -f docker-compose.dev.yml up --build

# Local (FlareSolverr must be reachable on FLARESOLVER_URL)
export YGGREBORN_EMAIL=<email> YGGREBORN_PASSWORD=<pass>
go build -o yggreborn_exporter . && ./yggreborn_exporter
```

## Endpoints

- `GET /metrics` - Prometheus metrics
- `GET /health` - Health check (`{"status":"healthy","authenticated":true}`)

```text
# TYPE yggreborn_total_uploaded_bytes gauge
yggreborn_total_uploaded_bytes 2.8282e+11
# TYPE yggreborn_total_downloaded_bytes gauge
yggreborn_total_downloaded_bytes 6.33e+09
```

Sizes are parsed from the page (`282.82 Go`, `6.33 Mo`...) with decimal units (1 Go = 10^9 bytes), so values are rounded to 2 decimals.

## Prometheus Configuration

```yaml
scrape_configs:
  - job_name: 'yggreborn_exporter'
    static_configs:
      - targets: ['localhost:9090']
    metrics_path: /metrics
```

## Authentication Flow

1. Wait for FlareSolverr, then ask it for `/login` to get a `cf_clearance` cookie and its User-Agent
2. `POST /login` with JSON `{"Identifier": "...", "password": "..."}`, the `cf_clearance` cookie and the same User-Agent
3. Keep the session cookies from the response
4. `GET /account` and parse upload/download from the hero-card

The User-Agent must match the one FlareSolverr used, or Cloudflare rejects the cookie.
