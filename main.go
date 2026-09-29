package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

		if subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) != 1 {
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
	display  string       // ProxyURL with any password masked, for logs
	client   *http.Client // pre-built, reused across all requests
	mu       sync.RWMutex

	exitIP    string    // guarded by mu: the IP the last successful probe saw
	checkedAt time.Time // guarded by mu
	checking  atomic.Bool
	stats     endpointStats
}

// endpointStats counts the attempts routed through one tunnel since start.
// They show which exit IPs the target rate-limits or blocks.
type endpointStats struct {
	requests    atomic.Int64
	ok          atomic.Int64 // status below 400
	rateLimited atomic.Int64 // 429
	forbidden   atomic.Int64 // 403, often a Cloudflare block of the exit IP
	upstream5xx atomic.Int64
	errors      atomic.Int64 // no response: the tunnel or the target failed
}

func (s *endpointStats) record(status int) {
	s.requests.Add(1)
	switch {
	case status == http.StatusTooManyRequests:
		s.rateLimited.Add(1)
	case status == http.StatusForbidden:
		s.forbidden.Add(1)
	case status >= 500:
		s.upstream5xx.Add(1)
	case status < 400:
		s.ok.Add(1)
	}
}

func (s *endpointStats) recordError() {
	s.requests.Add(1)
	s.errors.Add(1)
}

// newEndpointClient builds a reusable HTTP client that routes through the given SOCKS/HTTP proxy.
// Using a single long-lived client per endpoint allows connection pooling and avoids spawning
// new transport goroutines on every request (the cause of CPU spikes under load).
func newEndpointClient(proxyURL *url.URL) *http.Client {
	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// connectBudget bounds the connect phase of an attempt that another tunnel
// can still take over: the dial to the tunnel's proxy, its CONNECT to the
// target, and the TLS handshake. A hung tunnel fails here, before the target
// sees anything, which leaves the caller time to try another tunnel inside
// its own timeout. The last attempt has no budget, so on an overloaded host a
// slow tunnel can still finish within the client timeout.
const connectBudget = 6 * time.Second

// maxAttempts is how many tunnels one request may try. Only failures that
// happen before the request reaches the target are retried.
const maxAttempts = 3

// do sends req through the tunnel. A budget above zero bounds the connect
// phase (see connectBudget). sent reports whether the target may have
// received the request. When it is false, a retry on another tunnel is safe
// for any method. On success the caller must call release after it has read
// the response body.
func (e *VPNEndpoint) do(req *http.Request, budget time.Duration) (resp *http.Response, sent bool, release context.CancelFunc, err error) {
	ctx, cancel := context.WithCancel(req.Context())
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	if budget > 0 {
		timer := time.AfterFunc(budget, func() {
			if !connected.Load() {
				cancel()
			}
		})
		defer timer.Stop()
	}

	resp, err = e.client.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
	if err != nil {
		if !connected.Load() && ctx.Err() != nil && req.Context().Err() == nil {
			err = fmt.Errorf("tunnel did not connect within %s", budget)
		}
		cancel()
		return nil, connected.Load(), nil, err
	}
	return resp, true, cancel, nil
}

// healthCheckURLs are small targets that prove a tunnel reaches the internet
// and report its exit IP. They sit on separate networks, so an outage at one
// of them cannot mark the whole farm dead. The second runs only when the
// first fails.
var healthCheckURLs = []string{
	"https://api.ipify.org",
	"https://1.1.1.1/cdn-cgi/trace",
}

// isActive reports the endpoint's current Active flag.
func (e *VPNEndpoint) isActive() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Active
}

// probe checks whether the endpoint's VPN tunnel can currently reach the
// internet, and returns the exit IP it saw. Dead PIA regions (gateway
// EHOSTUNREACH / TLS failures) fail here so the routing strategies can skip
// them instead of returning 502 to callers.
func (e *VPNEndpoint) probe() (ok bool, exitIP string) {
	for _, target := range healthCheckURLs {
		if ip, err := e.fetchExitIP(target); err == nil {
			return true, ip
		}
	}
	return false, ""
}

func (e *VPNEndpoint) fetchExitIP(target string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return parseExitIP(string(body)), nil
}

// parseExitIP reads the IP from an ipify body ("1.2.3.4") or a Cloudflare
// trace ("ip=1.2.3.4" on its own line). It returns "" when neither matches.
func parseExitIP(body string) string {
	for _, line := range strings.Split(body, "\n") {
		candidate := strings.TrimPrefix(strings.TrimSpace(line), "ip=")
		if net.ParseIP(candidate) != nil {
			return candidate
		}
	}
	return ""
}

// check probes the tunnel and records the result.
func (e *VPNEndpoint) check() {
	ok, exitIP := e.probe()

	e.mu.Lock()
	changed := e.Active != ok
	e.Active = ok
	e.checkedAt = time.Now()
	if exitIP != "" {
		e.exitIP = exitIP
	}
	e.mu.Unlock()

	if changed {
		state := "INACTIVE"
		if ok {
			state = "ACTIVE (exit " + exitIP + ")"
		}
		log.Printf("[health] %s (%s) -> %s", e.Name, e.display, state)
	}
}

