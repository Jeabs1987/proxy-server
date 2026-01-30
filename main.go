package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

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
	mu       sync.RWMutex
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
		{Name: "US Texas", ProxyURL: "http://127.0.0.1:8884", Active: true},
		{Name: "US Chicago", ProxyURL: "http://127.0.0.1:8885", Active: true},
		{Name: "US Seattle", ProxyURL: "http://127.0.0.1:8886", Active: true},
		{Name: "US Denver", ProxyURL: "http://127.0.0.1:8887", Active: true},
		{Name: "US Atlanta", ProxyURL: "http://127.0.0.1:8888", Active: true},
		{Name: "US Las Vegas", ProxyURL: "http://127.0.0.1:8889", Active: true},
		{Name: "US Washington DC", ProxyURL: "http://127.0.0.1:8890", Active: true},
		{Name: "US Silicon Valley", ProxyURL: "http://127.0.0.1:8891", Active: true},
		{Name: "US East", ProxyURL: "http://127.0.0.1:8892", Active: true},
		{Name: "US West", ProxyURL: "http://127.0.0.1:8893", Active: true},
		{Name: "US Houston", ProxyURL: "http://127.0.0.1:8894", Active: true},
		{Name: "US Wyoming", ProxyURL: "http://127.0.0.1:8895", Active: true},
		{Name: "US Massachusetts", ProxyURL: "http://127.0.0.1:8896", Active: true},
		{Name: "US Oregon", ProxyURL: "http://127.0.0.1:8897", Active: true},
		{Name: "US Virginia", ProxyURL: "http://127.0.0.1:8898", Active: true},
		{Name: "US Ohio", ProxyURL: "http://127.0.0.1:8899", Active: true},
		{Name: "US Michigan", ProxyURL: "http://127.0.0.1:8900", Active: true},
		{Name: "US Pennsylvania", ProxyURL: "http://127.0.0.1:8901", Active: true},
		{Name: "US North Carolina", ProxyURL: "http://127.0.0.1:8902", Active: true},
		{Name: "US Idaho", ProxyURL: "http://127.0.0.1:8903", Active: true},
		{Name: "US Alabama", ProxyURL: "http://127.0.0.1:8904", Active: true},
		{Name: "US Alaska", ProxyURL: "http://127.0.0.1:8905", Active: true},
		{Name: "US Arkansas", ProxyURL: "http://127.0.0.1:8906", Active: true},
		{Name: "US Baltimore", ProxyURL: "http://127.0.0.1:8907", Active: true},
		{Name: "US Connecticut", ProxyURL: "http://127.0.0.1:8908", Active: true},
		{Name: "US Honolulu", ProxyURL: "http://127.0.0.1:8909", Active: true},
		{Name: "US Indiana", ProxyURL: "http://127.0.0.1:8910", Active: true},
		{Name: "US Iowa", ProxyURL: "http://127.0.0.1:8911", Active: true},
		{Name: "US Kansas", ProxyURL: "http://127.0.0.1:8912", Active: true},
		{Name: "US Kentucky", ProxyURL: "http://127.0.0.1:8913", Active: true},
		{Name: "US Louisiana", ProxyURL: "http://127.0.0.1:8914", Active: true},
		{Name: "US Maine", ProxyURL: "http://127.0.0.1:8915", Active: true},
		{Name: "US Minnesota", ProxyURL: "http://127.0.0.1:8916", Active: true},
		{Name: "US Mississippi", ProxyURL: "http://127.0.0.1:8917", Active: true},
		{Name: "US Missouri", ProxyURL: "http://127.0.0.1:8918", Active: true},
		{Name: "US Montana", ProxyURL: "http://127.0.0.1:8919", Active: true},
		{Name: "US Nebraska", ProxyURL: "http://127.0.0.1:8920", Active: true},
		{Name: "US New Hampshire", ProxyURL: "http://127.0.0.1:8921", Active: true},
		{Name: "US New Mexico", ProxyURL: "http://127.0.0.1:8922", Active: true},
		{Name: "US North Dakota", ProxyURL: "http://127.0.0.1:8923", Active: true},
		{Name: "US Oklahoma", ProxyURL: "http://127.0.0.1:8924", Active: true},
		{Name: "US Rhode Island", ProxyURL: "http://127.0.0.1:8925", Active: true},
		{Name: "US South Carolina", ProxyURL: "http://127.0.0.1:8926", Active: true},
		{Name: "US South Dakota", ProxyURL: "http://127.0.0.1:8927", Active: true},
		{Name: "US Tennessee", ProxyURL: "http://127.0.0.1:8928", Active: true},
		{Name: "US Vermont", ProxyURL: "http://127.0.0.1:8929", Active: true},
		{Name: "US West Virginia", ProxyURL: "http://127.0.0.1:8930", Active: true},
	}

	return &VPNPool{
		endpoints: endpoints,
		current:   0,
	}
}

