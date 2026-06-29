# bestproxy e2e harness

Validates forward/CONNECT mode + per-request failover + health locally, no real upstreams.

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

# 2. POST with a body survives forced failover
docker compose -f e2e/docker-compose.e2e.yml stop fwd1
curl -s -XPOST --data 'hello' http://localhost:8888/openrouter/v1/x   # → e2e-origin-ok (via fwd2)

# 3. Health/circuit-breaker marks the dead proxy down
curl -s http://localhost:8888/dashboard/json | jq '.sets[0].proxies[] | {addr, status_text}'

docker compose -f e2e/docker-compose.e2e.yml down
```
