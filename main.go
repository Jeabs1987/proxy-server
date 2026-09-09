package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// privateNetworks contains CIDR ranges that must never be proxied to (SSRF protection).
var privateNetworks []*net.IPNet

func init() {
	for _, cidr := range []string{
		"0.0.0.0/8",
		"100.64.0.0/10",
		"224.0.0.0/4",
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"::1/128",
		"::/128",
		"ff00::/8",
		"fc00::/7",
		"fe80::/10",
	} {
		_, network, _ := net.ParseCIDR(cidr)
		privateNetworks = append(privateNetworks, network)
	}
}

func isPrivateIP(host string) bool {
	// Strip port if present
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}

	ips, err := net.LookupIP(h)
	if err != nil {
		// If we can't resolve, block it to be safe
		return true
	}

	for _, ip := range ips {
		for _, network := range privateNetworks {
			if network.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// sensitiveHeaders are stripped before forwarding requests to targets.
// The x-forwarded-*/cf-* group carries the caller's own IP, which Nginx and
// Cloudflare add on the way in. Passing those on tells any target that reads
// them exactly who is behind the tunnel, which defeats the point of the farm.
var sensitiveHeaders = map[string]bool{
	"cookie":              true,
	"authorization":       true,
	"x-api-key":           true,
	"set-cookie":          true,
	"proxy-authorization": true,
	"x-forwarded-for":     true,
	"x-forwarded-host":    true,
	"x-forwarded-port":    true,
	"x-forwarded-proto":   true,
	"x-forwarded-server":  true,
	"x-real-ip":           true,
	"forwarded":           true,
	"true-client-ip":      true,
	"cdn-loop":            true,
	"cf-connecting-ip":    true,
	"cf-ipcountry":        true,
	"cf-ray":              true,
	"cf-visitor":          true,
	"cf-worker":           true,
}

func requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := os.Getenv("API_KEY")
		if apiKey == "" {
			// No key configured — deny all to avoid open proxy
			http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
			return
		}

		key := r.Header.Get("X-Api-Key")
		if key == "" {
			key = r.URL.Query().Get("api_key")
		}

		if key != apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

type LogBuffer struct {
	mu      sync.Mutex
	logs    []string
	maxSize int
}

func (l *LogBuffer) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := string(p)
	l.logs = append(l.logs, msg)
	if len(l.logs) > l.maxSize {
		l.logs = l.logs[1:]
	}
	return len(p), nil
}

func (l *LogBuffer) GetLogs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make([]string, len(l.logs))
	copy(result, l.logs)
	return result
}

type VPNEndpoint struct {
	Name     string
	ProxyURL string
	Active   bool
	client   *http.Client // pre-built, reused across all requests
	mu       sync.RWMutex
}

