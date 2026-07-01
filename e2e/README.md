# bestproxy e2e harness

Validates forward/CONNECT mode + health-based routing locally, no real upstreams.
(There is no per-request failover: the body is streamed, not buffered; a dead proxy is
routed around by the health checker, not retried mid-request.)

Topology: `bestproxy` → picks `fwd1`/`fwd2` (tinyproxy CONNECT, basic-auth) → tunnels TLS to
`origin` (Caddy self-signed). bestproxy uses `tls.insecure_skip_verify: true` and `scheme: http`
to the proxies (plaintext tinyproxy) — prod uses real certs + `scheme: https`.

## Run

```bash
docker compose -f e2e/docker-compose.e2e.yml up --build -d
```

## Checks

```bash
# 1. Happy path — tunneled end-to-end to origin
curl -s http://localhost:8888/openrouter/anything          # → e2e-origin-ok

# 2. Health/circuit-breaker marks a dead proxy down; selector routes around it.
#    Stop one proxy, wait for the failure threshold, then requests flow via the other.
docker compose -f e2e/docker-compose.e2e.yml stop fwd1
sleep 20
curl -s http://localhost:8888/dashboard/json | jq '.sets[0].proxies[] | {addr, status_text}'  # fwd1 → down
curl -s -XPOST --data 'hello' http://localhost:8888/openrouter/v1/x   # → e2e-origin-ok (via fwd2)

docker compose -f e2e/docker-compose.e2e.yml down
```