// recheck probes the tunnel now, off the request path, after a request
// through it failed. The probe decides whether the tunnel is dead, so a
// caller that asks for a broken target cannot take healthy tunnels out of
// the rotation. At most one recheck per tunnel runs at a time.
func (e *VPNEndpoint) recheck() {
	if !e.checking.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.checking.Store(false)
		e.check()
	}()
}

// Routing strategies. Anything other than random or specific rotates
// round-robin.
const (
	strategyRoundRobin = "roundrobin"
	strategyRandom     = "random"
	strategySpecific   = "specific"
)

type VPNPool struct {
	endpoints []*VPNEndpoint
	current   int
	mu        sync.Mutex
}

// NewVPNPool builds the pool from the endpoint list (see loadEndpoints).
func NewVPNPool() (*VPNPool, error) {
	endpoints, err := loadEndpoints()
	if err != nil {
		return nil, err
	}
	return &VPNPool{endpoints: endpoints}, nil
}

// GetNextEndpoint returns the next active VPN endpoint that is not in exclude,
// using round-robin. The bool is false when there is none; callers must not
// use the returned pointer in that case.
func (p *VPNPool) GetNextEndpoint(exclude map[*VPNEndpoint]bool) (*VPNEndpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for range p.endpoints {
		p.current = (p.current + 1) % len(p.endpoints)
		endpoint := p.endpoints[p.current]
		if endpoint.isActive() && !exclude[endpoint] {
			return endpoint, true
		}
	}

	if len(exclude) == 0 {
		log.Printf("WARN GetNextEndpoint: all %d endpoints inactive, returning 503", len(p.endpoints))
	}
	return nil, false
}

// GetRandomEndpoint returns a random active VPN endpoint that is not in
// exclude. The bool is false when there is none; callers must not use the
// returned pointer in that case.
func (p *VPNPool) GetRandomEndpoint(exclude map[*VPNEndpoint]bool) (*VPNEndpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	activeEndpoints := make([]*VPNEndpoint, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		if ep.isActive() && !exclude[ep] {
			activeEndpoints = append(activeEndpoints, ep)
		}
	}

	if len(activeEndpoints) == 0 {
		if len(exclude) == 0 {
			log.Printf("WARN GetRandomEndpoint: all %d endpoints inactive, returning 503", len(p.endpoints))
		}
		return nil, false
	}

	return activeEndpoints[rand.Intn(len(activeEndpoints))], true
}

