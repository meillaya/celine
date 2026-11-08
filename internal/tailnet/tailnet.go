// Package tailnet drives Tailscale Serve for the declared endpoints.
package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Meillaya/celine/internal/execx"
	"github.com/Meillaya/celine/internal/render"
)

// Self is the subset of `tailscale status --json` we rely on.
type Self struct {
	BackendState string   `json:"BackendState"`
	DNSName      string   `json:"DNSName"`
	CertDomains  []string `json:"CertDomains"`
}

// ReadSelf reports the node identity and certificate domains.
func ReadSelf(ctx context.Context, runner execx.Runner, ts string) (Self, error) {
	result, err := runner.Run(ctx, ts, []string{"status", "--json"}, execx.Options{})
	if err != nil {
		return Self{}, fmt.Errorf("tailscale status: %w", err)
	}
	var envelope struct {
		BackendState string   `json:"BackendState"`
		CertDomains  []string `json:"CertDomains"`
		Self         struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(result.Stdout, &envelope); err != nil {
		return Self{}, fmt.Errorf("decode tailscale status: %w", err)
	}
	return Self{
		BackendState: envelope.BackendState,
		DNSName:      strings.TrimSuffix(envelope.Self.DNSName, "."),
		CertDomains:  envelope.CertDomains,
	}, nil
}

// Hostname returns the configured hostname or the MagicDNS name.
func Hostname(configured, dnsName string) string {
	if configured != "" {
		return configured
	}
	return strings.TrimSuffix(dnsName, ".")
}

// CertDomainReady reports whether the node has an HTTPS certificate domain for
// hostname.
func (s Self) CertDomainReady(hostname string) bool {
	for _, domain := range s.CertDomains {
		if strings.TrimSuffix(domain, ".") == hostname {
			return true
		}
	}
	return false
}

// ServeStatus returns the raw serve configuration.
func ServeStatus(ctx context.Context, runner execx.Runner, ts string) ([]byte, error) {
	result, err := runner.Run(ctx, ts, []string{"serve", "status", "--json"}, execx.Options{})
	if err != nil {
		return nil, fmt.Errorf("tailscale serve status: %w", err)
	}
	return result.Stdout, nil
}

// Apply writes the serve handlers. Tailscale's CLI is idempotent per port.
func Apply(ctx context.Context, runner execx.Runner, ts string, endpoints []render.Endpoint) error {
	for _, endpoint := range endpoints {
		args := []string{
			"serve", "--bg", "--yes",
			fmt.Sprintf("--https=%d", endpoint.HTTPSPort),
			fmt.Sprintf("http://127.0.0.1:%d", endpoint.UpstreamPort),
		}
		if _, err := runner.Run(ctx, ts, args, execx.Options{}); err != nil {
			return fmt.Errorf("serve %s on %d: %w", endpoint.Service, endpoint.HTTPSPort, err)
		}
	}
	return nil
}

// Clear removes the handlers celine owns. It avoids `tailscale serve reset` so
// unrelated services on the node stay untouched.
func Clear(ctx context.Context, runner execx.Runner, ts string, endpoints []render.Endpoint) error {
	for _, endpoint := range endpoints {
		args := []string{"serve", fmt.Sprintf("--https=%d", endpoint.HTTPSPort), "off"}
		if _, err := runner.Run(ctx, ts, args, execx.Options{}); err != nil {
			return fmt.Errorf("clear serve port %d: %w\nhint: `tailscale serve reset` removes every handler on this node", endpoint.HTTPSPort, err)
		}
	}
	return nil
}

// StatusSummary is a value-free description for doctor and status output.
type StatusSummary struct {
	BackendState string   `json:"backend_state"`
	DNSName      string   `json:"dns_name"`
	CertDomains  []string `json:"cert_domains"`
	Serve        string   `json:"serve"`
}

// ReadStatus gathers the tailnet state.
func ReadStatus(ctx context.Context, runner execx.Runner, ts string) (StatusSummary, error) {
	self, err := ReadSelf(ctx, runner, ts)
	if err != nil {
		return StatusSummary{}, err
	}
	raw, err := ServeStatus(ctx, runner, ts)
	if err != nil {
		return StatusSummary{}, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" {
		trimmed = "no serve config"
	}
	return StatusSummary{
		BackendState: self.BackendState,
		DNSName:      self.DNSName,
		CertDomains:  self.CertDomains,
		Serve:        trimmed,
	}, nil
}
