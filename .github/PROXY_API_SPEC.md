# VPN Farm Proxy: Integration Guide

This guide is for projects that send traffic through the VPN farm. The farm routes each request through one of 100 VPN tunnels, so the target sees a rotating set of exit IPs instead of the VPS address. The tunnels are gluetun containers on one Private Internet Access (PIA) account.

## Choose an interface

| Interface           | Use it when                                                                       | Address                                                                         |
| ------------------- | --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `/proxy` API        | The caller can rewrite a request into a query string. This is the default choice. | `http://127.0.0.1:9001` on the VPS, `https://proxy.jeab.dev` from anywhere else |
| Forward proxy       | The tool only supports `HTTPS_PROXY`, or the request is a raw `CONNECT` tunnel.   | `https://egress.jeab.dev:9443`                                                  |
| Direct tunnel ports | The caller runs on the VPS and must bind one account to one tunnel.               | `http://127.0.0.1:8881` to `8980`                                               |

The first two interfaces check the API key, retry failed tunnels, and skip dead tunnels. The direct ports do none of that.

## Authentication

Send the API key in the `X-Api-Key` header. The `api_key` query parameter also works, but the header keeps the key out of URLs. A missing or wrong key returns `401 Unauthorized` with the body `Unauthorized`. Treat that as a configuration error and do not retry it.

Keep the key in the environment of the calling service. Never commit it.

## The `/proxy` API

```http
GET /proxy?url=https%3A%2F%2Fapi.ipify.org&strategy=roundrobin HTTP/1.1
Host: proxy.jeab.dev
X-Api-Key: <key>
```

The proxy accepts any HTTP method and sends it to the target.

| Parameter  | Required        | Meaning                                                                              |
| ---------- | --------------- | ------------------------------------------------------------------------------------ |
| `url`      | Yes             | The target URL, percent-encoded. A URL with no scheme gets `https`.                  |
| `strategy` | No              | `roundrobin` (default), `random`, or `specific`. Any other value means `roundrobin`. |
| `vpn`      | With `specific` | A tunnel name from `/status`.                                                        |

### What the proxy forwards

- **Request headers:** all of them, except the ones below.
- **Removed request headers:** `X-Api-Key`, `Authorization`, `Proxy-Authorization`, and `Cookie`. Send target credentials in the URL or the body, or use the forward proxy.
- **Removed headers that name the caller:** `X-Forwarded-*`, `X-Real-IP`, `Forwarded`, `True-Client-IP`, `CDN-Loop`, and `CF-*`. The target never sees the caller's address.
- **Request body:** up to 8 MiB. The public path through Nginx accepts 1 MiB.
- **Response:** the target's status, headers (including `Set-Cookie`), and body.
- **Redirects:** the proxy does not follow them. A `301` or `302` comes back as it is, and its `Location` header names the target's own URL. Send the next request through the proxy again.
- **Time limit:** 30 seconds per attempt, counted through the end of the response body. A slower response is cut.

### Tell a farm error from a target error

Every response that came from the target carries an `X-VPN-Used` header with the tunnel name. A response without this header came from the farm, Nginx, or Cloudflare. Use this to decide whether to blame the target or the farm.

| Status | Without `X-VPN-Used`                                            | With `X-VPN-Used`                                         |
| ------ | --------------------------------------------------------------- | --------------------------------------------------------- |
| `400`  | Missing or invalid `url`, or an unknown `vpn` name              | The target sent it                                        |
| `401`  | Wrong or missing API key                                        | The target sent it                                        |
| `403`  | The target resolves to a private or internal address            | The target sent it. This is often a block of the exit IP. |
| `413`  | The request body is over 8 MiB                                  | The target sent it                                        |
| `429`  | Not produced by the farm                                        | The target rate-limits this exit IP                       |
| `502`  | Every tunnel tried failed, or the target dropped the connection | The target sent it                                        |
| `503`  | No tunnel is active                                             | The target sent it                                        |

## Exit IPs and routing

- **Round robin** keeps one counter for all callers. Your own consecutive requests land on different tunnels only if other callers do not interleave.
- **`random`** picks one active tunnel for each request.
- **`specific`** uses the tunnel you name, even when it is inactive. Check `/status` first.
- **IPs are shared and can change.** PIA gives a tunnel its exit IP when the tunnel connects. Other PIA customers share that IP, and a reconnect can change it. On 2026-09-29 the 100 tunnels showed 100 different IPs in 100 different /24 networks.
- **Weekly restart:** every Sunday at 03:00 (VPS local time) the tunnels restart in batches of 10, so exit IPs change.

### Keep several requests on one IP

A login flow or a session token often needs one exit IP for all its requests. Send the first request with `roundrobin`, read `X-VPN-Used` from the response, and send the rest with `strategy=specific&vpn=<that name>`. Expect the pin to break when the tunnel reconnects. Handle a `502` on a pinned request by starting a new session on a new tunnel.

## Failover, timeouts, and retries

The farm handles tunnel failures for you.

1. A tunnel that fails before the target receives the request is replaced by another tunnel, up to 3 tunnels for each request. Failures include a refused connection, a failed CONNECT, a TLS error, and no connection within 6 seconds.
2. The failed tunnel gets a health check right away, and the normal check runs every 60 seconds.
3. The farm never retries after the target may have received the request. A `POST` cannot run twice.
4. `strategy=specific` never switches tunnels.

What this means for the caller:

