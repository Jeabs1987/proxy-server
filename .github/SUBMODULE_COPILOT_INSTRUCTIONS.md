<!-- BEGIN GENERATED: synced from the Reverse-Proxy infra repo (.github/SUBMODULE_COPILOT_INSTRUCTIONS.md). DO NOT EDIT inside this block — run scripts/sync-app-docs.sh. -->

# Copilot Instructions for App Development

This repository is deployed as a submodule within a larger infrastructure managed by a Reverse Proxy system.
Follow these instructions to ensure your application deploys and runs correctly in the production environment.

## Deployment Architecture
- **Host:** Ubuntu 24 VPS.
- **Reverse Proxy:** Nginx handles SSL and routing.
- **Process Manager:** Systemd manages the Go binary.
- **Deployment:** Automated via `deploy.sh` on the host, which pulls changes, builds, and restarts services.

## Main VPS fleet and load balancing

Primary is `vps-primary` / `213.199.48.131`; Secondary is `vps-secondary` /
`169.58.2.110`, SSH port 22. Their private `wg-apps` addresses are `10.20.0.1`
and `10.20.0.2`. Use operator SSH configuration; never put keys or credentials in
application repositories. Both hosts now serve selected production applications.
Placement is explicit per app, not an assumption that every service runs twice.

