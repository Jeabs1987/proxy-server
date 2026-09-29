package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
)

// defaultEndpoints is the tunnel list built into the binary. Every local
// entry must match a docker-compose.yml service 1:1 (endpoints_test.go checks
// this), so a deploy can never route to a port the farm does not publish.
//
//go:embed endpoints.json
var defaultEndpoints []byte

type endpointConfig struct {
	Name     string `json:"name"`
	ProxyURL string `json:"proxy_url"`
}

// loadEndpoints reads the tunnel list. ENDPOINTS_FILE replaces the built-in
// copy. ${VAR} references in proxy_url expand from the environment, so the
// credentials of a remote farm stay out of the repo.
func loadEndpoints() ([]*VPNEndpoint, error) {
	data, source := defaultEndpoints, "built-in endpoints.json"
	if path := os.Getenv("ENDPOINTS_FILE"); path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
		source = path
	}

	var configs []endpointConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&configs); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("%s: no endpoints", source)
	}

	names := make(map[string]bool)
	urls := make(map[string]bool)
	endpoints := make([]*VPNEndpoint, 0, len(configs))
	for i, c := range configs {
		proxyURL := os.ExpandEnv(c.ProxyURL)
		parsed, err := url.Parse(proxyURL)
		switch {
		case c.Name == "":
			return nil, fmt.Errorf("%s: entry %d has no name", source, i)
		case err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "":
			return nil, fmt.Errorf("%s: %q has an invalid proxy_url", source, c.Name)
		case names[c.Name]:
			return nil, fmt.Errorf("%s: duplicate name %q", source, c.Name)
		case urls[parsed.Host]:
			return nil, fmt.Errorf("%s: %q reuses proxy %s", source, c.Name, parsed.Host)
		}
		names[c.Name] = true
		urls[parsed.Host] = true
		endpoints = append(endpoints, &VPNEndpoint{
			Name:     c.Name,
			ProxyURL: proxyURL,
			display:  parsed.Redacted(),
			Active:   true,
			client:   newEndpointClient(parsed),
		})
	}
	return endpoints, nil
}