// pick returns an active endpoint outside exclude for a rotating strategy.
func (p *VPNPool) pick(strategy string, exclude map[*VPNEndpoint]bool) (*VPNEndpoint, bool) {
	if strategy == strategyRandom {
		return p.GetRandomEndpoint(exclude)
	}
	return p.GetNextEndpoint(exclude)
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

// forward sends a request through first. When a tunnel fails before the
// target sees the request, forward tries up to maxAttempts-1 other tunnels
// chosen by strategy (never for specific). newReq builds a fresh request for
// each attempt. On success the caller must call release after it has read
// the response body.
func (p *VPNPool) forward(first *VPNEndpoint, strategy string, newReq func() (*http.Request, error)) (*http.Response, *VPNEndpoint, context.CancelFunc, error) {
	tried := make(map[*VPNEndpoint]bool)
	endpoint := first
	for attempt := 1; ; attempt++ {
		tried[endpoint] = true
		req, err := newReq()
		if err != nil {
			return nil, endpoint, nil, err
		}

		retryable := strategy != strategySpecific && attempt < maxAttempts
		budget := time.Duration(0)
		if retryable {
			budget = connectBudget
		}
		resp, sent, release, err := endpoint.do(req, budget)
		if err == nil {
			endpoint.stats.record(resp.StatusCode)
			return resp, endpoint, release, nil
		}
		if req.Context().Err() != nil {
			return nil, endpoint, nil, err // the caller left, so this is not the tunnel's fault
		}
		endpoint.stats.recordError()
		endpoint.recheck()

		var next *VPNEndpoint
		if !sent && retryable {
			next, _ = p.pick(strategy, tried)
		}
		if next == nil {
			return nil, endpoint, nil, err
		}
		log.Printf("WARN proxy: %s failed before the target saw the request (%v), retrying on %s", endpoint.Name, err, next.Name)
		endpoint = next
	}
}

// StartHealthChecks probes every endpoint on an interval and flips Active so the
// routing strategies skip tunnels that cannot currently reach the internet
// (e.g. PIA regions whose gateways are unreachable). It runs one immediate pass,
// then repeats every interval. Non-blocking — startup is not delayed; endpoints
// begin optimistically Active and converge after the first pass (~15–35s).
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
// Each goroutine first sleeps a small random jitter (0–15 s) so the actual
// network calls are spread across the cycle window rather than all firing at
// once. The sleep comes before the semaphore so it does not hold a slot.
func (p *VPNPool) runHealthCheck() {
	const maxConcurrent = 10
	sem := make(chan struct{}, maxConcurrent)

	var wg sync.WaitGroup
	for _, ep := range p.endpoints {
		wg.Add(1)
		go func(ep *VPNEndpoint) {
			defer wg.Done()
			// Jitter spread across 0–15 s to avoid a synchronised wave.
			time.Sleep(time.Duration(rand.Intn(15)) * time.Second)
			sem <- struct{}{}
			defer func() { <-sem }()
			ep.check()
		}(ep)
	}
	wg.Wait()

	active := 0
	exitIPs := make(map[string]bool)
	for _, ep := range p.endpoints {
		ep.mu.RLock()
		if ep.Active {
			active++
			if ep.exitIP != "" {
				exitIPs[ep.exitIP] = true
			}
		}
		ep.mu.RUnlock()
	}
	log.Printf("[health] %d/%d endpoints active, %d distinct exit IPs", active, len(p.endpoints), len(exitIPs))
}

type ProxyServer struct {
	vpnPool *VPNPool
}

func NewProxyServer() (*ProxyServer, error) {
	pool, err := NewVPNPool()
	if err != nil {
		return nil, err
	}
	return &ProxyServer{vpnPool: pool}, nil
}

// maxBufferedBody caps the request body held in memory so the request can be
// replayed on another tunnel. Nginx already rejects bodies over 1 MiB on the
// public path.
const maxBufferedBody = 8 << 20

// readBody buffers the request body so a retry on another tunnel can send it
// again. It writes the error response itself when it returns false.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBufferedBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Failed to read request body: "+err.Error(), http.StatusBadRequest)
		}
		return nil, false
	}
	return body, true
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
	if strategy == strategySpecific {
		if vpnName == "" {
			http.Error(w, "Missing 'vpn' parameter for specific strategy", http.StatusBadRequest)
			return
		}
		endpoint = s.vpnPool.GetEndpointByName(vpnName)
		if endpoint == nil {
			http.Error(w, "VPN endpoint not found: "+vpnName, http.StatusBadRequest)
			return
		}
	} else {
		ep, ok := s.vpnPool.pick(strategy, nil)
		if !ok {
			http.Error(w, "No VPN endpoints available", http.StatusServiceUnavailable)
			return
		}
		endpoint = ep
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}

	target := parsedURL.String()
	resp, used, release, err := s.vpnPool.forward(endpoint, strategy, func() (*http.Request, error) {
		proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
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
		return proxyReq, nil
	})
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "Failed to execute request: "+err.Error(), http.StatusBadGateway)
		}
		return
	}
	defer release()
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Add custom header to indicate which VPN was used
	w.Header().Set("X-VPN-Used", used.Name)

	// Copy status code
	w.WriteHeader(resp.StatusCode)

	// Copy response body
	io.Copy(w, resp.Body)
}

func (s *ProxyServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	endpoints := s.vpnPool.ListEndpoints()

	type endpointStatus struct {
		Name        string `json:"name"`
		Active      bool   `json:"active"`
		ExitIP      string `json:"exit_ip,omitempty"`
		CheckedAt   string `json:"checked_at,omitempty"`
		Requests    int64  `json:"requests"`
		OK          int64  `json:"ok"`
		RateLimited int64  `json:"rate_limited"`
		Forbidden   int64  `json:"forbidden"`
		Upstream5xx int64  `json:"upstream_5xx"`
		Errors      int64  `json:"errors"`
	}
	type statusResponse struct {
		Endpoints []endpointStatus `json:"endpoints"`
	}

	resp := statusResponse{}
	for _, ep := range endpoints {
		status := endpointStatus{
			Name:        ep.Name,
			Requests:    ep.stats.requests.Load(),
			OK:          ep.stats.ok.Load(),
			RateLimited: ep.stats.rateLimited.Load(),
			Forbidden:   ep.stats.forbidden.Load(),
			Upstream5xx: ep.stats.upstream5xx.Load(),
			Errors:      ep.stats.errors.Load(),
		}
		ep.mu.RLock()
		status.Active = ep.Active
		status.ExitIP = ep.exitIP
		if !ep.checkedAt.IsZero() {
			status.CheckedAt = ep.checkedAt.UTC().Format(time.RFC3339)
		}
		ep.mu.RUnlock()
		resp.Endpoints = append(resp.Endpoints, status)
	}

	json.NewEncoder(w).Encode(resp)
}

// handleRoot answers "/" with a short plain-text line, so uptime checks get a
// 200, and every other unknown path with 404.
func (s *ProxyServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "VPN farm proxy. Authenticated endpoints: /proxy?url=<target>, /status.")
}

func main() {
	server, err := NewProxyServer()
	if err != nil {
		log.Fatalf("FATAL endpoints: %v", err)
	}

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
