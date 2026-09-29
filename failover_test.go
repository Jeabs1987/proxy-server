package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testEndpoint(name, proxyURL string) *VPNEndpoint {
	u, _ := url.Parse(proxyURL)
	return &VPNEndpoint{Name: name, ProxyURL: proxyURL, display: u.Redacted(), Active: true, client: newEndpointClient(u)}
}

// deadProxyURL returns a proxy address where nothing listens.
func deadProxyURL(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

// countingProxy stands in for a working tunnel: it answers every proxied
// request itself and counts what reached it.
func countingProxy(t *testing.T, hits *atomic.Int32) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("target got body %q, want the replayed payload", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	return server
}

func newPost() (*http.Request, error) {
	return http.NewRequest(http.MethodPost, "http://example.test/redeem", strings.NewReader("payload"))
}

func TestForwardRetriesTunnelThatNeverConnected(t *testing.T) {
	var hits atomic.Int32
	dead := testEndpoint("dead", deadProxyURL(t))
	good := testEndpoint("good", countingProxy(t, &hits).URL)
	pool := &VPNPool{endpoints: []*VPNEndpoint{dead, good}}

	resp, used, release, err := pool.forward(dead, strategyRoundRobin, newPost)
	if err != nil {
		t.Fatalf("forward failed although a live tunnel was available: %v", err)
	}
	defer release()
	resp.Body.Close()

	if used != good || resp.StatusCode != http.StatusCreated || hits.Load() != 1 {
		t.Errorf("used %s, status %d, target hits %d", used.Name, resp.StatusCode, hits.Load())
	}
	if dead.stats.errors.Load() != 1 || good.stats.ok.Load() != 1 {
		t.Errorf("stats: dead errors %d, good ok %d", dead.stats.errors.Load(), good.stats.ok.Load())
	}
}

func TestForwardDoesNotRetryAfterTargetMayHaveSeenRequest(t *testing.T) {
	var hits atomic.Int32
	// This tunnel connects, takes the request, and drops the connection, so the
	// target may already have acted on it. A retry could redeem twice.
	dropping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer dropping.Close()
	first := testEndpoint("dropping", dropping.URL)
	good := testEndpoint("good", countingProxy(t, &hits).URL)
	pool := &VPNPool{endpoints: []*VPNEndpoint{first, good}}

	if _, _, _, err := pool.forward(first, strategyRoundRobin, newPost); err == nil {
		t.Fatal("forward reported success for a dropped request")
	}
	if hits.Load() != 0 {
		t.Errorf("request was replayed on another tunnel %d times", hits.Load())
	}
}

func TestForwardNeverRetriesSpecificTunnel(t *testing.T) {
	var hits atomic.Int32
	dead := testEndpoint("dead", deadProxyURL(t))
	good := testEndpoint("good", countingProxy(t, &hits).URL)
	pool := &VPNPool{endpoints: []*VPNEndpoint{dead, good}}

	if _, _, _, err := pool.forward(dead, strategySpecific, newPost); err == nil {
		t.Fatal("forward reported success through a dead tunnel")
	}
	if hits.Load() != 0 {
		t.Error("a request for a named tunnel went through a different one")
	}
}

func TestDoFailsFastOnHungTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for connectBudget")
	}
	// Accepts the connection and the CONNECT, then never answers: what a
	// gluetun proxy does while its VPN tunnel is down.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var held []net.Conn
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()

	ep := testEndpoint("hung", "http://"+l.Addr().String())
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/redeem", strings.NewReader("payload"))
	start := time.Now()
	_, sent, _, err := ep.do(req, connectBudget)
	if err == nil || sent {
		t.Fatalf("hung tunnel: err %v, sent %v; want an unsent failure", err, sent)
	}
	if !strings.Contains(err.Error(), "did not connect") {
		t.Errorf("error does not name the connect timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed > connectBudget+2*time.Second {
		t.Errorf("gave up after %s, want about %s", elapsed, connectBudget)
	}
}
