package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Meillaya/celine/internal/execx"
)

func TestEnvironmentOverrideSkipsSecretSpec(t *testing.T) {
	runner := &execx.Fake{}
	resolver := New(runner, "/repo/secretspec.toml")
	resolver.LookupEnv = func(name string) (string, bool) {
		if name == NameKavitaTokenKey {
			return "env-token", true
		}
		return "", false
	}
	values, err := resolver.Resolve(context.Background(), NameKavitaTokenKey)
	if err != nil {
		t.Fatal(err)
	}
	if values[NameKavitaTokenKey] != "env-token" {
		t.Fatalf("value = %q", values[NameKavitaTokenKey])
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("expected no secretspec call, got %v", runner.CommandLines())
	}
}

func TestResolveFromSecretSpec(t *testing.T) {
	runner := &execx.Fake{Func: func(call execx.Call) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`{"KAVITA_TOKEN_KEY":"abc"}`)}, nil
	}}
	resolver := New(runner, "/repo/secretspec.toml")
	resolver.LookupEnv = func(string) (string, bool) { return "", false }
	resolver.Version = "1.2.3"
	values, err := resolver.Resolve(context.Background(), NameKavitaTokenKey)
	if err != nil {
		t.Fatal(err)
	}
	if values[NameKavitaTokenKey] != "abc" {
		t.Fatalf("value = %q", values[NameKavitaTokenKey])
	}
	command := runner.CommandLines()[0]
	for _, want := range []string{"--file /repo/secretspec.toml", "export --format json", "--caller celine", "--caller-version 1.2.3"} {
		if !strings.Contains(command, want) {
			t.Errorf("command missing %q: %s", want, command)
		}
	}
}

func TestResolveRejectsMissingSecret(t *testing.T) {
	runner := &execx.Fake{Func: func(call execx.Call) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`{}`)}, nil
	}}
	resolver := New(runner, "/repo/secretspec.toml")
	resolver.LookupEnv = func(string) (string, bool) { return "", false }
	_, err := resolver.Resolve(context.Background(), NameKavitaTokenKey)
	if err == nil || !strings.Contains(err.Error(), "did not return required secret") {
		t.Fatalf("expected missing-secret error, got %v", err)
	}
}

func TestMaterializeWrites0600(t *testing.T) {
	runner := &execx.Fake{}
	resolver := New(runner, "/repo/secretspec.toml")
	resolver.LookupEnv = func(name string) (string, bool) {
		if name == NameKavitaTokenKey {
			return "materialized", true
		}
		return "", false
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "token")
	if err := resolver.Materialize(context.Background(), NameKavitaTokenKey, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "materialized" {
		t.Fatalf("content = %q", data)
	}
}