- **Do not add a second retry layer for tunnel errors.** The farm already tried up to 3 tunnels.
- **Set the caller timeout to 20 seconds or more.** One failover then finishes inside the timeout. A request that fails on its first two tunnels can take about 42 seconds.
- **Retry a `502` only when the request is safe to repeat.** The target may have acted on it.
- **Back off on `503`.** No tunnel is active. Wait 10 seconds or more.
- **Change tunnel on `429` or a block page.** A `429`, or a `403` with an HTML body, means the target limits that exit IP. Retry with `roundrobin` or `random` after a pause. Do not retry on the same tunnel.

## `GET /status`

Returns the health and traffic counters of every tunnel. The counters start at zero each time the service starts.

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

| Field                                       | Meaning                                                                                                                   |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `name`                                      | The tunnel name. Use it for `vpn=` and as the forward-proxy username. Do not hardcode names. Get them from this endpoint. |
| `active`                                    | The last health check reached the internet through this tunnel.                                                           |
| `exit_ip`                                   | The public IP the last successful check saw.                                                                              |
| `requests`                                  | Attempts routed through this tunnel. A retried request counts once on each tunnel it tried.                               |
| `ok`                                        | Responses with a status below 400.                                                                                        |
| `rate_limited`, `forbidden`, `upstream_5xx` | Target responses with 429, 403, and 5xx. A high count on one tunnel means the target limits that IP.                      |
| `errors`                                    | Attempts with no response at all.                                                                                         |

## Forward-proxy mode

Use this when a client supports `HTTPS_PROXY` but cannot rewrite URLs, or when you need a raw `CONNECT` tunnel to a non-HTTP service.

**Address:** `https://<selector>:<API_KEY>@egress.jeab.dev:9443`

Cloudflare does not carry `CONNECT` or port 9443. Do not use `proxy.jeab.dev:9443`. The name `egress.jeab.dev` has no public DNS record on purpose, because a record would publish the origin IP. Add it to the client's `/etc/hosts`:

```text
213.199.48.131  egress.jeab.dev
```

A client that cannot edit `/etc/hosts` can dial the IP and set the TLS name to `egress.jeab.dev` instead.

The username selects the tunnel.

| Username                                | Tunnel                              |
| --------------------------------------- | ----------------------------------- |
| empty or `roundrobin`                   | The next active tunnel              |
| `random`                                | A random active tunnel              |
| A tunnel name, for example `US%20Texas` | That tunnel. Percent-encode spaces. |

The password is the API key. The listener handles `CONNECT` (for `https://` targets) and absolute-URL requests (for `http://` targets). The `200 Connection Established` reply and every plain-HTTP response carry `X-VPN-Used`.

```bash
curl -x 'https://random:<key>@egress.jeab.dev:9443' https://api.ipify.org
```

| Status | Meaning                                                                                                     |
| ------ | ----------------------------------------------------------------------------------------------------------- |
| `407`  | Wrong or missing credentials                                                                                |
| `403`  | The target resolves to a private or internal address                                                        |
| `502`  | Every tunnel tried failed, the target failed, or the named tunnel does not exist (`VPN endpoint not found`) |
| `503`  | No tunnel is active                                                                                         |

Failover works as in `/proxy`. A `CONNECT` that fails moves to another tunnel unless the username names one. The proxy closes a tunnel after 5 minutes without data.

## Direct tunnel ports

Each tunnel also publishes its own gluetun HTTP proxy on `127.0.0.1`, ports `8881` to `8980`. Only code on the VPS can reach them. Use them only when one account must stay on one tunnel, as `ks-reverse-engineered` does.

- These ports have no API key, no failover, and no health filter. A dead tunnel returns errors. Check `/status` before you assign an account.
- A slot keeps its port for good, but the region behind it can change. PIA retires regions, and the farm then swaps a new region into the same port. Port `8922` served US New Mexico until 2026-09-29 and now serves Austria.
- The exit IP behind a slot is not fixed. A reconnect can change it. Design each pin as "one tunnel", not "one IP". A truly fixed IP needs a different product, such as a PIA dedicated IP.
- The pool grew from 50 to 100 slots on 2026-09-29. A caller that lists ports `8881-8930` sees only the first 50.

## Examples

Go, on the VPS, with the pin recipe:

```go
func fetch(ctx context.Context, client *http.Client, key, target, vpn string) (*http.Response, error) {
	q := url.Values{"url": {target}}
	if vpn != "" {
		q.Set("strategy", "specific")
		q.Set("vpn", vpn)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:9001/proxy?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := client.Do(req) // client.Timeout of 20 s or more
	if err == nil && resp.Header.Get("X-VPN-Used") == "" && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, errors.New("vpn farm rejected the API key")
	}
	return resp, err // pin later requests with resp.Header.Get("X-VPN-Used")
}
```

Python, from another host:

```python
import requests

resp = requests.get(
    "https://proxy.jeab.dev/proxy",
    params={"url": "https://api.ipify.org"},
    headers={"X-Api-Key": API_KEY},
    timeout=25,
)
tunnel = resp.headers.get("X-VPN-Used")  # None means the farm answered, not the target
```

## Checklist for a new integration

1. Read the base URL from an environment variable. Default to `http://127.0.0.1:9001` on the VPS and `https://proxy.jeab.dev` elsewhere.
2. Send the key in `X-Api-Key`. Fail loudly on a `401` that has no `X-VPN-Used`.
3. Set the timeout to 20 seconds or more.
4. Use `X-VPN-Used` to tell farm errors from target errors.
5. Do not repeat a request that may already have run, unless the target treats it as idempotent.
6. Take tunnel names from `/status`. Never hardcode them.
7. Percent-encode the `url` parameter.
