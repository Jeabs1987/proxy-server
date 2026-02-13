# VPN Farm Service Specification

This document outlines the interface for external services interacting with the VPN Farm proxy service. The service acts as an HTTP forward proxy that routes requests through a pool of VPN tunnels.

## Base URL

**Public Endpoint:** `https://proxy.jeab.dev`

The service is hosted on the VPS and handles incoming requests via Nginx on port 443, forwarding to the internal service on port 9001.
Internal Hostname (VPS): `http://localhost:9001`

## Core API

### 1. Proxy Request

Forwards an HTTP request through a selected VPN tunnel.

**Endpoint:** `GET /proxy`

**Query Parameters:**

| Parameter  | Type   | Required | Description |
|------------|--------|----------|-------------|
| `url`      | string | **Yes**  | The full target URL to fetch (e.g., `https://api.ipify.org?format=json`). |
| `strategy` | string | No       | Routing strategy. Options: `roundrobin` (default), `random`, `specific`. |
| `vpn`      | string | No       | Required if `strategy=specific`. The name of the VPN endpoint to use. |

**Supported VPN Names:**
- `US Florida`
- `US California`
- `US New York`
- `US Texas`
- `US Chicago`

**Request Headers:**
All headers sent by the client are forwarded to the target URL.

**Response Headers:**

| Header | Description |
|--------|-------------|
| `X-VPN-Used` | The name of the VPN endpoint that processed the request. |
| `X-VPN-Proxy` | The internal IP/Port of the proxy used. |

**Example Request:**

```http
GET /proxy?url=https://api.ipify.org&strategy=specific&vpn=US%20Texas HTTP/1.1
Host: proxy.jeab.dev
Accept: application/json
```

**Example Response:**

```http
HTTP/1.1 200 OK
Content-Type: application/json
X-VPN-Used: US Texas
X-VPN-Proxy: http://172.22.0.13:8888

{"ip": "123.45.67.89"}
```

### 2. System Status

Returns the health and status of all VPN endpoints.

**Endpoint:** `GET /status`

**Response Format:** JSON

**Example Response:**

```json
{
  "endpoints": [
    {
      "name": "US Florida",
      "proxy": "http://172.22.0.10:8888",
      "active": true
    },
    {
      "name": "US California",
      "proxy": "http://172.22.0.11:8888",
      "active": true
    }
    // ...
  ]
}
```

## Management API

These endpoints are provided for infrastructure compliance and monitoring.

### 3. Fetch Logs

Retrieve the latest server logs.

**Endpoint:** `GET /api/logs/server`

**Response:** JSON array of log strings.

### 4. Restart Service

Trigger a graceful restart of the proxy service.

**Endpoint:** `POST /api/server/restart`

**Response:** 200 OK text message.

## Usage Recommendations

1. **Retries:** Clients should implement retry logic. If a specific VPN endpoint fails (returns 502/503), try a different `strategy` or `vpn`.
2. **Timeouts:** The proxy has a 30-second timeout. Clients should set their timeouts accordingly.
3. **Encoding:** Ensure the `url` parameter is properly URL-encoded (percent-encoded), especially if it contains query parameters itself.
