#!/usr/bin/env bash
# scripts/staggered-restart.sh
#
# Weekly maintenance restart for the VPN farm.
# Recreates every vpn-* container in batches of 10, with a 20-second pause
# between batches.  Only 10 tunnels are down at any time during maintenance
# (the proxy fails requests over to the others) instead of a complete outage.
#
# Usage (from the repo root):
#   bash scripts/staggered-restart.sh
#
# The cron entry in root's crontab on the VPS should be:
#   0 3 * * 0 cd /opt/reverse-proxy/apps/proxy-server && bash scripts/staggered-restart.sh >> /var/log/vpn-restart.log 2>&1
#
# chmod +x scripts/staggered-restart.sh

set -euo pipefail

BATCH_SIZE=10
SLEEP_BETWEEN_BATCHES=20
COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

log "=== staggered-restart.sh starting ==="
log "Batch size: ${BATCH_SIZE}  Sleep between batches: ${SLEEP_BETWEEN_BATCHES}s"

# Pull the updated image first (sequential — does not cause a thundering herd).
log "Pulling latest gluetun image..."
docker compose -f "${COMPOSE_FILE}" pull
log "Image pull complete."

# Collect all vpn-* service names in a stable order.
mapfile -t ALL_SERVICES < <(
  docker compose -f "${COMPOSE_FILE}" config --services \
    | grep '^vpn-' \
    | sort
)

TOTAL=${#ALL_SERVICES[@]}
log "Found ${TOTAL} vpn-* services to recreate."

batch_num=0
i=0
while [ "${i}" -lt "${TOTAL}" ]; do
  batch_num=$(( batch_num + 1 ))
  # Slice the next BATCH_SIZE services.
  batch=( "${ALL_SERVICES[@]:${i}:${BATCH_SIZE}}" )
  i=$(( i + BATCH_SIZE ))

  log "--- Batch ${batch_num}: recreating ${#batch[@]} containers: ${batch[*]}"
  docker compose -f "${COMPOSE_FILE}" up -d --force-recreate --no-deps "${batch[@]}"
  log "--- Batch ${batch_num} done."

  # Sleep between batches, but skip the pause after the last batch.
  if [ "${i}" -lt "${TOTAL}" ]; then
    log "Sleeping ${SLEEP_BETWEEN_BATCHES}s before next batch..."
    sleep "${SLEEP_BETWEEN_BATCHES}"
  fi
done

log "=== All ${TOTAL} containers recreated in ${batch_num} batches. ==="

# Remove any orphaned containers left over from old compose definitions.
log "Removing orphaned containers..."
docker compose -f "${COMPOSE_FILE}" up -d --remove-orphans
log "=== staggered-restart.sh complete ==="