The canonical contract is
[LOAD_BALANCING.md](https://github.com/ArmadaInteractiveCo/Reverse-Proxy/blob/main/.github/LOAD_BALANCING.md).
`configs/lb/apps.json` declares arbitrary named hosts, app placements, environments
and health requirements. New VPSes start disabled. `scripts/lb-fleet.py` enrolls the
private network/firewall and stages the same verified release before an operator
activates traffic. Never run Primary's all-services `deploy.sh` on a worker.
See also [SECONDARY_VPS.md](https://github.com/ArmadaInteractiveCo/Reverse-Proxy/blob/main/.github/SECONDARY_VPS.md).

### Releases, readiness and routing

- Normal pushes build on Primary and synchronize enrolled workers before cache
  purging. Worker activation uses staged runtime/config snapshots and readiness
  checks with rollback/recovery receipts. Identical synchronization is a no-op;
  never overwrite worker-seeded mutable state or restart unchanged applications.
- Primary and workers may briefly serve adjacent builds. Keep APIs and migrations
  compatible. A change that introduces writer fencing requires a coordinated
  drain of all old unfenced writers before activation. A rollout is not an atomic
  distributed transaction; inspect every host's release/readiness result.
- `/health/ready` must verify the dependencies needed to accept new work, including
  subscriptions/registrations for stateful services. Nginx app health removes a bad
  backend independently of slower Cloudflare host monitoring. Stale database
  recovery-copy health is also reported. Do not turn dependency failure into a
  successful empty response or an authentication failure.
- Cloudflare affinity is a performance preference, never an authentication or
  correctness guarantee. Any HTTP request may reach another healthy origin.
  Safe GET fallback is bounded; POST and other state-changing requests are not
  replayed to another upstream. Fixed shard URLs never fall back to another shard.
- Trust forwarded scheme/client-address headers only from the dedicated known
  proxies/private peers. Nginx overwrites forwarded headers before passing them
  upstream. Configure trusted CIDRs narrowly; never accept user-supplied forwarded
  values merely because a request contains those headers.

### Shared data, sessions and background work

- Enrolled replicas use the same authoritative data services over the private
  network. PostgreSQL has one writable primary. MongoDB uses its configured
  primary/replica set. Redis sessions and coordination use one shared authority.
  Secondary recovery copies are not independent application writers.
- Authentication sessions, one-use login tickets, OAuth state, revocation and
  abuse-control buckets must work across origins. Avoid in-memory session stores
  and per-host rate limits that clients can evade by changing origin. Shared
  dependency failures should return unavailable while preserving a valid sign-in.
- Keep durable user data and uploads in shared storage. A local cache is safe only
  if losing or changing hosts cannot change the user's authoritative state. Use
  deadlines, pooled connections, bounded concurrency and batched queries across
  the private link. Check production latency before multiplying host count.
- Scheduled writes, email/messages, payments and game-account jobs need an explicit
  owner or durable idempotency. Disable duplicate workers using the manifest's
  per-host environment overrides. Never run a second independent writer just
  because another VPS is available.
- Shared `localhost` service endpoints are valid on workers only where the infra
  provisions a forwarding service. Declare new remote dependencies explicitly and
  verify their authentication/readiness; do not assume all Primary ports exist.
- Database recovery requires writer fencing before promotion and the documented
  failback procedure. PostgreSQL, MongoDB and persistent Redis replicas improve
  recovery; they do not provide automatic multi-writer application availability.
  Never promote a recovery copy while the old writer can still accept writes.

### Stateful games and lobby routing

A load balancer distributes connections; the game admission service places players.
Fishpals uses a live ready-shard directory, globally unique lobby IDs and codes,
named expiring reservations, per-host budgets and fixed `/ws/shards/<ID>` routes.
Its defaults are ten random players plus ten invitation places per public lobby,
and configurable friends lobbies with owner-controlled access. Adding a compatible
registered shard contributes capacity without changing an application A/B list.

Cross-VPS travel reserves the destination first, commits the final snapshot and a
bound recoverable handoff, then logs in with a fresh shared-session ticket. The
destination confirms lobby, shard, incarnation and admission. PostgreSQL ownership
generations reject old saves/cleanup; Redis presence is only a routing hint. An
uncertain transfer uses receipt recovery. Exact joins never silently matchmake
elsewhere, and parties require explicit departure consent across shards.

Applications with in-memory rooms, sessions or queues must implement their own
correct placement/ownership boundary before enabling multiple copies. Fishpals'
[LOBBY_SYSTEM](https://github.com/ArmadaInteractiveCo/fishpals/blob/main/.github/LOBBY_SYSTEM.md)
is the application contract; it does not make other games automatically shard-safe.
For independently deployed worker roles rather than mirrored web origins, use the
separate `configs/fleet/projects.json` / `.infra/fleet.sh` prepare/activate/health/
rollback workflow described in SECONDARY_VPS. Preserve its explicit role ownership.

## Build & Run Requirements

### 1. Go Backend
- **Binary Name:** The deployment script builds the Go binary and names it `main`.
- **Output Location:** The `main` binary is placed in the root of your application directory (e.g., `apps/<your-app>/main`).
- **Port Binding:**
  - **CRITICAL:** Your application MUST listen on the port defined by the `PORT` environment variable.
  - Do NOT hardcode ports.
  - Example:
    ```go
    port := os.Getenv("PORT")
    if port == "" {
        port = "8080" // Default for local development
    }
    http.ListenAndServe(":"+port, nil)
    ```
- **Environment:**
  - `APP_ENV` will be set to `production` on the server.

### 2. Frontend (if applicable)
- **Build:** The deployment script runs `npm install` and `npm run build` if a `package.json` is found.
- **Output:** Ensure your build script outputs to a standard directory like `dist/` or `build/`.
- **Serving:** Your Go backend should serve these static files.

## Git Configuration (.gitignore)
To prevent deployment loops and conflicts, your `.gitignore` MUST include the following:
```gitignore
# Build Artifacts
main
dist/
build/

# Dependencies
node_modules/

# Environment
.env
```

## Workflow
1.  **Local Development:**
    - Edit locally; run builds, tests and services on an isolated VPS checkout over SSH.
    - Use a `.env` file for local secrets (do not commit it).
2.  **Pushing Changes:**
    - Commit and push to your repository's `main` branch.
    - The VPS will automatically pull changes within ~1 minute.
3.  **Adding Dependencies:**
    - Go: `go get <package>` and commit `go.mod`/`go.sum`.
    - Node: `npm install <package>` and commit `package.json`/`package-lock.json`.

## Test Environments & Logging
For applications deployed to test subdomains (e.g., `test.agar3d.io`), the following logging infrastructure is REQUIRED:

1.  **Log Endpoints:**
    The application MUST expose the following endpoints when running in a non-production environment (or when explicitly configured):
    -   `GET /api/logs/server`: Returns recent server logs (JSON array or plain text).
    -   `POST /api/server/restart`: Restarts the server (e.g., by exiting with status 0 or 1, relying on systemd to restart).

2.  **Log Viewer:**
    The infrastructure provides a static log viewer.
    -   **Nginx Config:** The test domain's Nginx config MUST alias `/logs` to the shared viewer:
        ```nginx
        location /logs {
            alias /opt/reverse-proxy/apps/log-viewer;
            index index.html;
        }
        ```
    -   This viewer expects the API endpoints above to be available relative to the root.

## Payment Integration
The infrastructure includes a unified payment service (`payment-service`) that owns **all** Stripe checkout for the portfolio — **both one-time payments and recurring subscriptions** — plus optional per-app Checkout branding. Apps never embed Stripe keys: they call `/checkout`, implement the entitlement callbacks below, and let payment-service drive Stripe. (The `nurses-compass` NCLEX flow is the reference integration.)

### Service Details
- **Internal URL:** `http://localhost:8089` (accessible from other apps on the VPS)
- **External URL:** `https://payments.armadainteractive.co`

### One-time Payments
1.  **Create Session:** Make a POST request to `/checkout` (omit `mode`, or pass `"mode": "payment"`):
    ```json
    {
      "app_id": "your-app-name",
      "price_id": "price_...", // OR amount (int cents), currency, product_name
      "success_url": "https://your-app.com/success",
      "cancel_url": "https://your-app.com/cancel",
      "metadata": { "app_id": "your-app-name", "user_id": "123", "product_id": "premium" }
    }
    ```
    `/checkout` is public and **rate-limited at the edge** (a few req/sec per IP) — call it for genuine, user-initiated checkout only, never in a loop. For an embedded flow, send `"ui_mode": "embedded"` + `return_url` and read `client_secret` from the response instead of redirecting.
2.  **Redirect:** Response contains `{"session_id": "cs_...", "url": "..."}`. Redirect the user to the Stripe URL.
3.  **Verify:** Poll `GET /transaction?id=<session_id>` to check status (`paid`, `pending`, `expired`).

For one-time payments payment-service is **verify-only** — it does not grant access itself. Your app grants from its own success/webhook flow and answers `POST /api/entitlements/verify` (read-only) so the service can confirm delivery. See [ENTITLEMENTS_VERIFY_CONTRACT.md](ENTITLEMENTS_VERIFY_CONTRACT.md).

### Subscriptions
Send `"mode": "subscription"` to `/checkout` with **either** a pre-made Stripe `price_id` **or** an inline recurring price (`amount` cents + `currency` + `interval` `"month"`/`"year"` (default `"month"`) + `product_name`):
```json
{
  "app_id": "your-app-name",
  "mode": "subscription",
  "amount": 1499,
  "currency": "usd",
  "interval": "month",
  "product_name": "Monthly Plan",
  "success_url": "https://your-app.com/success",
  "cancel_url": "https://your-app.com/cancel",
  "metadata": { "app_id": "your-app-name", "user_id": "123", "product_id": "monthly" }
}
```
`metadata` is propagated onto **both** the Checkout Session and `subscription_data.metadata`, so renewal/cancel webhooks (which have no user present) can still route back to the owning app. Always include `app_id`, `user_id`, and `product_id` in subscription `metadata`.

**Subscription lifecycle is delivered by PUSH, not poll.** payment-service receives the Stripe subscription/invoice webhooks, persists them, and **calls** your app at `POST /api/entitlements/subscription` for each lifecycle change (activate / renew / update / payment-failed / cancel). Unlike read-only verify, **this endpoint mutates your app's entitlement state**. Every app that sells subscriptions must implement it — see the "Subscription lifecycle" section of [ENTITLEMENTS_VERIFY_CONTRACT.md](ENTITLEMENTS_VERIFY_CONTRACT.md) for the event list, body schema, status codes, and idempotency rules.

**Customer billing portal.** To let a subscriber self-serve (update card, view invoices, cancel), `POST /portal-session` to payment-service (`X-Internal-Token: <ENTITLEMENTS_VERIFY_TOKEN>`) with `{ "customer_id": "cus_…", "app_id": "your-app-name", "return_url": "https://…" }` and redirect the user to the returned `{ "url": "https://billing.stripe.com/…" }`. The Stripe customer lives in **our** account, so your app can't mint this session itself — call this route instead of holding a Stripe key. Ownership is `app_id`-scoped server-side (the customer must own one of your subscriptions; unknown/cross-app → `404`). `customer_id` is the `cus_…` you stored from the subscription-push payload / `customer.subscription.created` webhook; `return_url` is optional.

### Reversals (refund / chargeback / fraud)
When a Stripe **refund**, **chargeback** (dispute), or **early-fraud-warning** lands, payment-service **pushes** it to your app at `POST /api/entitlements/reversal` (server-to-server, same `X-Internal-Token` auth, derived from `ENTITLEMENT_VERIFY_URL_<APPID>` by swapping the path). This covers **both** one-time charges and subscription charges. **Any app that sells anything must implement it** to revoke access on money-back events. The events are `refunded` / `chargeback_opened` / `chargeback_lost` / `fraud_warning` → **REVOKE** that purchase's entitlement immediately, and `chargeback_won` → **RESTORE** it. This is the opposite of a subscription `canceled` (which keeps already-paid time): a reversal means the money is gone or held, so access is pulled now. The push payload's top-level `product_id` is **your internal product key** (e.g. `nclex-monthly`), not a Stripe `prod_…` id. ⚠️ There is **no reconciliation pull** for reversals, so the receiver must be deployed **before** payment-service starts sending. Full contract (body schema, status codes, idempotency key `(charge_id, event)`): the "Reversals" section of [ENTITLEMENTS_VERIFY_CONTRACT.md](ENTITLEMENTS_VERIFY_CONTRACT.md).

### Checkout branding
Configure per-app Stripe Checkout branding by setting env var `STRIPE_BRANDING_<APPID>` to a JSON object (keys: `display_name`, `background_color`, `button_color`, `border_style`, `font_family`, `logo_url`, `icon_url`). Branding applies to **hosted** checkout only — Stripe rejects branding on `embedded`/`custom` ui_mode, so it is skipped there. No app code change is required; the service reads the env var by `app_id`.

### Promotion codes / discounts
Stripe coupons and promotion codes are supported on `/checkout` via three optional, **mutually exclusive** body fields (the coupon/code must already exist in our Stripe account — the service references it, it never creates one):
- `"allow_promotion_codes": true` — show the customer-redeemable "Add promotion code" field so the buyer types their own code.
- `"promotion_code": "promo_…"` — pre-apply a specific promotion code (buyer sees it already applied, no field).
- `"coupon": "<coupon-id>"` — pre-apply a specific coupon directly.

```json
{
  "app_id": "your-app-name",
  "price_id": "price_...",
  "allow_promotion_codes": true,
  "success_url": "https://your-app.com/success",
  "cancel_url": "https://your-app.com/cancel"
}
```

Precedence when more than one is sent is `promotion_code` > `coupon` > `allow_promotion_codes` (a pre-applied discount wins and suppresses the redeemable field — Stripe rejects sending both). These work for **one-time and subscription** mode and for **both** hosted and embedded ui_mode. No env var or per-app config is needed; the fields are per-request, so your app decides when to offer a discount.

### Reading `/transaction` — customer PII is token-gated
`GET /transaction?id=<session_id>` always returns the non-sensitive fields to any caller, so unauthenticated status polling keeps working: `id`, `app_id`, `amount`, `currency`, `status`, `created_at`.

The customer **`email`** and the **`metadata`** object (which carries the `user_id` you set at checkout) are returned **only when the request carries a valid internal token**:

```
GET /transaction?id=cs_...
X-Internal-Token: <ENTITLEMENTS_VERIFY_TOKEN>
```

Without the header (or with a wrong token) the response is still `200`, but `email` is `""` and `metadata` is `{}`. **Any server-to-server flow that needs the buyer's email or your `user_id` back (to deliver a product or unlock an account) must send `X-Internal-Token`.** The value is the shared `ENTITLEMENTS_VERIFY_TOKEN` in `/etc/reverse-proxy/secrets.env` — the same secret your `/api/entitlements/verify` endpoint already loads. It is a server-side secret; never send it from the browser (`/transaction` is a backend-to-backend call).

### Confirming delivery (entitlements verify)
After a paid one-time checkout, `payment-service` calls your app's `POST /api/entitlements/verify` (server-to-server, `X-Internal-Token`-authenticated, **read-only**) to confirm the unlock landed and flip the Discord sale embed green/red. Implement that endpoint so sales register as ✅ delivered. Subscriptions are handled separately by the **push** receiver `POST /api/entitlements/subscription` (above). Full contract for both: `ENTITLEMENTS_VERIFY_CONTRACT.md` in the infra (Reverse-Proxy) repo.

### Statement Descriptors
Charges from `payment-service` carry a per-app suffix on the customer's card statement, rendered as `ARMADA* <SUFFIX>` (Stripe caps the combined string at 22 chars). The suffix is resolved by `payment-service` from env var `STRIPE_DESCRIPTOR_<APPID>` (uppercase, hyphens stripped) — **no app code change required**. Apps without a configured value inherit the account default.

## Image Generation (AI)
The infrastructure includes a centralized image generation service via `llm-core` (`llm.jeab.dev`). Requests are routed through the shared openclaw OpenAI OAuth account — **there is no per-app billing cost**. Use this instead of embedding OpenAI API keys in individual apps.

### Service Details
- **Internal URL:** `http://localhost:8083` (accessible from other apps on the VPS)
- **External URL:** `https://llm.jeab.dev`
- **Auth:** Include `X-API-Key` header with the `LLM_API_KEY` value, OR call from a `*.jeab.dev` / `localhost` origin.

### Generate Image
Create images from text prompts.

- **Endpoint:** `POST /api/image/generate`
- **Content-Type:** `application/json`

#### Request Body
```json
{
  "prompt": "A pixel-art treasure chest icon, 64x64, transparent background",
  "model": "gpt-image-2",
  "size": "1024x1024",
  "quality": "high",
  "n": 1,
  "response_format": "b64_json",
  "background": "transparent",
  "output_format": "png"
}
```

#### Parameters
| Parameter | Required | Default | Values |
|-----------|----------|---------|--------|
| `prompt` | Yes | — | Text description of the image |
| `model` | No | `gpt-image-2` | `gpt-image-2`, `gpt-image-1.5`, `gpt-image-1`, `gpt-image-1-mini` |
| `n` | No | 1 | 1–4 |
| `size` | No | `1024x1024` | `1024x1024`, `1536x1024`, `1024x1536`, `2048x2048`, `2048x1152`, `3840x2160`, `2160x3840` |
| `quality` | No | `auto` | `low`, `medium`, `high`, `auto` |
| `response_format` | No | `b64_json` | `url`, `b64_json` |
| `background` | No | `auto` | `transparent`, `opaque`, `auto` |
| `output_format` | No | `png` | `png`, `jpeg`, `webp` |

#### Response
```json
{
  "created": 1234567890,
  "data": [
    {
      "b64_json": "<base64-encoded image data>",
      "revised_prompt": "A detailed pixel-art treasure chest..."
    }
  ]
}
```

### Edit Image (Inpainting)
Modify an existing image with a prompt. Useful for adding/removing elements.

- **Endpoint:** `POST /api/image/edit`
- **Content-Type:** `application/json`

#### Request Body
```json
{
  "image": "<base64-encoded source image>",
  "prompt": "Replace the background with a starry night sky",
  "mask": "<base64-encoded mask (optional, white=edit area)>",
  "model": "gpt-image-2",
  "size": "1024x1024",
  "n": 1,
  "response_format": "b64_json"
}
```

### Usage Examples

**Game sprites (e.g., from agar3d, kidgame):**
```javascript
const response = await fetch('https://llm.jeab.dev/api/image/generate', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'X-API-Key': process.env.LLM_API_KEY
  },
  body: JSON.stringify({
    prompt: 'A cute cartoon fish character, side view, game sprite, transparent background, pixel art style',
    model: 'gpt-image-2',
    size: '1024x1024',
    background: 'transparent',
    output_format: 'png',
    response_format: 'b64_json'
  })
});
const { data } = await response.json();
const imageBuffer = Buffer.from(data[0].b64_json, 'base64');
```

**Go backend usage:**
```go
resp, err := http.Post("http://localhost:8083/api/image/generate",
    "application/json",
    strings.NewReader(`{"prompt":"Icon for a health potion","model":"gpt-image-2","size":"1024x1024","background":"transparent","output_format":"png"}`))
```

### Best Practices
- Use `response_format: "b64_json"` to get image data directly (URLs expire).
- Use `background: "transparent"` with `output_format: "png"` for sprites and icons.
- Use `gpt-image-2` (default) for best overall quality. Use `gpt-image-1-mini` when speed matters more than fidelity.
- Use `size: "2048x2048"` or larger for hero art / marketing images.
- Cache generated images locally — don't re-generate the same asset on every request.
- For batch generation (e.g. generating all game icons at build time), call sequentially to avoid rate limits.

## Customer Support Messaging ("Contact Us")
The infrastructure includes a shared **support-gateway** service that powers a two-way "Contact us" chat backed by a Discord channel. A visitor opens the widget on your app and submits a message; it posts to a central Discord support channel as an embed with its own thread. Staff reply in that thread and the reply streams back to the visitor's browser in real time (SSE). Unlike the payment/image services there is **no server-to-server contract to implement** — the widget is fully self-contained and same-origin.

### Service Details
- **Internal URL:** `http://localhost:9005` (loopback-only; the gateway never gets its own public domain).
- **Reached from your app** over a same-origin `/api/support/` path proxied by your app's own nginx vhost — so there is no CORS and no cross-origin EventSource/cookie friction.

### Integrating your app (mostly infra-side; no app-backend code)
1.  **Allow-list:** add your app to `apps/support-gateway/src/apps.ts` (`{ label, color }` — drives the Discord embed). Unknown `app_id`s are rejected by the gateway.
2.  **nginx (infra repo):** add a `location /api/support/` block to your app's vhost, proxying to `127.0.0.1:9005` with SSE tuning + the shared rate-limit zone (defined once in `configs/nginx/00-support-gateway.conf`):
    ```nginx
    location /api/support/ {
        limit_req zone=support_limit burst=10 nodelay;
        proxy_pass http://127.0.0.1:9005;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;                 # SSE — do not buffer the event stream
        proxy_read_timeout 86400s;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        add_header Cache-Control "no-transform";
    }
    ```
3.  **Frontend:** drop the widget script into your page — one tag, no build step, no dependencies:
    ```html
    <script defer src="/api/support/widget.js" data-app-id="your-app-name"></script>
    ```

That's the whole integration: **no app-backend code and no secrets.** The widget renders a floating "Contact us" button (its styles isolated in a shadow DOM so they can't clash with your CSS), persists the conversation in `localStorage` so a returning visitor resumes the same thread, and handles SSE reconnection/replay automatically.

### Behavior notes
- **Real-time both ways:** customer follow-ups relay into the Discord thread; staff replies — and conversation close (staff `!close` or archiving the thread) — stream to the browser over SSE with `Last-Event-ID` replay after a reconnect.
- **Auth:** each conversation is authenticated by a random per-conversation bearer token (stored server-side only as a hash) minted at creation and held in the visitor's `localStorage`. There is **no shared app secret** for you to manage.
- **Abuse controls** live in the gateway + the nginx `limit_req` (honeypot field, submit-timing check, per-IP conversation caps, per-conversation message caps, length limits).
- Closed conversations are retained 30 days, then purged.

## Troubleshooting
- **Deployment Loop:** If the server keeps rebuilding, check if `main` or `dist/` files are being tracked by git. Remove them with `git rm --cached <file>`.
- **Port Conflicts:** Ensure you are using `os.Getenv("PORT")`.

<!-- END GENERATED. App-specific content may follow below and is preserved by the sync. -->
