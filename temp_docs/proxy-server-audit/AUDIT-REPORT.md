# Audit — proxy-server (VPN farm)

**Scope:** main.go, forwardproxy.go, docker-compose.yml, scripts/, deploy config at origin/main 059a374, the live VPS on 2026-09-29, and the ks-redeemer client (redeemer/api.go).

**Live baseline (2026-09-29):** 49/50 active. 48 tunnels answered, with 48 distinct exit IPs in 48 distinct /24s. The proxy uses 3.5% CPU and 23 MB RSS. The 50 tunnels use about 1.6 cores and 1.5 GiB in total. Traffic is about 3.5k /proxy requests per hour: 94% 200, 3.7% 499, 1.4% 502, 0.6% 429. Between 02:00 and 05:00, 429s rise to 6-9%. Host load average is 40-75 on 12 cores (CPU PSI about 60%). Three redeemer instances and a backup job cause most of it, not the farm.

## Critical

None.

## Important

**Health check depends on one third-party host** — [main.go:171](../../main.go#L171) — if api.ipify.org fails, every endpoint goes inactive and all /proxy traffic gets 503.

**No failover when a tunnel fails mid-cycle** — [main.go:509](../../main.go#L509) — a dial or proxy-connect error returns 502 at once. Nothing retries on another tunnel or marks the dead one inactive (about 1.2% of requests).

**Upstream request ignores caller cancellation** — [main.go:492](../../main.go#L492) — no r.Context(), so the 499s keep their tunnel busy for up to 30 s. The same bug is at [forwardproxy.go:196](../../forwardproxy.go#L196).

**Endpoint transport has no connect-phase timeouts** — [main.go:150](../../main.go#L150) — no dial or TLS-handshake limit, and a 30 s client timeout against the redeemer's 15 s. The caller gives up first.

**Endpoint set is hardcoded in two files** — [main.go:217](../../main.go#L217) — the 1:1 invariant with docker-compose.yml blocks a clean second farm. Load endpoints from a file.

**No per-endpoint request stats or exit IP** — [main.go:533](../../main.go#L533) — you cannot see which IPs draw 429s or Cloudflare blocks.

**Co-located consumer calls the farm through Cloudflare** — [ks-redeemer/redeemer/api.go:105](../../../ks-redeemer/redeemer/api.go#L105) — each request makes a round trip through Cloudflare and nginx. Use http://127.0.0.1:9001 on the same host.

**US New Mexico tunnel dead for over 24 hours** — [docker-compose.yml:1194](../../docker-compose.yml#L1194) — TLS handshake failed on every attempt. This matches the Las Vegas and Berlin pattern.

## Nice-to-have

**9 MB build binary is committed** — [.gitignore:5](../../.gitignore#L5) — vpn-farm is tracked in 4 versions. Ignore it, the compose backups, and __pycache__.

**Tunnel ports publish on 0.0.0.0** — [docker-compose.yml:21](../../docker-compose.yml#L21) — only the DOCKER-USER rule protects them. Bind to 127.0.0.1.

**Health-check jitter holds the semaphore** — [main.go:390](../../main.go#L390) — a pass takes 40-60 s, but the comment says 10 s.

**Cookie header is stripped from target requests** — [main.go:71](../../main.go#L71) — the redeemer's cookie jar never reaches the game gateway. Confirm that the gateway does not need it.

**API key comparison is not constant-time** — [main.go:106](../../main.go#L106) — the forward proxy already uses subtle.ConstantTimeCompare.

**Root page cannot load its data** — [main.go:557](../../main.go#L557) — it fetches /status without a key and shows port 8080.

**Stale scaffolding** — [main.go:617](../../main.go#L617) — rand.Seed has no effect. The Dockerfile uses Go 1.21. PROXY_API_SPEC.md and the CLAUDE.md memory figure are out of date.

**CONNECT tunnels have no idle limit** — [forwardproxy.go:169](../../forwardproxy.go#L169) — an abandoned tunnel holds its goroutines and sockets.

## By subsystem

- **Routing and failover (main.go):** context, transport timeouts, retry with passive marking, health-check targets, jitter. These changes belong together.
- **Config and scaling:** endpoints file, compose anchors, 127.0.0.1 binds. Do these before you add a second farm.
- **Observability:** per-endpoint counters and exit IP in /status.
- **Consumer (ks-redeemer):** PROXY_BASE_URL env var.
- **Hygiene:** binary, .gitignore, Dockerfile, docs, root page, rand.Seed.
