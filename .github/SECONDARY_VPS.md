# Main VPS fleet integration

Primary (`vps-primary`, `213.199.48.131`) and Secondary (`vps-secondary`,
`169.58.2.110`) are separate hosts connected privately at `10.20.0.1` and
`10.20.0.2`. Use the operator's existing SSH configuration; keep credentials out
of repositories and release artifacts. Runtime commands run on the VPS.

## Load balancing contract - 2026-10-01

The authoritative placement and operations contract is
[LOAD_BALANCING.md](https://github.com/ArmadaInteractiveCo/Reverse-Proxy/blob/main/.github/LOAD_BALANCING.md).
Selected applications serve from both hosts; enrollment is explicit in infra
`configs/lb/apps.json`. This does not make every application a replicated service.

- Primary builds and synchronizes enrolled workers before cache purging. Workers
  stage complete releases, require readiness and restore the previous release on
  failed activation. Unchanged synchronization must not restart the application.
- Nginx removes unready application backends independently of Cloudflare's host
  monitor. Any HTTP request can reach either healthy origin; affinity is only a
  performance preference. Bounded safe-GET retries never replay POST mutations.
  Fixed game-shard routes never substitute a different shard.
- Share authoritative databases, session/revocation state and uploads. Keep one
  owner or durable idempotency for scheduled writes, queues and external side
  effects. Local caches must be disposable. Use pooled connections, deadlines,
  bounded concurrency and batched private-network requests.
- PostgreSQL has one writer. MongoDB uses its primary. Redis authentication and
  coordination use one shared authority. Secondary recovery copies are monitored
  but are not independent application writers; promotion and failback require
  explicit fencing under the runbook. App replication alone cannot survive loss
  of its shared data authority without that recovery procedure.
- Nginx overwrites forwarded scheme/client-address headers. Applications trust
  only the configured immediate proxy/private peers. Authentication must remain
  valid across origins, and dependency failures must report unavailable.
- New named hosts start disabled. Enroll, prepare and verify them with infra
  `scripts/lb-fleet.py`, then activate their explicit app placements. Never run
  Primary's all-services `deploy.sh` on another host. Existing nodes and new nodes
  may briefly serve adjacent versions; incompatible writer changes need a full
  coordinated drain before activation.

The separate opt-in worker system in `configs/fleet/projects.json` and
`scripts/fleet-deploy.py` has its own prepare/activate/health/rollback hooks. It is
not automatic load-balancer enrollment. See
[SECONDARY_VPS.md](https://github.com/ArmadaInteractiveCo/Reverse-Proxy/blob/main/.github/SECONDARY_VPS.md)
for that distinction, private service forwarding and backup/recovery details.

## Application placement

The outbound VPN farm is not an interchangeable replicated HTTP application.
Its 100 tunnel endpoints, egress sessions and Compose deployment retain their
existing ownership. Ingress load balancing does not duplicate those containers
or authorize another set of game-account sessions.
