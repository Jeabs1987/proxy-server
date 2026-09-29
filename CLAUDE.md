# proxy-server — Claude Code Guide

VPN-farm HTTP proxy. A Go service (`main.go`) round-robins outbound requests
across a farm of [gluetun](https://github.com/qdm12/gluetun) (PIA) Docker
containers so consumers get rotating egress IPs.

- **Service:** `go-proxy-server` (systemd), binds `os.Getenv("PORT")` → `9001`
- **Public:** `https://proxy.jeab.dev` · **Internal:** `http://localhost:9001`
- **API:** `GET /proxy?url=<encoded>&strategy=roundrobin|random|specific&api_key=…`,
  `GET /status` (auth). Full spec: [.github/PROXY_API_SPEC.md](.github/PROXY_API_SPEC.md)
- **Main consumer:** `ks-redeemer` (routes Kingshot game-server traffic through here).

## Deployment Constraints

- **Port:** always `os.Getenv("PORT")`. Never hardcode.
- **Binary:** build output named `main` in app root. Never commit `main`, `.env`.
- **Creds:** PIA login lives in `.env` on the VPS (`PIA_USERNAME`/`PIA_PASSWORD`),
  gitignored. `setup-vpn.sh` writes it. The 50 containers read it via `${…}`.
- **Deploy:** push to `main` → VPS auto-pulls + rebuilds + restarts the Go
  service. Deploy does **not** run `docker compose up` for this app, so compose
  changes reach running containers only through the Sunday staggered restart or
  a manual `docker compose up -d --no-deps <services>` over SSH.
  Run runtime/docker commands over SSH on the Main VPS (`root@213.199.48.131`);
  never hand-edit files under `/opt/reverse-proxy/apps/` on the VPS.

## The Endpoint Invariant (read before touching the farm)

The endpoint set is defined in **two places that MUST stay 1:1**:

1. `endpoints.json` lists **50** endpoints (`name`, `proxy_url`), each pointing at
   `http://127.0.0.1:88xx` (host ports **8881–8930**). `go:embed` builds it into
   the binary. `ENDPOINTS_FILE` overrides it at run time, and `${VAR}` in a
   `proxy_url` expands from the environment (for credentials of a remote farm).
2. `docker-compose.yml` defines **50** `vpn-*` services, each publishing
   `127.0.0.1:88xx:8888` on a unique static IP `172.22.0.x`. The shared settings
   sit in the `x-gluetun` / `x-pia-env` anchors.

An endpoint `name` must equal its service's `SERVER_REGIONS`, and its port must
equal the published port.

**Verify correspondence after any change:** `go test ./...`.
`TestEndpointsMatchCompose` fails on any name, port, or static-IP mismatch.

**Health and failover.** `StartHealthChecks` probes each tunnel every 60 s
(`https://api.ipify.org`, then `https://1.1.1.1/cdn-cgi/trace` if ipify fails,
so one provider outage cannot empty the pool) and flips `Active`. Routing skips
inactive tunnels. `/status` shows each tunnel's `exit_ip` and request counters,
and the `[health]` log line counts distinct exit IPs. When a request fails
**before the target has seen it** (dial, CONNECT, or TLS failure, or no
connection within 6 s), the proxy retries it on up to 2 other tunnels and
re-probes the failed one at once. It never retries after the target may have
received the request, and `strategy=specific` never switches tunnels. Endpoints
start optimistically `Active` and converge in 15–35 s after boot. Until then,
failover covers dead slots.

## Drift: why the farm shrank, and how it's prevented

**Do NOT trim the live set with a `profiles:` key.** From inception until
2026-06-05, 30 of the 50 services carried `profiles: ["disabled"]`, so a plain
`docker compose up -d` started only the 20 un-profiled ones. Combined with the
old weekly cron (below), the running set became a one-way ratchet that only
shrank — leaving `main.go` round-robining over 30 dead ports (mostly `502`).
The profiles were removed so **all 50 start by default**. If you ever need to
reduce capacity, drop the endpoints from `endpoints.json` too (keep them 1:1)
rather than disabling containers behind the running proxy.

**Bring up / reconcile the full farm (over SSH):**
```bash
cd /opt/reverse-proxy/apps/proxy-server && docker compose up -d   # → all 50
docker ps --filter name=vpn- --format '{{.Names}}' | wc -l        # → 50
```

**Weekly maintenance cron** (root crontab on the Main VPS) Sunday 3am: pulls a
fresh gluetun image (keeps the server list current — see below), then recreates
the farm in batches of 10 with 20s gaps between batches (keeps ~40 tunnels live
throughout maintenance, clears memory leaks, and reconciles any stopped
container). The script lives at `scripts/staggered-restart.sh` in this repo.

**To update the VPS crontab manually** (`crontab -e` as root on the VPS):
```cron
0 3 * * 0 cd /opt/reverse-proxy/apps/proxy-server && bash scripts/staggered-restart.sh >> /var/log/vpn-restart.log 2>&1
```

**Do NOT** use the old one-liner (`docker compose up -d --force-recreate` on all 50
at once) — it drives host CPU load to ~50 for ~60s (50 concurrent OpenVPN inits
on a 12-core host) and causes a complete proxy outage during that window.

## Dead regions = a STALE server list, not bad config

gluetun bakes a PIA `servers.json` into its image. If that snapshot ages, the
few server IPs it lists for a region get decommissioned by PIA and the tunnel
fails with `EHOSTUNREACH` / `TLS handshake failed` — the health-check then marks
it inactive. On 2026-06-05 the on-VPS image was ~5.5 months old and **only
20/50 tunnels connected**; pulling the current image (`image: qmcgaw/gluetun`,
unpinned = latest) jumped it to **43/50**. So the fix for "lots of regions dead"
is almost always `docker compose pull && docker compose up -d --force-recreate`,
which the weekly cron now does. The 7 PIA micro-regions that stayed dead even on
a fresh image (Wyoming, New Hampshire, Oklahoma, Rhode Island, the Carolinas/
Dakotas, Vermont — PIA genuinely removed them) were swapped for working NA/EU
regions (New York, Vancouver, Mexico, Netherlands, France, UK London, Berlin),
giving ~50/50. When picking replacements, test a candidate first
(`docker run --rm --cap-add=NET_ADMIN -e VPN_SERVICE_PROVIDER='private internet access' -e OPENVPN_USER=… -e OPENVPN_PASSWORD=… -e SERVER_REGIONS='<region>' -e HTTPPROXY=on -p 9999:8888 qmcgaw/gluetun`
then `curl -x http://127.0.0.1:9999 https://api.ipify.org`) and keep the
`endpoints.json` name 1:1 with the compose `SERVER_REGIONS`.

**A fresh image does NOT always help — PIA keeps retiring US micro-regions.**
On 2026-07-31 seven more regions died (Texas, Atlanta, Silicon Valley, Oregon,
Virginia, Pennsylvania, West Virginia → 43/50). The image was already current,
and both `docker restart` and `docker compose up -d --force-recreate` failed to
revive them, so the 2026-06-05 playbook did not apply. Candidate testing showed
the retirement is US-wide: of 12 unused US/CA regions only **US Salt Lake City**
and **CA Montreal** connected, while **all 12** EU candidates connected. The
seven were swapped for Salt Lake City, Montreal, Ireland, ES Madrid, IT Milano,
SE Stockholm, and Poland — same ports and static IPs, `main.go` names 1:1.

Two traps when diagnosing this:

- **`docker ps` health lies.** Five of the seven reported `healthy` while unable
  to pass traffic (the healthcheck runs every 5m with 3 retries). Trust the
  proxy's own `[health] N/50` log line, or curl through the port.
- **ICMP is not a liveness test.** `US Chicago` fails to answer ping on all 8 of
  its listed IPs yet its tunnel works. Only an actual gluetun container plus a
  `curl -x` through it proves a region is usable.
- **A one-day-old image can still be stale for one region.** On 2026-09-27,
  `US Las Vegas` (8889) and `DE Berlin` (8929) failed `TLS handshake failed` on
  every listed server (8 each, ~640 failures in 6 h) on an image pulled the night
  before. PIA's live list (`https://serverlist.piaservers.net/vpninfo/servers/v6`)
  had moved both regions to IPs that gluetun did not list. Compare that list with
  the `link remote:` IPs in `docker logs` before you blame the region. gluetun's
  own updater cannot fix this, because it fetches through the tunnel. Both were
  swapped for **US Silicon Valley** and **DE Frankfurt** (same ports, static IPs;
  `main.go` names 1:1). Silicon Valley was one of the regions retired on
  2026-07-31; it connected again on 2026-09-27.

**Resource note:** tunnels use about 30 MiB each (cap 256 MiB). On 2026-09-29
the 50 used 1.5 GiB in total on the 47 GiB host, and a median of 1.7% CPU each. CPU caps are `cpus: 0.50` per container (raised from `0.10` to
stop CFS throttling — see infra `VPS_PERFORMANCE_INVESTIGATION.md`).
