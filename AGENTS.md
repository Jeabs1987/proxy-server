# proxy-server — Agent Guide

This is the project's only agent-instruction file. Do not add a `CLAUDE.md`: Claude Code reads `AGENTS.md` only when no `CLAUDE.md` exists in the working directory or any parent.

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
  gitignored. `setup-vpn.sh` writes it. The containers read it via `${…}`.
- **Deploy:** push to `main` → VPS auto-pulls + rebuilds + restarts the Go
  service. Deploy does **not** run `docker compose up` for this app, so compose
  changes reach running containers only through the Sunday staggered restart or
  a manual `docker compose up -d --no-deps <services>` over SSH.
  Run runtime/docker commands over SSH on the Main VPS (`root@213.199.48.131`).
  Secondary (`root@169.58.2.110`, key-only) holds a **standby** farm. Its
  controller starts it only while this farm is unreachable, and stops it after
  this farm has been healthy for a while. Never start it by hand next to a live
  farm: both use one PIA account. A drill uses `proxy-standby start --only <services>`.
  Spec: [PROXY_STANDBY.md](https://github.com/ArmadaInteractiveCo/Reverse-Proxy/blob/main/.github/PROXY_STANDBY.md).
  Never hand-edit files under `/opt/reverse-proxy/apps/` on the VPS.

## The Endpoint Invariant (read before touching the farm)

The endpoint set is defined in **two places that MUST stay 1:1**:

1. `endpoints.json` lists **100** endpoints (`name`, `proxy_url`), each pointing at
   `http://127.0.0.1:88xx`/`89xx` (host ports **8881–8980**). `go:embed` builds it into
   the binary. `ENDPOINTS_FILE` overrides it at run time, and `${VAR}` in a
   `proxy_url` expands from the environment (for credentials of a remote farm).
2. `docker-compose.yml` defines **100** `vpn-*` services, each publishing
   `127.0.0.1:<port>:8888` on a unique static IP `172.22.0.x` (`.10`–`.109`). The shared settings
   sit in the `x-gluetun` / `x-pia-env` anchors.

An endpoint `name` must equal its service's `SERVER_REGIONS`, and its port must
equal the published port.

**Verify correspondence after any change:** `go test ./...`. The standby on
Secondary receives `docker-compose.yml`, `endpoints.json`, `main` and `.env` from
this checkout within minutes of a deploy. It refuses a release in which the two
files disagree, and keeps its last good copy.
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
The profiles were removed so **all of them start by default**. If you ever need to
reduce capacity, drop the endpoints from `endpoints.json` too (keep them 1:1)
rather than disabling containers behind the running proxy.

**Bring up / reconcile the full farm (over SSH):**
```bash
cd /opt/reverse-proxy/apps/proxy-server && docker compose up -d   # → all 100
docker ps --filter name=vpn- --format '{{.Names}}' | wc -l        # → 100
```

**Weekly maintenance cron** (root crontab on the Main VPS) Sunday 3am: pulls a
fresh gluetun image (keeps the server list current — see below), then recreates
the farm in batches of 10 with 20s gaps between batches (keeps ~90 tunnels live
throughout maintenance, clears memory leaks, and reconciles any stopped
container). The script lives at `scripts/staggered-restart.sh` in this repo.

**To update the VPS crontab manually** (`crontab -e` as root on the VPS):
```cron
0 3 * * 0 cd /opt/reverse-proxy/apps/proxy-server && bash scripts/staggered-restart.sh >> /var/log/vpn-restart.log 2>&1
```

**Do NOT** use the old one-liner (`docker compose up -d --force-recreate` on the
whole farm at once) — with 50 tunnels it drove host CPU load to ~50 for ~60s
(50 concurrent OpenVPN inits on a 12-core host) and caused a complete proxy
outage during that window. With 100 it would be worse.

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
  proxy's own `[health] N/100` log line, or curl through the port.
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

**2026-09-29: expanded 50 → 100, and New Mexico swapped.** `US New Mexico`
(8922) failed `TLS handshake failed` for over 24 h: gluetun dialed only
`84.239.33.x`, while PIA's live list had moved the region to `147.90.190.x`
(the Las Vegas/Berlin pattern). It was swapped for **Austria** (same port and
static IP). 70 unused regions were tested as throwaway containers (10 at a
time on 127.0.0.1:19800–19809, `curl -x` through each). 60 finished before the
selection: 53 worked, all with exit IPs outside the farm's /24s. Dead: DE
Berlin, US Las Vegas, Serbia, Kazakhstan. Not in gluetun's list: UK Tottenham,
US South Carolina, US Tennessee. 50 working ones became ports 8931–8980
(`.60`–`.109`). The picks, in order, were Europe first (closest to the VPS),
then North America, then the rest. Streaming Optimized variants were skipped
because they share servers with their base regions.

**Resource note:** tunnels use about 30 MiB each (cap 256 MiB). On 2026-09-29
the 50 used 1.5 GiB in total on the 47 GiB host, and a median of 1.7% CPU each,
so 100 need about 3 GiB and one extra core. The host is CPU-bound by other
services (load 40–75 on 12 cores, mostly the redeemers), and `docker run` takes
~20 s per container under that load. Start new tunnels in small batches.
CPU caps are `cpus: 0.50` per container (raised from `0.10` to
stop CFS throttling — see infra `VPS_PERFORMANCE_INVESTIGATION.md`).

## Model Orchestration

Read your own model name. It decides whether you orchestrate or do the work.

| Running model | Role | Delegates to |
|---|---|---|
| Claude Opus | Orchestrator | Claude Sonnet |
| astra | Orchestrator | sol, terra or luna, by task difficulty |
| sol | Orchestrator | sol, terra or luna, by task difficulty |
| Claude Sonnet, terra, luna, any other model | Worker | No one (see the last rule) |

An orchestrator holds the plan and makes the hard calls: architecture, correctness,
ambiguity and the final review. It sends the mechanical, well-specified work to a
lower tier through the harness's subagent mechanism (in Claude Code, the `Agent`
tool or `Workflow`). Spend your own tokens on judgement, not on grunt work.

- **Keep (do it yourself):** decomposition, sequencing, disambiguation, correctness
  and accuracy judgement, conflicting results, and the final go/no-go on subagent output.
- **Delegate to the tier in the table:** well-specified edits, boilerplate, bulk
  refactors, search sweeps, test runs, formatting, and any task with a verifiable result.
  Pick the lowest tier that can do the task reliably. If a task is ambiguous, or writing
  the brief costs more than doing the work, do it yourself.
- **Verify, don't trust:** a subagent's result is an input to your judgement, not a
  final answer. You stay accountable for accuracy.
- **Workers:** do the work in your own loop. Use subagents only to fan out independent,
  parallel work, never to hand off judgement.
