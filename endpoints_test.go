package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEndpointsMatchCompose replaces the old grep check in CLAUDE.md: every
// local endpoint must have exactly one compose service with the same region
// and port, and every service must have an endpoint.
func TestEndpointsMatchCompose(t *testing.T) {
	endpoints, err := loadEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	fromEndpoints := make(map[string]string) // port -> name
	for _, ep := range endpoints {
		u, _ := url.Parse(ep.ProxyURL)
		if u.Hostname() == "127.0.0.1" {
			fromEndpoints[u.Port()] = ep.Name
		}
	}

	compose, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	service := regexp.MustCompile(`^  (vpn-[\w-]+):\s*$`)
	region := regexp.MustCompile(`^\s+SERVER_REGIONS: (.+)$`)
	port := regexp.MustCompile(`^\s+- "127\.0\.0\.1:(\d+):8888"$`)
	address := regexp.MustCompile(`^\s+ipv4_address: (\S+)$`)

	fromCompose := make(map[string]string) // port -> region
	addresses := make(map[string]string)   // static IP -> service
	var name, reg string
	for _, line := range strings.Split(string(compose), "\n") {
		if m := service.FindStringSubmatch(line); m != nil {
			name, reg = m[1], ""
		} else if m := region.FindStringSubmatch(line); m != nil {
			reg = m[1]
		} else if m := address.FindStringSubmatch(line); m != nil {
			if other, dup := addresses[m[1]]; dup {
				t.Errorf("%s and %s share static IP %s", other, name, m[1])
			}
			addresses[m[1]] = name
		} else if m := port.FindStringSubmatch(line); m != nil {
			if _, dup := fromCompose[m[1]]; dup {
				t.Errorf("port %s is published twice", m[1])
			}
			fromCompose[m[1]] = reg
		}
	}

	if len(fromCompose) == 0 {
		t.Fatal("found no 127.0.0.1:NNNN:8888 ports in docker-compose.yml")
	}
	for p, r := range fromCompose {
		if got, ok := fromEndpoints[p]; !ok {
			t.Errorf("compose publishes %s (%s) but endpoints.json has no endpoint for it", p, r)
		} else if got != r {
			t.Errorf("port %s: endpoints.json name %q, compose SERVER_REGIONS %q", p, got, r)
		}
	}
	for p, n := range fromEndpoints {
		if _, ok := fromCompose[p]; !ok {
			t.Errorf("endpoints.json routes %q to port %s, which compose does not publish", n, p)
		}
	}
}

func TestLoadEndpointsFile(t *testing.T) {
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "endpoints.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Setenv("FARM2_AUTH", "user:secret")
	t.Setenv("ENDPOINTS_FILE", write(`[{"name": "Remote", "proxy_url": "http://${FARM2_AUTH}@10.66.0.2:8881"}]`))
	endpoints, err := loadEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	if got := endpoints[0].ProxyURL; got != "http://user:secret@10.66.0.2:8881" {
		t.Errorf("proxy_url did not expand: %s", got)
	}
	if strings.Contains(endpoints[0].display, "secret") {
		t.Errorf("log form leaks the password: %s", endpoints[0].display)
	}

	for _, bad := range []string{
		`[]`,
		`[{"name": "", "proxy_url": "http://127.0.0.1:1"}]`,
		`[{"name": "A", "proxy_url": "socks5://127.0.0.1:1"}]`,
		`[{"name": "A", "proxy_url": "http://127.0.0.1:1"}, {"name": "A", "proxy_url": "http://127.0.0.1:2"}]`,
		`[{"name": "A", "proxy_url": "http://127.0.0.1:1"}, {"name": "B", "proxy_url": "http://127.0.0.1:1"}]`,
		`[{"name": "A", "proxy_url": "http://127.0.0.1:1", "typo": true}]`,
	} {
		t.Setenv("ENDPOINTS_FILE", write(bad))
		if _, err := loadEndpoints(); err == nil {
			t.Errorf("accepted invalid endpoints file %s", bad)
		}
	}
}

func TestParseExitIP(t *testing.T) {
	for body, want := range map[string]string{
		"203.0.113.7": "203.0.113.7",
		"fl=12f\nh=1.1.1.1\nip=198.51.100.4\nts=1": "198.51.100.4",
		"2001:db8::1\n":        "2001:db8::1",
		"<html>blocked</html>": "",
	} {
		if got := parseExitIP(body); got != want {
			t.Errorf("parseExitIP(%q) = %q, want %q", body, got, want)
		}
	}
}