// GetNextEndpoint returns the next VPN endpoint using round-robin
func (p *VPNPool) GetNextEndpoint() *VPNEndpoint {
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
			return endpoint
		}
		attempts++
	}

	// If no active endpoints, return the first one anyway
	return p.endpoints[0]
}

// GetRandomEndpoint returns a random VPN endpoint
func (p *VPNPool) GetRandomEndpoint() *VPNEndpoint {
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
		return p.endpoints[0]
	}

	return activeEndpoints[rand.Intn(len(activeEndpoints))]
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

type ProxyServer struct {
	vpnPool *VPNPool
	client  *http.Client
}

func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		vpnPool: NewVPNPool(),
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
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

	// Select VPN endpoint based on routing strategy
	strategy := r.URL.Query().Get("strategy")
	vpnName := r.URL.Query().Get("vpn")

	var endpoint *VPNEndpoint
	switch strategy {
	case "random":
		endpoint = s.vpnPool.GetRandomEndpoint()
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
		endpoint = s.vpnPool.GetNextEndpoint()
	}

	// Create proxy URL
	proxyURL, err := url.Parse(endpoint.ProxyURL)
	if err != nil {
		http.Error(w, "Invalid proxy URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Create a new HTTP client with the VPN proxy
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	// Create the proxied request
	proxyReq, err := http.NewRequest(r.Method, parsedURL.String(), r.Body)
	if err != nil {
		http.Error(w, "Failed to create request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// Add info header about which VPN was used
	log.Printf("Routing request to %s through VPN: %s", targetURL, endpoint.Name)

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
	w.Header().Set("X-VPN-Proxy", endpoint.ProxyURL)

	// Copy status code
	w.WriteHeader(resp.StatusCode)

	// Copy response body
	io.Copy(w, resp.Body)
}

func (s *ProxyServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	endpoints := s.vpnPool.ListEndpoints()
	fmt.Fprintf(w, "{\n")
	fmt.Fprintf(w, "  \"endpoints\": [\n")

	for i, ep := range endpoints {
		ep.mu.RLock()
		active := ep.Active
		ep.mu.RUnlock()

		comma := ","
		if i == len(endpoints)-1 {
			comma = ""
		}

		fmt.Fprintf(w, "    {\n")
		fmt.Fprintf(w, "      \"name\": \"%s\",\n", ep.Name)
		fmt.Fprintf(w, "      \"proxy\": \"%s\",\n", ep.ProxyURL)
		fmt.Fprintf(w, "      \"active\": %t\n", active)
		fmt.Fprintf(w, "    }%s\n", comma)
	}

	fmt.Fprintf(w, "  ]\n")
	fmt.Fprintf(w, "}\n")
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

	http.HandleFunc("/", server.handleRoot)
	http.HandleFunc("/proxy", server.handleProxy)
	http.HandleFunc("/status", server.handleStatus)

	// Logging and Restart endpoints for compliance
	logBuf := &LogBuffer{maxSize: 1000}
	mw := io.MultiWriter(os.Stdout, logBuf)
	log.SetOutput(mw)

	http.HandleFunc("/api/logs/server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logBuf.GetLogs())
	})

	http.HandleFunc("/api/server/restart", func(w http.ResponseWriter, r *http.Request) {
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
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting VPN Farm Proxy Server on port %s", port)
	log.Printf("Available VPN endpoints: %d", len(server.vpnPool.endpoints))
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
