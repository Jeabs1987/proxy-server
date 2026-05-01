# Copilot Instructions for App Development

This repository is deployed as a submodule within a larger infrastructure managed by a Reverse Proxy system.
Follow these instructions to ensure your application deploys and runs correctly in the production environment.

## Deployment Architecture
- **Host:** Ubuntu 24 VPS.
- **Reverse Proxy:** Nginx handles SSL and routing.
- **Process Manager:** Systemd manages the Go binary.
- **Deployment:** Automated via `deploy.sh` on the host, which pulls changes, builds, and restarts services.

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
    - Run `go run main.go` (or similar).
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
The infrastructure includes a unified payment service (`payment-service`) for handling Stripe checkouts and subscriptions.

### Service Details
- **Internal URL:** `http://localhost:8089` (accessible from other apps on the VPS)
- **External URL:** `https://payments.armadainteractive.co`

### One-time Payments
1.  **Create Session:** Make a POST request to `/checkout`:
    ```json
    {
      "app_id": "your-app-name",
      "price_id": "price_...", // OR amount (int cents), currency, product_name
      "success_url": "https://your-app.com/success",
      "cancel_url": "https://your-app.com/cancel",
      "metadata": { "user_id": "123" }
    }
    ```
2.  **Redirect:** Response contains `{"url": "..."}`. Redirect the user to this Stripe URL.
3.  **Verify:** Poll `GET /transaction?id=<session_id>` to check status (`paid`, `pending`, `expired`).

### Subscriptions
Subscriptions require a recurring **Price ID** created in the Stripe Dashboard:
- Dashboard → Products → Create Product → Add a Price with recurring billing
- This gives you a `price_id` like `price_1ABC...`
- Create separate prices for different plans/intervals (e.g. monthly, yearly)

**1. Create a subscription checkout session:**
```json
POST /checkout
{
  "app_id": "your-app-name",
  "mode": "subscription",
  "price_id": "price_...",
  "success_url": "https://your-app.com/success",
  "cancel_url": "https://your-app.com/cancel",
  "metadata": { "user_id": "123" }
}
```
Response:
```json
{ "session_id": "cs_...", "url": "https://checkout.stripe.com/..." }
```
Redirect the user to the URL. After checkout, the `subscription_id` (`sub_...`) is available via the `customer.subscription.created` webhook — store it alongside the user in your own database.

**2. Check subscription status:**
```
GET /subscription?id=<subscription_id>
```
Returns: `id`, `status` (`active`, `past_due`, `canceled`, etc.), `price_id`, `current_period_end`, `cancel_at_period_end`.

**3. List subscriptions for your app:**
```
GET /subscriptions?app_id=<your-app-name>
```

**4. Cancel a subscription:**
```json
POST /subscription/cancel
{
  "subscription_id": "sub_...",
  "at_period_end": true
}
```
- `at_period_end: true` — stays active until the end of the billing period (recommended)
- `at_period_end: false` — cancels immediately

### Subscription Status Lifecycle
| Status | Meaning |
|--------|---------|
| `active` | Subscription is active and billing normally |
| `past_due` | Latest invoice payment failed, Stripe is retrying |
| `canceled` | Subscription has been canceled |
| `unpaid` | Payment retries exhausted |
| `trialing` | In a free trial period |

Your app should gate access based on status being `active` (or `trialing` if you offer trials). Poll `/subscription?id=<sub_id>` to check the current status on demand.

## Image Generation (AI)
The infrastructure includes a centralized image generation service via `llm-core` (`llm.jeab.dev`), powered by OpenAI's API. Use this instead of embedding API keys in individual apps.

### Service Details
- **Internal URL:** `http://localhost:8083` (accessible from other apps on the VPS)
- **External URL:** `https://llm.jeab.dev`
- **Auth:** Include `X-API-Key` header with the `LLM_API_KEY` value, OR call from a `*.jeab.dev` / `localhost` origin.

### Generate Image
Create images from text prompts using DALL-E 3, DALL-E 2, or GPT Image 1.

- **Endpoint:** `POST /api/image/generate`
- **Content-Type:** `application/json`

#### Request Body
```json
{
  "prompt": "A pixel-art treasure chest icon, 64x64, transparent background",
  "model": "gpt-image-1",
  "size": "1024x1024",
  "quality": "hd",
  "style": "natural",
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
| `model` | No | `gpt-image-1` | `gpt-image-1`, `dall-e-3`, `dall-e-2` |
| `n` | No | 1 | 1-10 (DALL-E 3 only supports 1) |
| `size` | No | `1024x1024` | `1024x1024`, `1792x1024`, `1024x1792`, `256x256`, `512x512` |
| `quality` | No | `standard` | `standard`, `hd` (DALL-E 3 only) |
| `style` | No | `vivid` | `vivid`, `natural` (DALL-E 3 only) |
| `response_format` | No | `b64_json` | `url`, `b64_json` |
| `background` | No | — | `transparent`, `opaque`, `auto` (gpt-image-1 only) |
| `output_format` | No | — | `png`, `jpeg`, `webp` (gpt-image-1 only) |

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
  "model": "gpt-image-1",
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
    model: 'gpt-image-1',
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
    strings.NewReader(`{"prompt":"Icon for a health potion","model":"gpt-image-1","size":"256x256","background":"transparent","output_format":"png"}`))
```

### Best Practices
- Use `response_format: "b64_json"` to get the image data directly (URLs expire after 1 hour).
- Use `gpt-image-1` for sprites/icons that need transparent backgrounds.
- Use `dall-e-3` when you need the highest artistic quality and don't need transparency.
- Cache generated images locally — don't re-generate the same asset repeatedly.
- For batch generation (e.g., generating all game icons at build time), call sequentially to avoid rate limits.

## Troubleshooting
- **Deployment Loop:** If the server keeps rebuilding, check if `main` or `dist/` files are being tracked by git. Remove them with `git rm --cached <file>`.
- **Port Conflicts:** Ensure you are using `os.Getenv("PORT")`.
