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

## Troubleshooting
- **Deployment Loop:** If the server keeps rebuilding, check if `main` or `dist/` files are being tracked by git. Remove them with `git rm --cached <file>`.
- **Port Conflicts:** Ensure you are using `os.Getenv("PORT")`.
