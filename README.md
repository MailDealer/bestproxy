# bestproxy

High-performance reverse proxy for geo-blocking bypass. Routes incoming requests through a pool of upstream servers, automatically selecting the one with the lowest latency and no availability issues.

## How it works

You define named proxy sets in the config. Each set gets an HTTP endpoint. Incoming requests to `/{set-name}/...` are forwarded to the best available upstream in that set, with the `/{set-name}` prefix stripped.

```
Client → POST /openrouter/v1/chat/completions → bestproxy
                                                     ↓ picks lowest-latency healthy upstream
                                               → POST /v1/chat/completions → openrouter-fi-01.msndr.net:443
```

**Upstream selection** uses Power-of-Two-Choices (P2C) with EWMA latency: two random healthy upstreams are picked, the one with lower EWMA wins. This avoids thundering herd while still routing to faster upstreams.

**Health monitoring** runs a TCP ping per upstream on a configurable interval. Consecutive failures trip a circuit breaker (marks upstream as down); consecutive successes restore it.

**Backup upstreams** — each set can declare a `backup:` list of reserve proxies. They are health-checked like everything else but receive traffic *only when every primary proxy in the set is down*. As soon as any primary recovers, traffic returns to the primaries. Backups are **not** pre-warmed at startup, so the first request after a failover pays a TLS handshake.

**Connection pool** pre-warms TLS connections at startup and keeps idle connections alive (`MaxIdleConnsPerHost`). All data is in memory — no database or Redis required.

## Quick start

```bash
# 1. Edit config
cp config.yaml my-config.yaml
# edit my-config.yaml with your upstreams

# 2. Run
docker compose up
```

The proxy listens on `:8888` by default. Dashboard is at [http://localhost:8888/dashboard](http://localhost:8888/dashboard).

## Configuration

```yaml
server:
  addr: ":8888"
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 90s

health:
  interval: 10s          # how often to TCP-ping each upstream
  timeout: 5s            # ping timeout
  failure_threshold: 3   # consecutive failures before marking upstream down
  recovery_threshold: 2  # consecutive successes before restoring upstream

sets:
  - name: openrouter
    pool:
      min: 20    # TLS connections to pre-warm at startup per upstream
      max: 100   # max idle connections kept alive per upstream
    proxies:
      - host: openrouter-fi-01.msndr.net   # port defaults to 443
      - host: openrouter-fi-02.msndr.net
      - host: openrouter-de-03.msndr.net
        port: 8443                          # custom port
    backup:                                 # used only when all proxies above are down
      - host: openrouter-backup-01.msndr.net
      - host: openrouter-backup-02.msndr.net

  - name: anthropic
    pool:
      min: 10
      max: 50
    proxies:
      - host: anthropic-us-01.msndr.net
```

### Config fields

| Field | Default | Description |
|---|---|---|
| `server.addr` | `:8888` | Listen address |
| `server.read_timeout` | `30s` | HTTP read timeout |
| `server.write_timeout` | `30s` | HTTP write timeout |
| `server.idle_timeout` | `90s` | Keep-alive idle timeout |
| `health.interval` | `10s` | Health check interval per upstream |
| `health.timeout` | `5s` | Health check TCP dial timeout |
| `health.failure_threshold` | `3` | Failures to mark upstream down |
| `health.recovery_threshold` | `2` | Successes to restore upstream |
| `sets[].pool.min` | `5` | Pre-warmed connections at startup |
| `sets[].pool.max` | `100` | Max idle connections per upstream |
| `sets[].proxies[].port` | `443` | Upstream port if not specified |
| `sets[].backup` | — | Reserve proxies (same fields as `proxies`); used only when all primaries are down, not pre-warmed |

## Docker

Mount your config and run:

```bash
docker run -d \
  -p 8888:8888 \
  -v $(pwd)/config.yaml:/config.yaml:ro \
  bestproxy
```

Or with docker compose:

```yaml
services:
  bestproxy:
    image: bestproxy
    ports:
      - "8888:8888"
    volumes:
      - ./config.yaml:/config.yaml:ro
    restart: unless-stopped
```

## Endpoints

| Path | Description |
|---|---|
| `/{set-name}/...` | Proxy endpoint — forwards request to best upstream in the set |
| `/dashboard` | Web dashboard with live stats |
| `/dashboard/json` | Dashboard data as JSON (single request) |
| `/dashboard/events` | Server-Sent Events stream (updates every 2s) |

## Dashboard

The web dashboard at `/dashboard` shows live stats for every upstream, auto-updating every 2 seconds via SSE.

```
bestproxy                                        updated: 14:22:01
─────────────────────────────────────────────────────────────────
SET: OPENROUTER
Proxy                  Req    Errors  1 min  5 min  1 hour  Active  Idle  Total  Created  Status
openrouter-fi-01      1 420       2   32ms   35ms    40ms       3    17     20       20     ●
openrouter-fi-02        890       0   28ms   31ms    35ms       1    19     20       20     ●
openrouter-de-03        210       1  104ms  108ms   112ms       0     9     10       20     ●

SET: ANTHROPIC
anthropic-us-01         230       0   45ms   48ms    50ms       0     9     10       10     ●
```

**Latency columns** — rolling averages over the last 1 minute, 5 minutes, and 1 hour. Color-coded: green < 100ms, yellow < 300ms, red ≥ 300ms.

**Connection pool columns:**
- **Active** — requests currently in flight to this upstream
- **Idle** — connections sitting ready in the pool
- **Total** — live connections (active + idle)
- **Created** — total TLS handshakes since start

### JSON API

`GET /dashboard/json` returns the full snapshot as JSON:

```bash
curl http://localhost:8888/dashboard/json
```

```json
{
  "generated_at": "2026-05-12T07:45:59Z",
  "sets": [
    {
      "name": "openrouter",
      "proxies": [
        {
          "addr": "openrouter-fi-01.msndr.net:443",
          "total_requests": 1420,
          "error_count": 2,
          "avg_1m_str": "32ms",
          "avg_5m_str": "35ms",
          "avg_1h_str": "40ms",
          "status_text": "up",
          "pool_in_flight": 3,
          "pool_idle": 17,
          "pool_size": 20,
          "pool_created": 20
        }
      ]
    }
  ]
}
```

## Usage example

Configure your HTTP client to use bestproxy as a base URL:

```python
import httpx

client = httpx.Client(base_url="http://localhost:8888/openrouter")
response = client.post("/v1/chat/completions", json={...})
```

```bash
curl http://localhost:8888/openrouter/v1/models
curl http://localhost:8888/anthropic/v1/messages
```

## Performance

- **Zero-allocation hot path** — request stats via `sync/atomic`, no locks
- **P2C selection** — O(1), two atomic reads + one mutex for EWMA
- **Pre-warmed TLS pool** — no handshake overhead on first requests
- **Per-upstream transport** — dedicated `http.Transport` with connection pooling per upstream, supports HTTP/2 multiplexing
- **Rolling latency windows** — pre-allocated circular buffer (512 slots), no GC pressure
- **All data in memory** — no external dependencies

## Project layout

```
cmd/bestproxy/          entry point, wiring, graceful shutdown
internal/config/        YAML config loading and validation
internal/stats/         atomic counters, rolling window, pool stats
internal/proxy/         upstream proxy, P2C selector, pool handler
internal/health/        background TCP health checker per upstream
internal/dashboard/     web dashboard, SSE stream, JSON API
```
