# VPN Farm Service Specification

This document describes the interface for services that use the VPN Farm proxy. The service sends each request out through one of a pool of VPN tunnels, so callers get rotating egress IPs.

## Base URL

**Public endpoint:** `https://proxy.jeab.dev`

Nginx on the VPS terminates port 443 and forwards to the service on port 9001.
**Internal (same VPS):** `http://127.0.0.1:9001`. Callers on the VPS should use this address. The public URL goes through Cloudflare and back.

**Forward-proxy mode:** `proxy.jeab.dev:9443` (TLS) speaks the ordinary HTTP proxy protocol. See [Forward proxy](#5-forward-proxy).

## Authentication

Every endpoint except `/` needs the API key, either in the `X-Api-Key` header or in the `api_key` query parameter. A missing or wrong key returns `401 Unauthorized`.

## Core API

### 1. Proxy Request

Sends an HTTP request through a VPN tunnel.

**Endpoint:** `GET|POST|… /proxy`

**Query parameters:**

| Parameter  | Type   | Required | Description |
|------------|--------|----------|-------------|
| `url`      | string | **Yes**  | The full target URL, percent-encoded (e.g. `https%3A%2F%2Fapi.ipify.org`). |
| `strategy` | string | No       | `roundrobin` (default), `random`, or `specific`. |
| `vpn`      | string | No       | Required when `strategy=specific`. An endpoint name from `/status`. |

**Request headers:** the proxy forwards the caller's headers to the target, except its own credentials (`X-Api-Key`, `Authorization`, `Proxy-Authorization`), `Cookie`, and every header that carries the caller's IP (`X-Forwarded-*`, `X-Real-IP`, `Forwarded`, `CF-*`, `True-Client-IP`).

**Request body:** up to 8 MiB (Nginx limits the public path to 1 MiB). A larger body returns `413`.

**Response:** the target's status, headers, and body, plus:

| Header | Description |
|--------|-------------|
| `X-VPN-Used` | The name of the tunnel that carried the request. |

**Failover:** with `roundrobin` or `random`, a tunnel that fails before the target receives the request is replaced by another tunnel, up to 3 tunnels per request. The proxy does not retry after the target may have seen the request, so a `POST` never runs twice. `specific` never switches tunnels.

**Errors from the proxy itself:**

| Status | Meaning |
|--------|---------|
| `400` | Missing or invalid `url`, or an unknown `vpn` name. |
| `403` | The target resolves to a private or internal address. |
| `413` | The request body is over 8 MiB. |
| `502` | Every tunnel tried failed, or the target dropped the connection. |
| `503` | No tunnel is active. |

**Example:**

```http
GET /proxy?url=https%3A%2F%2Fapi.ipify.org&strategy=specific&vpn=US%20Florida HTTP/1.1
Host: proxy.jeab.dev
X-Api-Key: …
```

```http
HTTP/1.1 200 OK
X-VPN-Used: US Florida

102.129.152.96
```

### 2. System Status

Returns the health and traffic counters of every tunnel. The counters start at zero when the service starts.

**Endpoint:** `GET /status`

```json
{
  "endpoints": [
    {
      "name": "US Florida",
      "active": true,
      "exit_ip": "102.129.152.96",
      "checked_at": "2026-09-29T14:19:35Z",
      "requests": 1204,
      "ok": 1180,
      "rate_limited": 12,
      "forbidden": 0,
      "upstream_5xx": 3,
      "errors": 9
    }
  ]
}
```

| Field | Description |
|-------|-------------|
| `active` | The last health probe (every 60 s, or right after a failed request) reached the internet through this tunnel. Routing skips inactive tunnels. |
| `exit_ip` | The public IP the last successful probe saw. |
| `requests` | Attempts routed through this tunnel. A retried request counts once on each tunnel it tried. |
| `ok` | Responses with a status below 400. |
| `rate_limited` / `forbidden` / `upstream_5xx` | Target responses with 429, 403, or 5xx. A rising count on one tunnel usually means the target limits or blocks that exit IP. |
| `errors` | Attempts with no response at all. |

## Management API

### 3. Fetch Logs

**Endpoint:** `GET /api/logs/server` — a JSON array of the latest 1000 log lines.

### 4. Restart Service

**Endpoint:** `POST /api/server/restart` — the process exits and systemd starts it again.

### 5. Forward proxy

`proxy.jeab.dev:9443` accepts `CONNECT` (for `https://` targets) and absolute-URL requests (for `http://` targets). Authenticate with `Proxy-Authorization: Basic <selector>:<API_KEY>`. The username selects the tunnel: empty or `roundrobin`, `random`, or an endpoint name from `/status`.

```bash
curl -x https://proxy.jeab.dev:9443 -U ":$API_KEY" https://api.ipify.org
```

Failover works as in `/proxy`: a `CONNECT` that fails moves to another tunnel unless the username names one.

## Usage Recommendations

1. **Retries:** the proxy already retries tunnel failures that happen before the target sees the request. Retry at the caller only for target-side failures, for example a `429` or a Cloudflare block page.
2. **Timeouts:** one tunnel attempt waits at most 30 s. A failed tunnel that another can replace is abandoned after 6 s. A caller timeout of 15–20 s leaves room for one failover.
3. **Encoding:** percent-encode the `url` parameter, especially when it has its own query string.