// newEndpointClient builds a reusable HTTP client that routes through the given SOCKS/HTTP proxy.
// Using a single long-lived client per endpoint allows connection pooling and avoids spawning
// new transport goroutines on every request (the cause of CPU spikes under load).
func newEndpointClient(proxyURL string) *http.Client {
	parsedProxy, _ := url.Parse(proxyURL)
	transport := &http.Transport{
		Proxy:               http.ProxyURL(parsedProxy),
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// healthCheckURL is a tiny, reliable target used to verify a tunnel can
// actually reach the internet. Kept lightweight since it is fetched once per
// endpoint per health-check cycle.
const healthCheckURL = "https://api.ipify.org"

// setActive updates the endpoint's Active flag and reports whether it changed.
func (e *VPNEndpoint) setActive(active bool) (changed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	changed = e.Active != active
	e.Active = active
	return changed
}

// isActive reports the endpoint's current Active flag.
func (e *VPNEndpoint) isActive() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Active
}

// probe checks whether the endpoint's VPN tunnel can currently reach the
// internet. Dead PIA regions (gateway EHOSTUNREACH / TLS failures) fail here so
// the routing strategies can skip them instead of returning 502 to callers.
func (e *VPNEndpoint) probe() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthCheckURL, nil)
	if err != nil {
		return false
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

type VPNPool struct {
	endpoints []*VPNEndpoint
	current   int
	mu        sync.Mutex
}

func NewVPNPool() *VPNPool {
	endpoints := []*VPNEndpoint{
		{Name: "US Florida", ProxyURL: "http://127.0.0.1:8881", Active: true},
		{Name: "US California", ProxyURL: "http://127.0.0.1:8882", Active: true},
		{Name: "US Wisconsin", ProxyURL: "http://127.0.0.1:8883", Active: true},
		{Name: "US Salt Lake City", ProxyURL: "http://127.0.0.1:8884", Active: true},
		{Name: "US Chicago", ProxyURL: "http://127.0.0.1:8885", Active: true},
		{Name: "US Seattle", ProxyURL: "http://127.0.0.1:8886", Active: true},
		{Name: "US Denver", ProxyURL: "http://127.0.0.1:8887", Active: true},
		{Name: "Ireland", ProxyURL: "http://127.0.0.1:8888", Active: true},
		{Name: "US Las Vegas", ProxyURL: "http://127.0.0.1:8889", Active: true},
		{Name: "US Washington DC", ProxyURL: "http://127.0.0.1:8890", Active: true},
		{Name: "CA Montreal", ProxyURL: "http://127.0.0.1:8891", Active: true},
		{Name: "US East", ProxyURL: "http://127.0.0.1:8892", Active: true},
		{Name: "US West", ProxyURL: "http://127.0.0.1:8893", Active: true},
		{Name: "US Houston", ProxyURL: "http://127.0.0.1:8894", Active: true},
		{Name: "US New York", ProxyURL: "http://127.0.0.1:8895", Active: true},
		{Name: "US Massachusetts", ProxyURL: "http://127.0.0.1:8896", Active: true},
		{Name: "ES Madrid", ProxyURL: "http://127.0.0.1:8897", Active: true},
		{Name: "IT Milano", ProxyURL: "http://127.0.0.1:8898", Active: true},
		{Name: "US Ohio", ProxyURL: "http://127.0.0.1:8899", Active: true},
		{Name: "US Michigan", ProxyURL: "http://127.0.0.1:8900", Active: true},
		{Name: "SE Stockholm", ProxyURL: "http://127.0.0.1:8901", Active: true},
		{Name: "US North Carolina", ProxyURL: "http://127.0.0.1:8902", Active: true},
		{Name: "US Idaho", ProxyURL: "http://127.0.0.1:8903", Active: true},
		{Name: "US Alabama", ProxyURL: "http://127.0.0.1:8904", Active: true},
		{Name: "US Alaska", ProxyURL: "http://127.0.0.1:8905", Active: true},
		{Name: "US Arkansas", ProxyURL: "http://127.0.0.1:8906", Active: true},
		{Name: "US Baltimore", ProxyURL: "http://127.0.0.1:8907", Active: true},
		{Name: "US Connecticut", ProxyURL: "http://127.0.0.1:8908", Active: true},
		{Name: "US Wilmington", ProxyURL: "http://127.0.0.1:8909", Active: true},
		{Name: "US East Streaming Optimized", ProxyURL: "http://127.0.0.1:8910", Active: true},
		{Name: "US Iowa", ProxyURL: "http://127.0.0.1:8911", Active: true},
		{Name: "US Kansas", ProxyURL: "http://127.0.0.1:8912", Active: true},
		{Name: "US Indiana", ProxyURL: "http://127.0.0.1:8913", Active: true},
		{Name: "US Louisiana", ProxyURL: "http://127.0.0.1:8914", Active: true},
		{Name: "US Maine", ProxyURL: "http://127.0.0.1:8915", Active: true},
		{Name: "US Minnesota", ProxyURL: "http://127.0.0.1:8916", Active: true},
		{Name: "US Mississippi", ProxyURL: "http://127.0.0.1:8917", Active: true},
		{Name: "US Missouri", ProxyURL: "http://127.0.0.1:8918", Active: true},
		{Name: "US Montana", ProxyURL: "http://127.0.0.1:8919", Active: true},
		{Name: "US Nebraska", ProxyURL: "http://127.0.0.1:8920", Active: true},
		{Name: "CA Vancouver", ProxyURL: "http://127.0.0.1:8921", Active: true},
		{Name: "US New Mexico", ProxyURL: "http://127.0.0.1:8922", Active: true},
		{Name: "US North Dakota", ProxyURL: "http://127.0.0.1:8923", Active: true},
		{Name: "Mexico", ProxyURL: "http://127.0.0.1:8924", Active: true},
		{Name: "Netherlands", ProxyURL: "http://127.0.0.1:8925", Active: true},
		{Name: "France", ProxyURL: "http://127.0.0.1:8926", Active: true},
		{Name: "UK London", ProxyURL: "http://127.0.0.1:8927", Active: true},
		{Name: "CA Toronto", ProxyURL: "http://127.0.0.1:8928", Active: true},
		{Name: "DE Berlin", ProxyURL: "http://127.0.0.1:8929", Active: true},
		{Name: "Poland", ProxyURL: "http://127.0.0.1:8930", Active: true},
	}

	// Pre-create one reusable HTTP client per endpoint so handleProxy never
	// allocates a new transport on each request.
	for _, ep := range endpoints {
		ep.client = newEndpointClient(ep.ProxyURL)
	}

	return &VPNPool{
		endpoints: endpoints,
		current:   0,
	}
}

// GetNextEndpoint returns the next active VPN endpoint using round-robin.
// The bool is false when no endpoint is currently active; callers must not use
// the returned pointer in that case.
func (p *VPNPool) GetNextEndpoint() (*VPNEndpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Find next active endpoint
	attempts := 0
	for attempts < len(p.endpoints) {
		p.current = (p.current + 1) % len(p.endpoints)
		endpoint := p.endpoints[p.current]

		endpoint.mu.RLock()
		active := endpoint.Active
		endpoint.mu.RUnlock()

		if active {
			return endpoint, true
		}
		attempts++
	}

	// No active endpoints available.
	log.Printf("WARN GetNextEndpoint: all %d endpoints inactive, returning 503", len(p.endpoints))
	return nil, false
}

// GetRandomEndpoint returns a random active VPN endpoint.
// The bool is false when no endpoint is currently active; callers must not use
// the returned pointer in that case.
func (p *VPNPool) GetRandomEndpoint() (*VPNEndpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	activeEndpoints := make([]*VPNEndpoint, 0)
	for _, ep := range p.endpoints {
		ep.mu.RLock()
		if ep.Active {
			activeEndpoints = append(activeEndpoints, ep)
		}
		ep.mu.RUnlock()
	}

	if len(activeEndpoints) == 0 {
		log.Printf("WARN GetRandomEndpoint: all %d endpoints inactive, returning 503", len(p.endpoints))
		return nil, false
	}

	return activeEndpoints[rand.Intn(len(activeEndpoints))], true
}

// GetEndpointByName returns a specific VPN endpoint by name
func (p *VPNPool) GetEndpointByName(name string) *VPNEndpoint {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, ep := range p.endpoints {
		if ep.Name == name {
			return ep
		}
	}
	return nil
}

// ListEndpoints returns all available endpoints
func (p *VPNPool) ListEndpoints() []*VPNEndpoint {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.endpoints
}

// StartHealthChecks probes every endpoint on an interval and flips Active so the
// routing strategies skip tunnels that cannot currently reach the internet
// (e.g. PIA regions whose gateways are unreachable). It runs one immediate pass,
// then repeats every interval. Non-blocking — startup is not delayed; endpoints
// begin optimistically Active and converge after the first pass (~10s).
func (p *VPNPool) StartHealthChecks(interval time.Duration) {
	go func() {
		p.runHealthCheck()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			p.runHealthCheck()
		}
	}()
}

