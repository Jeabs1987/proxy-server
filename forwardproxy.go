package main

// Forward-proxy mode. The /proxy?url= API only works for callers willing to
// rewrite every request into a query parameter; this listener speaks the
// ordinary HTTP proxy protocol instead, so `curl -x`, HTTPS_PROXY=... and any
// library that honours proxy settings can egress through the VPN farm.
//
// Auth is Proxy-Authorization: Basic <selector>:<API_KEY>, where the username
// selects the tunnel:
//
//	""  / "roundrobin"  rotate through active tunnels (default)
//	"random"            pick an active tunnel at random
//	"US Texas"          that tunnel by name (names come from /status)

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hopByHopHeaders are connection-scoped and must not be passed on to the target.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

type forwardProxy struct {
	pool *VPNPool
	key  string
}

func (f *forwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	endpoint, strategy, ok := f.authorize(w, r)
	if !ok {
		return
	}

	if r.Method == http.MethodConnect {
		f.handleConnect(w, r, endpoint, strategy)
		return
	}
	f.handleAbsolute(w, r, endpoint, strategy)
}

// authorize checks the proxy credentials and resolves the username into the
// tunnel the caller asked for and the strategy that picked it. It writes the
// error response itself.
func (f *forwardProxy) authorize(w http.ResponseWriter, r *http.Request) (*VPNEndpoint, string, bool) {
	selector, key, ok := parseProxyAuth(r.Header.Get("Proxy-Authorization"))
	if !ok || subtle.ConstantTimeCompare([]byte(key), []byte(f.key)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="vpn-farm"`)
		http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
		return nil, "", false
	}

	switch strategy := strings.ToLower(selector); strategy {
	case "", strategyRoundRobin, strategyRandom:
		endpoint, ok := f.pool.pick(strategy, nil)
		if !ok {
			http.Error(w, "No VPN endpoints available", http.StatusServiceUnavailable)
			return nil, "", false
		}
		return endpoint, strategy, true
	default:
		endpoint := f.pool.GetEndpointByName(selector)
		if endpoint == nil {
			http.Error(w, "VPN endpoint not found: "+selector, http.StatusBadGateway)
			return nil, "", false
		}
		return endpoint, strategySpecific, true
	}
}

// parseProxyAuth splits a Basic credential into its username and password.
func parseProxyAuth(header string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(decoded), ":")
	return user, pass, ok
}

// handleConnect tunnels raw bytes to the target through the tunnel's HTTP
// proxy, which is what every https:// request through a proxy uses.
func (f *forwardProxy) handleConnect(w http.ResponseWriter, r *http.Request, endpoint *VPNEndpoint, strategy string) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	if isPrivateIP(target) {
		http.Error(w, "Requests to private/internal networks are not allowed", http.StatusForbidden)
		return
	}

	// A failed CONNECT sends nothing to the target, so another tunnel can take
	// over unless the caller named this one.
	tried := make(map[*VPNEndpoint]bool)
	var upstream net.Conn
	var upstreamReader *bufio.Reader
	for attempt := 1; ; attempt++ {
		tried[endpoint] = true
		retryable := strategy != strategySpecific && attempt < maxAttempts && r.Context().Err() == nil
		budget := lastAttemptBudget
		if retryable {
			budget = connectBudget
		}
		var err error
		upstream, upstreamReader, err = openTunnel(endpoint, target, r, budget)
		if err == nil {
			endpoint.stats.record(http.StatusOK)
			break
		}
		endpoint.stats.recordError()
		endpoint.recheck()
		log.Printf("WARN forward-proxy: %s: %v", endpoint.Name, err)

		var next *VPNEndpoint
		if retryable {
			next, _ = f.pool.pick(strategy, tried)
		}
		if next == nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		endpoint = next
	}
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT not supported on this listener", http.StatusInternalServerError)
		return
	}
	client, clientBuf, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "Failed to hijack connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()
	client.SetDeadline(time.Time{})

	if _, err := fmt.Fprintf(client, "HTTP/1.1 200 Connection Established\r\nX-VPN-Used: %s\r\n\r\n", endpoint.Name); err != nil {
		return
	}

	// Both buffered readers may already hold bytes read off the socket, so copy
	// from them rather than from the raw connections.
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, activityReader{clientBuf, &lastActive}); done <- struct{}{} }()
	go func() { io.Copy(client, activityReader{upstreamReader, &lastActive}); done <- struct{}{} }()

	idle := time.NewTicker(tunnelIdleTimeout / 5)
	defer idle.Stop()
	for {
		select {
		case <-done:
			return // the deferred closes unblock the other direction
		case <-idle.C:
			if time.Since(time.Unix(0, lastActive.Load())) > tunnelIdleTimeout {
				return
			}
		}
	}
}

// lastAttemptBudget bounds a CONNECT that no other tunnel can take over. It
// matches the endpoint client timeout.
const lastAttemptBudget = 30 * time.Second

// openTunnel asks the endpoint's HTTP proxy for a CONNECT tunnel to target.
// The whole exchange must finish within budget, so a hung VPN tunnel fails
// and another can take over. Bytes the proxy sent after its response stay in
// the reader.
func openTunnel(endpoint *VPNEndpoint, target string, r *http.Request, budget time.Duration) (net.Conn, *bufio.Reader, error) {
	proxyURL, err := url.Parse(endpoint.ProxyURL)
	if err != nil {
		return nil, nil, fmt.Errorf("bad endpoint configuration: %v", err)
	}

	upstream, err := net.DialTimeout("tcp", proxyURL.Host, budget)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reach VPN endpoint %s: %v", proxyURL.Host, err)
	}
	upstream.SetDeadline(time.Now().Add(budget))

	auth := ""
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		auth = "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	if _, err := fmt.Fprintf(upstream, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, auth); err != nil {
		upstream.Close()
		return nil, nil, fmt.Errorf("failed to open tunnel: %v", err)
	}

	reader := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(reader, r)
	if err != nil {
		upstream.Close()
		return nil, nil, fmt.Errorf("failed to open tunnel: %v", err)
	}
	// resp.Body is deliberately left alone. The connection is the tunnel, not a
	// message body: closing it makes net/http drain the socket to EOF, which
	// tears down the tunnel before the caller sends its first byte.
	if resp.StatusCode != http.StatusOK {
		upstream.Close()
		return nil, nil, fmt.Errorf("VPN endpoint refused CONNECT: %s", resp.Status)
	}
	upstream.SetDeadline(time.Time{})
	return upstream, reader, nil
}

// tunnelIdleTimeout closes a CONNECT tunnel after this long with no bytes in
// either direction, so an abandoned tunnel does not hold its sockets forever.
const tunnelIdleTimeout = 5 * time.Minute

// activityReader records the time of every read that returns data.
type activityReader struct {
	r    io.Reader
	last *atomic.Int64
}

func (a activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// handleAbsolute serves a plain http:// request, which proxy clients send with
// the full URL in the request line instead of a CONNECT.
func (f *forwardProxy) handleAbsolute(w http.ResponseWriter, r *http.Request, endpoint *VPNEndpoint, strategy string) {
	if !r.URL.IsAbs() {
		http.Error(w, "This port is a forward proxy; send an absolute URL or CONNECT", http.StatusBadRequest)
		return
	}

	if isPrivateIP(r.URL.Host) {
		http.Error(w, "Requests to private/internal networks are not allowed", http.StatusForbidden)
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}

	target := r.URL.String()
	resp, used, release, err := f.pool.forward(endpoint, strategy, func() (*http.Request, error) {
		proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for key, values := range r.Header {
			lower := strings.ToLower(key)
			if hopByHopHeaders[lower] || sensitiveHeaders[lower] {
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

	for key, values := range resp.Header {
		if hopByHopHeaders[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-VPN-Used", used.Name)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// certCache reloads the TLS certificate off disk so Let's Encrypt renewals are
// picked up without restarting the service.
type certCache struct {
	certFile string
	keyFile  string

	mu       sync.Mutex
	cert     *tls.Certificate
	loadedAt time.Time
}

func (c *certCache) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cert != nil && time.Since(c.loadedAt) < time.Hour {
		return c.cert, nil
	}

	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		if c.cert != nil {
			log.Printf("WARN forward-proxy: certificate reload failed, serving the cached one: %v", err)
			return c.cert, nil
		}
		return nil, err
	}
	c.cert = &cert
	c.loadedAt = time.Now()
	return c.cert, nil
}

// StartForwardProxy runs the forward-proxy listener when FORWARD_PROXY_ADDR is
// set. It serves TLS when FORWARD_PROXY_CERT and FORWARD_PROXY_KEY are both
// given, and plain HTTP otherwise.
func StartForwardProxy(pool *VPNPool) {
	addr := os.Getenv("FORWARD_PROXY_ADDR")
	if addr == "" {
		return
	}

	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		log.Printf("WARN forward-proxy: API_KEY is unset, refusing to open %s as an unauthenticated proxy", addr)
		return
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           &forwardProxy{pool: pool, key: apiKey},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		// Hijacking a CONNECT needs HTTP/1.1, and a tunnel is open for as long
		// as the caller keeps it, so no read/write timeouts here.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}

	certFile, keyFile := os.Getenv("FORWARD_PROXY_CERT"), os.Getenv("FORWARD_PROXY_KEY")
	if os.Getenv("APP_ENV") == "production" && (certFile == "" || keyFile == "") {
		log.Printf("ERROR forward-proxy: refusing plaintext credential transport in production")
		return
	}
	go func() {
		if certFile != "" && keyFile != "" {
			cache := &certCache{certFile: certFile, keyFile: keyFile}
			if _, err := cache.get(nil); err != nil {
				log.Printf("ERROR forward-proxy: cannot load %s: %v", certFile, err)
				return
			}
			server.TLSConfig = &tls.Config{
				GetCertificate: cache.get,
				MinVersion:     tls.VersionTLS12,
			}
			log.Printf("Forward proxy (TLS) listening on %s", addr)
			log.Printf("ERROR forward-proxy: %v", server.ListenAndServeTLS("", ""))
			return
		}
		log.Printf("Forward proxy (plaintext) listening on %s", addr)
		log.Printf("ERROR forward-proxy: %v", server.ListenAndServe())
	}()
}
