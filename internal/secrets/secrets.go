// Package secrets resolves the values declared in secretspec.toml through the
// SecretSpec CLI. Values are never logged and are materialized only where an
// application needs them.
package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Meillaya/celine/internal/execx"
)

// NameKavitaTokenKey is the SecretSpec key for Kavita's JWT signing key.
const NameKavitaTokenKey = "KAVITA_TOKEN_KEY"

// Resolver invokes secretspec.
type Resolver struct {
	Runner     execx.Runner
	Manifest   string
	Profile    string
	Provider   string
	Reason     string
	Caller     string
	Version    string
	LookupEnv  func(string) (string, bool)
	Secretspec string
}

// New builds a resolver with the defaults used by the CLI.
func New(runner execx.Runner, manifest string) *Resolver {
	return &Resolver{
		Runner:     runner,
		Manifest:   manifest,
		Profile:    "default",
		Caller:     "celine",
		Secretspec: "secretspec",
		LookupEnv:  os.LookupEnv,
	}
}

// Resolve returns the requested values. Environment overrides are accepted for
// tests and CI so the unit tests never touch a provider.
func (r *Resolver) Resolve(ctx context.Context, names ...string) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}

	fromEnv := map[string]string{}
	missing := make([]string, 0, len(names))
	for _, name := range names {
		if r.LookupEnv != nil {
			if value, ok := r.LookupEnv(name); ok && value != "" {
				fromEnv[name] = value
				continue
			}
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return fromEnv, nil
	}

	if r.Runner == nil {
		return nil, fmt.Errorf("no command runner configured for SecretSpec")
	}

	args := []string{"--file", r.Manifest, "export", "--format", "json"}
	if r.Profile != "" {
		args = append(args, "--profile", r.Profile)
	}
	if r.Provider != "" {
		args = append(args, "--provider", r.Provider)
	}
	args = append(args, "--reason", r.reason())
	if r.Caller != "" {
		args = append(args, "--caller", r.Caller)
	}
	if r.Version != "" {
		args = append(args, "--caller-version", r.Version)
	}

	result, err := r.Runner.Run(ctx, r.Secretspec, args, execx.Options{})
	if err != nil {
		return nil, fmt.Errorf("resolve secrets with SecretSpec: %w\nhint: run `secretspec check` for this project, then retry", err)
	}

	values := map[string]string{}
	if err := json.Unmarshal(result.Stdout, &values); err != nil {
		return nil, fmt.Errorf("SecretSpec returned malformed JSON: %w", err)
	}
	for name, value := range fromEnv {
		values[name] = value
	}
	var absent []string
	for _, name := range names {
		if values[name] == "" {
			absent = append(absent, name)
		}
	}
	if len(absent) > 0 {
		return nil, fmt.Errorf("SecretSpec did not return required secret(s): %s", strings.Join(absent, ", "))
	}
	return values, nil
}

// Check runs the value-free SecretSpec check and returns its report.
func (r *Resolver) Check(ctx context.Context) ([]byte, error) {
	args := []string{"--file", r.Manifest, "check", "--no-prompt", "--json", "--reason", r.reason()}
	if r.Profile != "" {
		args = append(args, "--profile", r.Profile)
	}
	if r.Provider != "" {
		args = append(args, "--provider", r.Provider)
	}
	result, err := r.Runner.Run(ctx, r.Secretspec, args, execx.Options{})
	if err != nil {
		return result.Stdout, fmt.Errorf("SecretSpec check failed: %w", err)
	}
	return result.Stdout, nil
}

// Materialize writes one secret to path with mode 0600, atomically.
func (r *Resolver) Materialize(ctx context.Context, name, path string) error {
	values, err := r.Resolve(ctx, name)
	if err != nil {
		return err
	}
	value := values[name]
	if value == "" {
		return fmt.Errorf("secret %s resolved to an empty value", name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create secret directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".secret-*")
	if err != nil {
		return fmt.Errorf("create temporary secret file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set secret file mode: %w", err)
	}
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close()
		return fmt.Errorf("write secret: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync secret: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close secret: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install secret file: %w", err)
	}
	return nil
}

func (r *Resolver) reason() string {
	if r.Reason != "" {
		return r.Reason
	}
	return "celine deployment"
}
