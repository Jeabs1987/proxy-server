#!/usr/bin/env python3
"""VPN-farm dead-endpoint monitor -> Discord (Armada) alert.

Single endpoints flap constantly. A PIA reconnect drops one tunnel for a poll or
two and the round-robin already skips it, so alerting on one down-tick is noise:
the farm sits at 50/50 and dips to 49/50 several times an hour under normal
operation. What actually needs a human is a tunnel that STAYS down -- PIA
retiring a region leaves it dead permanently (see CLAUDE.md, "Dead regions"),
and the farm silently loses capacity until somebody notices.

So an endpoint must be down for FAIL_THRESHOLD consecutive checks before it
alerts. Losing many at once is a harder signal (bad image pull, docker daemon,
whole-farm outage) and alerts after POOL_THRESHOLD checks. Recovery posts a
green check. Debounced via per-endpoint streaks + an ``alerted`` set in the
state file, matching /opt/ks-service/ks_auth_monitor.py.
"""
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

STATUS_URL = "http://127.0.0.1:9001/status"
CHANNEL_ID = "1553474085378326660"        # Armada #jeabslist (fleet debug)
SECRETS = "/etc/reverse-proxy/secrets.env"
STATE = "/var/lib/vpn-health-monitor/state.json"
UNIT = "go-proxy-server"

FAIL_THRESHOLD = 6     # consecutive down-checks before a per-endpoint alert (~30 min @ 5m)
POOL_THRESHOLD = 2     # consecutive checks before a whole-farm alert (~10 min)
POOL_FLOOR_FRACTION = 0.8  # fewer active than this share of all endpoints = farm-level problem
POOL_KEY = "<farm-down>"


def load_token():
    try:
        with open(SECRETS) as f:
            for line in f:
                line = line.strip()
                if line.startswith("DISCORD_BOT_TOKEN="):
                    return line.split("=", 1)[1].strip().strip('"').strip("'")
    except OSError:
        pass
    return None


def unit_property(prop):
    try:
        return subprocess.run(
            ["systemctl", "show", UNIT, "-p", prop, "--value"],
            capture_output=True, text=True, timeout=10,
        ).stdout
    except (OSError, subprocess.SubprocessError):
        return ""


def load_api_key():
    """Read API_KEY from the proxy's own systemd unit so the secret lives in one place.

    The key lives in the unit's EnvironmentFile (unit-secrets), and
    `systemctl show -p Environment` does not list values from such files. Reading
    only Environment left this monitor without a key, so every check got 401 and
    it reported the farm down for weeks without alerting on dead tunnels.
    """
    if os.environ.get("API_KEY"):
        return os.environ["API_KEY"]
    lines = unit_property("Environment").split()
    for entry in unit_property("EnvironmentFiles").splitlines():
        path = entry.split(" (", 1)[0].strip().lstrip("-")
        try:
            with open(path) as f:
                lines += f.read().splitlines()
        except OSError:
            pass
    for line in lines:
        line = line.strip()
        if line.startswith("API_KEY="):
            return line.split("=", 1)[1].strip().strip('"').strip("'")
    return None


def get_status(api_key):
    """Return (code, parsed_body_or_text). code is None if unreachable."""
    req = urllib.request.Request(STATUS_URL, headers={"X-Api-Key": api_key or ""})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            code, body = r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        code, body = e.code, (e.read().decode() if e.fp else "")
    except Exception:
        return None, None
    try:
        return code, json.loads(body)
    except ValueError:
        return code, body


def pool_floor(total):
    """Active endpoints below this count are a farm-level problem (40 of 50, 80 of 100)."""
    return int(total * POOL_FLOOR_FRACTION)


def down_now(code, data):
    """(set of DOWN endpoint names, active_count, total_count)."""
    if code is None or code != 200 or not isinstance(data, dict):
        return {POOL_KEY}, 0, 0
    eps = data.get("endpoints") or []
    if not eps:
        return {POOL_KEY}, 0, 0
    down = {e.get("name", "?") for e in eps if not e.get("active")}
    active = len(eps) - len(down)
    if active < pool_floor(len(eps)):
        return {POOL_KEY}, active, len(eps)
    return down, active, len(eps)


def post_discord(token, content):
    req = urllib.request.Request(
        "https://discord.com/api/v10/channels/%s/messages" % CHANNEL_ID,
        data=json.dumps({"content": content}).encode(),
        method="POST",
        headers={
            "Authorization": "Bot %s" % token,
            "Content-Type": "application/json",
            "User-Agent": "vpn-health-monitor/1.0",
        },
    )
    with urllib.request.urlopen(req, timeout=10) as r:
        return r.status


def threshold_for(name):
    return POOL_THRESHOLD if name == POOL_KEY else FAIL_THRESHOLD


def send(token, msg):
    if not token:
        print("no token; would post: %s" % msg, file=sys.stderr)
        return
    try:
        post_discord(token, msg)
    except Exception as e:  # noqa: BLE001
        print("discord post failed: %s" % e, file=sys.stderr)


def main():
    os.makedirs(os.path.dirname(STATE), exist_ok=True)
    try:
        st = json.load(open(STATE))
        streaks = dict(st.get("streaks", {}))
        alerted = set(st.get("alerted", []))
    except (OSError, ValueError):
        streaks, alerted = {}, set()

    code, data = get_status(load_api_key())
    down, active, total = down_now(code, data)

    for name in list(streaks):
        if name not in down:
            streaks.pop(name)
    for name in down:
        streaks[name] = streaks.get(name, 0) + 1

    token = load_token()
    newly_bad = sorted(n for n in down if streaks[n] >= threshold_for(n) and n not in alerted)
    recovered = sorted(n for n in alerted if n not in down)

    if newly_bad:
        alerted |= set(newly_bad)
        if newly_bad == [POOL_KEY]:
            msg = (":rotating_light: **VPN farm down** — only %d/%d tunnels active "
                   "(floor %d, status=%s). Check `docker ps --filter name=vpn-` and "
                   "the go-proxy-server journal." % (active, total, pool_floor(total), code))
        else:
            msg = (":rotating_light: **VPN endpoints dead** — %s down for >=%d checks "
                   "(%d/%d active). PIA has likely retired the region: test candidates "
                   "and swap per proxy-server CLAUDE.md, "
                   "\"Dead regions\"." % (", ".join("`%s`" % n for n in newly_bad),
                                          FAIL_THRESHOLD, active, total))
        send(token, msg)

    if recovered:
        alerted -= set(recovered)
        pretty = "farm" if recovered == [POOL_KEY] else \
            ", ".join("`%s`" % n for n in recovered)
        send(token, ":white_check_mark: **VPN recovered** — %s back online (%d/%d active)."
             % (pretty, active, total))

    json.dump({"streaks": streaks, "alerted": sorted(alerted)}, open(STATE, "w"))
    print("code=%s active=%d/%d down=%s streaks=%s alerted=%s newly_bad=%s recovered=%s"
          % (code, active, total, sorted(down), streaks, sorted(alerted), newly_bad, recovered))


if __name__ == "__main__":
    main()
