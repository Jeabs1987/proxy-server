package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyRejectsInternalAndSpecialDestinations(t *testing.T) {
	for _, host := range []string{"127.0.0.1:80", "0.0.0.0:80", "0.1.2.3:80", "100.64.0.1:80", "169.254.169.254:80", "224.0.0.1:80", "[::]:80", "[::1]:80", "[ff02::1]:80", "[::ffff:127.0.0.1]:80"} {
		if !isPrivateIP(host) {
			t.Errorf("proxy permits restricted destination %s", host)
		}
	}
}

func TestForwardProxyRejectsUnauthenticatedConnect(t *testing.T) {
	for _, header := range []string{"", "Basic invalid", "Basic dXNlcjp3cm9uZw=="} {
		request := httptest.NewRequest(http.MethodConnect, "http://example.com:443", nil)
		request.Header.Set("Proxy-Authorization", header)
		response := httptest.NewRecorder()
		// A nil pool proves unauthenticated traffic cannot reach tunnel selection.
		(&forwardProxy{key: "test-only-secret"}).ServeHTTP(response, request)
		if response.Code != http.StatusProxyAuthRequired {
			t.Errorf("CONNECT with invalid credentials returned %d", response.Code)
		}
	}
}