// runHealthCheck probes all endpoints concurrently and updates their Active flag.
// A semaphore limits concurrent in-flight probes to 10 to avoid a thundering
// herd of HTTPS connections during startup or after a mass container restart.
// Each goroutine also sleeps a small random jitter (0–15 s) after acquiring its
// slot so the actual network calls are spread across the cycle window rather
// than all firing at once.
func (p *VPNPool) runHealthCheck() {
	const maxConcurrent = 10
	sem := make(chan struct{}, maxConcurrent)

	var wg sync.WaitGroup
	for _, ep := range p.endpoints {
		wg.Add(1)
		go func(ep *VPNEndpoint) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Jitter spread across 0–15 s to avoid a synchronised wave.
			time.Sleep(time.Duration(rand.Intn(15)) * time.Second)
			ok := ep.probe()
			if ep.setActive(ok) {
				state := "INACTIVE"
				if ok {
					state = "ACTIVE"
				}
				log.Printf("[health] %s (%s) -> %s", ep.Name, ep.ProxyURL, state)
			}
		}(ep)
	}
	wg.Wait()

	active := 0
	for _, ep := range p.endpoints {
		if ep.isActive() {
			active++
		}
	}
	log.Printf("[health] %d/%d endpoints active", active, len(p.endpoints))
}

