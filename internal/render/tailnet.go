package render

import (
	"fmt"

	"github.com/Meillaya/celine/internal/config"
)

// Endpoint is one Tailscale Serve exposure.
type Endpoint struct {
	Service      string
	Hostname     string
	HTTPSPort    int
	UpstreamPort int
}

// URL returns the public base URL for the endpoint.
func (e Endpoint) URL() string {
	if e.HTTPSPort == 443 {
		return "https://" + e.Hostname
	}
	return fmt.Sprintf("https://%s:%d", e.Hostname, e.HTTPSPort)
}

// Endpoints expands the configured serve targets. hostname may be empty.
func Endpoints(cfg *config.Config, hostname string) ([]Endpoint, error) {
	if !cfg.Tailnet.Enable {
		return nil, nil
	}
	if hostname == "" {
		hostname = cfg.Tailnet.Hostname
	}
	endpoints := make([]Endpoint, 0, len(cfg.Tailnet.Serve))
	for _, target := range cfg.Tailnet.Serve {
		if !cfg.ServiceEnabled(target.Service) {
			continue
		}
		upstream, err := cfg.UpstreamPort(target.Service)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, Endpoint{
			Service:      target.Service,
			Hostname:     hostname,
			HTTPSPort:    target.HTTPSPort,
			UpstreamPort: upstream,
		})
	}
	return endpoints, nil
}
