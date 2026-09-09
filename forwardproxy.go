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
	endpoint, ok := f.authorize(w, r)
	if !ok {
		return
	}

	if r.Method == http.MethodConnect {
		f.handleConnect(w, r, endpoint)
		return
	}
	f.handleAbsolute(w, r, endpoint)
}

// authorize checks the proxy credentials and resolves the username into the
// tunnel the caller asked for. It writes the error response itself.
func (f *forwardProxy) authorize(w http.ResponseWriter, r *http.Request) (*VPNEndpoint, bool) {
	selector, key, ok := parseProxyAuth(r.Header.Get("Proxy-Authorization"))
	if !ok || subtle.ConstantTimeCompare([]byte(key), []byte(f.key)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="vpn-farm"`)
		http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
		return nil, false
	}

	switch strings.ToLower(selector) {
	case "", "roundrobin":
		endpoint, ok := f.pool.GetNextEndpoint()
		if !ok {
			http.Error(w, "No VPN endpoints available", http.StatusServiceUnavailable)
			return nil, false
		}
		return endpoint, true
	case "random":
		endpoint, ok := f.pool.GetRandomEndpoint()
		if !ok {
			http.Error(w, "No VPN endpoints available", http.StatusServiceUnavailable)
			return nil, false
		}
		return endpoint, true
	default:
		endpoint := f.pool.GetEndpointByName(selector)
		if endpoint == nil {
			http.Error(w, "VPN endpoint not found: "+selector, http.StatusBadGateway)
			return nil, false
		}
		return endpoint, true
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
func (f *forwardProxy) handleConnect(w http.ResponseWriter, r *http.Request, endpoint *VPNEndpoint) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	if isPrivateIP(target) {
		http.Error(w, "Requests to private/internal networks are not allowed", http.StatusForbidden)
		return
	}

	proxyURL, err := url.Parse(endpoint.ProxyURL)
	if err != nil {
		http.Error(w, "Bad endpoint configuration", http.StatusInternalServerError)
		return
	}

	upstream, err := net.DialTimeout("tcp", proxyURL.Host, 10*time.Second)
	if err != nil {
		log.Printf("WARN forward-proxy: dial %s (%s): %v", endpoint.Name, proxyURL.Host, err)
		http.Error(w, "Failed to reach VPN endpoint: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	if _, err := fmt.Fprintf(upstream, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		http.Error(w, "Failed to open tunnel: "+err.Error(), http.StatusBadGateway)
		return
	}

	upstreamReader := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamReader, r)
	if err != nil {
		http.Error(w, "Failed to open tunnel: "+err.Error(), http.StatusBadGateway)
		return
	}
	// resp.Body is deliberately left alone. The connection is the tunnel, not a
	// message body: closing it makes net/http drain the socket to EOF, which
	// tears down the tunnel before the caller sends its first byte.
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "VPN endpoint refused CONNECT: "+resp.Status, http.StatusBadGateway)
		return
	}

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
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, clientBuf); done <- struct{}{} }()
	go func() { io.Copy(client, upstreamReader); done <- struct{}{} }()
	<-done // the deferred closes unblock the other direction
}

// handleAbsolute serves a plain http:// request, which proxy clients send with
// the full URL in the request line instead of a CONNECT.
func (f *forwardProxy) handleAbsolute(w http.ResponseWriter, r *http.Request, endpoint *VPNEndpoint) {
	if !r.URL.IsAbs() {
		http.Error(w, "This port is a forward proxy; send an absolute URL or CONNECT", http.StatusBadRequest)
		return
	}

	if isPrivateIP(r.URL.Host) {
		http.Error(w, "Requests to private/internal networks are not allowed", http.StatusForbidden)
		return
	}

	proxyReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "Failed to create request: "+err.Error(), http.StatusInternalServerError)
		return
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

	resp, err := endpoint.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "Failed to execute request: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		if hopByHopHeaders[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-VPN-Used", endpoint.Name)
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