type ProxyServer struct {
	vpnPool *VPNPool
}

func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		vpnPool: NewVPNPool(),
	}
}

func (s *ProxyServer) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Get target URL from query parameter or path
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
		return
	}

	// Parse and validate target URL
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, "Invalid URL: "+err.Error(), http.StatusBadRequest)
		return
	}

	if parsedURL.Scheme == "" {
		parsedURL.Scheme = "https"
	}

	// Only allow http/https schemes
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		http.Error(w, "Only http and https schemes are allowed", http.StatusBadRequest)
		return
	}

	// SSRF protection: block requests to private/internal networks
	if isPrivateIP(parsedURL.Host) {
		http.Error(w, "Requests to private/internal networks are not allowed", http.StatusForbidden)
		return
	}

	// Select VPN endpoint based on routing strategy
	strategy := r.URL.Query().Get("strategy")
	vpnName := r.URL.Query().Get("vpn")

	var endpoint *VPNEndpoint
	switch strategy {
	case "random":
		ep, ok := s.vpnPool.GetRandomEndpoint()
		if !ok {
			http.Error(w, "No VPN endpoints available", http.StatusServiceUnavailable)
			return
		}
		endpoint = ep
	case "specific":
		if vpnName != "" {
			endpoint = s.vpnPool.GetEndpointByName(vpnName)
			if endpoint == nil {
				http.Error(w, "VPN endpoint not found: "+vpnName, http.StatusBadRequest)
				return
			}
		} else {
			http.Error(w, "Missing 'vpn' parameter for specific strategy", http.StatusBadRequest)
			return
		}
	default:
		// Round-robin by default
		ep, ok := s.vpnPool.GetNextEndpoint()
		if !ok {
			http.Error(w, "No VPN endpoints available", http.StatusServiceUnavailable)
			return
		}
		endpoint = ep
	}

	// Reuse the pre-built client for this endpoint (avoids spawning a new
	// http.Transport and its goroutines on every request).
	client := endpoint.client

	// Create the proxied request
	proxyReq, err := http.NewRequest(r.Method, parsedURL.String(), r.Body)
	if err != nil {
		http.Error(w, "Failed to create request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers, stripping sensitive ones
	for key, values := range r.Header {
		if sensitiveHeaders[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// Execute the request
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, "Failed to execute request: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Add custom header to indicate which VPN was used
	w.Header().Set("X-VPN-Used", endpoint.Name)

	// Copy status code
	w.WriteHeader(resp.StatusCode)

	// Copy response body
	io.Copy(w, resp.Body)
}

func (s *ProxyServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	endpoints := s.vpnPool.ListEndpoints()

	type endpointStatus struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	}
	type statusResponse struct {
		Endpoints []endpointStatus `json:"endpoints"`
	}

	resp := statusResponse{}
	for _, ep := range endpoints {
		ep.mu.RLock()
		active := ep.Active
		ep.mu.RUnlock()
		resp.Endpoints = append(resp.Endpoints, endpointStatus{Name: ep.Name, Active: active})
	}

	json.NewEncoder(w).Encode(resp)
}

func (s *ProxyServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	html := `
<!DOCTYPE html>
<html>
<head>
    <title>VPN Farm Proxy</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 50px auto; padding: 20px; }
        h1 { color: #333; }
        .endpoint { background: #f5f5f5; padding: 10px; margin: 10px 0; border-radius: 5px; }
        .code { background: #272822; color: #f8f8f2; padding: 15px; border-radius: 5px; overflow-x: auto; }
        pre { margin: 0; }
    </style>
</head>
<body>
    <h1>🌐 VPN Farm Proxy Server</h1>
    <p>Route your HTTP requests through multiple PIA VPN locations.</p>
    
    <h2>Usage</h2>
    <div class="code">
        <pre># Round-robin (default)
curl "http://localhost:8080/proxy?url=https://api.ipify.org?format=json"

# Random selection
curl "http://localhost:8080/proxy?url=https://api.ipify.org?format=json&strategy=random"

# Specific VPN
curl "http://localhost:8080/proxy?url=https://api.ipify.org?format=json&strategy=specific&vpn=US%20East"</pre>
    </div>

    <h2>Available VPNs</h2>
    <div id="endpoints">Loading...</div>

    <h2>Endpoints</h2>
    <ul>
        <li><code>/proxy?url=&lt;target&gt;</code> - Proxy a request through a VPN</li>
        <li><code>/status</code> - Check VPN endpoint status</li>
    </ul>

    <script>
        fetch('/status')
            .then(r => r.json())
            .then(data => {
                const html = data.endpoints.map(ep => 
                    '<div class="endpoint">' +
                    '<strong>' + ep.name + '</strong><br>' +
                    'Status: ' + (ep.active ? '✅ Active' : '❌ Inactive') +
                    '</div>'
                ).join('');
                document.getElementById('endpoints').innerHTML = html;
            });
    </script>
</body>
</html>
`
	fmt.Fprint(w, html)
}

func main() {
	rand.Seed(time.Now().UnixNano())

	server := NewProxyServer()

	// Continuously probe tunnels so round-robin/random routing skips endpoints
	// whose VPN region is currently unreachable (otherwise callers get 502s).
	server.vpnPool.StartHealthChecks(60 * time.Second)

	// Ordinary HTTP-proxy protocol on its own port, for callers that set
	// HTTPS_PROXY instead of calling the /proxy?url= API.
	StartForwardProxy(server.vpnPool)

	http.HandleFunc("/", server.handleRoot)
	http.HandleFunc("/proxy", requireAPIKey(server.handleProxy))
	http.HandleFunc("/status", requireAPIKey(server.handleStatus))

	// Logging and Restart endpoints for compliance
	logBuf := &LogBuffer{maxSize: 1000}
	mw := io.MultiWriter(os.Stdout, logBuf)
	log.SetOutput(mw)

	http.HandleFunc("/api/logs/server", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logBuf.GetLogs())
	}))

	http.HandleFunc("/api/server/restart", requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		log.Println("Restart requested via API")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Restarting..."))

		go func() {
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}()
	}))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting VPN Farm Proxy Server on port %s", port)
	log.Printf("Available VPN endpoints: %d", len(server.vpnPool.endpoints))
	apiServer := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	log.Fatal(apiServer.ListenAndServe())
}
