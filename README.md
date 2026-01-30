# VPN Farm 🌐

A lightweight system for hosting multiple Private Internet Access (PIA) VPN instances with different connection locations and routing traffic through them via a Go proxy server.

## Features

- 🚀 **Multiple VPN Locations**: Run 50 concurrent VPN connections across US regions
- 🔄 **Smart Routing**: Round-robin, random, or specific VPN selection
- 🐳 **Docker-based**: Lightweight, isolated VPN containers using Gluetun
- 🔧 **Easy Configuration**: Simple environment-based setup
- 📊 **Status Monitoring**: Web interface to check VPN endpoint status
- ⚡ **High Performance**: Efficient Go-based proxy server

## Architecture

```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │ HTTP Request
       ▼
┌─────────────────────┐
│  Go Proxy Server    │
│  (Port 8080)        │
└──────┬──────────────┘
       │ Distributes traffic
       ▼
┌──────────────────────────────────────────┐
│              VPN Containers              │
├──────────┬──────────┬──────────┬─────────┤
│ Florida  │California│ New York │  Texas  │
│          │          │          │         │
│ Chicago  │          │          │         │
└──────────┴──────────┴──────────┴─────────┘
       │
       ▼ Encrypted VPN Traffic
   Internet
```

## Prerequisites

- Docker and Docker Compose
- Private Internet Access subscription
- Linux system with kernel support for TUN/TAP devices

## Quick Start

### 1. Clone and Configure

```bash
cd /home/jbz/Documents/vpn-farm

# Create .env file with your PIA credentials
cp .env.example .env
nano .env  # Add your PIA_USERNAME and PIA_PASSWORD
```

### 2. Start the VPN Farm

```bash
# Pull images and start all services
docker compose up -d

# Check status
docker compose ps

# View logs
docker compose logs -f
```

### 3. Test the Proxy

Visit http://localhost:8080 in your browser for the web interface, or use curl:

```bash
# Test with round-robin routing (default)
curl "http://localhost:8080/proxy?url=https://api.ipify.org?format=json"

# Test with random VPN selection
curl "http://localhost:8080/proxy?url=https://api.ipify.org?format=json&strategy=random"

# Test with specific VPN location
curl "http://localhost:8080/proxy?url=https://ipinfo.io&strategy=specific&vpn=UK%20London"

# Check VPN status
curl http://localhost:8080/status
```

## Usage

### API Endpoints

#### `GET /`
Web interface showing available VPNs and usage examples.

#### `GET /proxy`
Proxy HTTP requests through VPN connections.

**Query Parameters:**
- `url` (required): Target URL to request
- `strategy` (optional): Routing strategy
  - `roundrobin` (default): Distribute requests evenly across VPNs
  - `random`: Select a random VPN for each request
  - `specific`: Use a specific VPN (requires `vpn` parameter)
- `vpn` (optional): VPN name when using `strategy=specific`
  - Available: `US Florida`, `US California`, `US New York`, `US Texas`, `US Chicago`

**Response Headers:**
- `X-VPN-Used`: Name of the VPN that handled the request
- `X-VPN-Proxy`: Internal proxy URL used

#### `GET /status`
Returns JSON with status of all VPN endpoints.

### Examples

#### Basic Usage
```bash
# Get your IP through a VPN
curl "http://localhost:8080/proxy?url=https://api.ipify.org"
```

#### Fetch a Website
```bash
# Fetch through Texas VPN
curl "http://localhost:8080/proxy?url=https://example.com&strategy=specific&vpn=US%20Texas"
```

#### API Requests
```bash
# Make API call through New York VPN
curl -H "Accept: application/json" \
  "http://localhost:8080/proxy?url=https://api.github.com&strategy=specific&vpn=US%20New%20York"
```

#### Check Which VPN Was Used
```bash
curl -I "http://localhost:8080/proxy?url=https://httpbin.org/ip" | grep X-VPN
```

## Configuration

### Adding More VPN Locations

Edit [docker-compose.yml](docker-compose.yml) to add more VPN containers:

```yaml
vpn-australia:
  image: qmcgaw/gluetun
  container_name: vpn-australia
  cap_add:
    - NET_ADMIN
  environment:
    - VPN_SERVICE_PROVIDER=private internet access
    - OPENVPN_USER=${PIA_USERNAME}
    - OPENVPN_PASSWORD=${PIA_PASSWORD}
    - SERVER_REGIONS=AU Sydney
    - FIREWALL_OUTBOUND_SUBNETS=172.22.0.0/16
  networks:
    vpn_network:
      ipv4_address: 172.22.0.15
  ports:
    - "8886:8888"
  restart: unless-stopped
```

Then update [main.go](main.go) to add the endpoint:

```go
{Name: "Australia", ProxyURL: "http://172.22.0.15:8888", Active: true},
```

### Available PIA Regions

Some popular regions (check [Gluetun wiki](https://github.com/qdm12/gluetun-wiki/blob/main/setup/providers/private-internet-access.md) for full list):
- US East, US West, US Texas, US California, US Florida
- UK London, UK Manchester
- CA Toronto, CA Vancouver
- AU Sydney, AU Melbourne
- Germany, France, Netherlands, Switzerland, Sweden
- Japan, Singapore, Hong Kong, India

## Management

### View Logs
```bash
# All services
docker compose logs -f

# Specific VPN
docker compose logs -f vpn-us-east

# Proxy server
docker compose logs -f proxy-server
```

### Restart Services
```bash
# Restart all
docker compose restart

# Restart specific VPN
docker compose restart vpn-uk
```

### Stop Services
```bash
# Stop all
docker compose down

# Stop but keep volumes
docker compose stop
```

### Update
```bash
# Pull latest images
docker compose pull

# Rebuild and restart
docker compose up -d --build
```

## Troubleshooting

### VPN Connection Issues

Check VPN container logs:
```bash
docker compose logs vpn-us-east
```

Verify VPN is connected:
```bash
docker exec vpn-us-east wget -qO- https://api.ipify.org
```

### Proxy Not Working

1. Check if proxy server is running:
```bash
docker compose ps proxy-server
```

2. Check network connectivity:
```bash
docker exec proxy-server ping -c 3 172.22.0.10
```

3. Verify VPN containers are on the correct network:
```bash
docker network inspect vpn-farm_vpn_network
```

### Performance Issues

- Reduce number of concurrent VPN connections
- Increase timeout values in [main.go](main.go)
- Check your internet bandwidth and PIA server load

## Security Considerations

- The proxy server should not be exposed directly to the internet without authentication
- Consider adding API keys or basic auth for production use
- VPN credentials in `.env` should be kept secure (not committed to git)
- All traffic is encrypted within VPN tunnels

## Development

### Local Development Without Docker

```bash
# Set environment variable
export PORT=8080

# Run directly
go run main.go
```

Note: You'll need to point the proxy URLs to accessible VPN endpoints.

### Building

```bash
# Build binary
go build -o vpn-proxy-server .

# Run
./vpn-proxy-server
```

## License

MIT

## Credits

- VPN connectivity powered by [Gluetun](https://github.com/qdm12/gluetun)
- Built for use with [Private Internet Access](https://www.privateinternetaccess.com/)
# vpn-farm
