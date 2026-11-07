package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `
[meta]
schema = 1

[paths]
media_root = "media"
state_dir = "state"
backup_dir = "backups"

[runtime]
rootless = true
timezone = "America/Toronto"

[calibre]
enable = true
version = "9.18.0"
port = 8080

[[calibre.libraries]]
name = "General"
path = "Calibre/General"

[kavita]
enable = true
image = "example/kavita:1"
port = 5000

[tailnet]
enable = true

[[tailnet.serve]]
service = "kavita"
https_port = 443

[[tailnet.serve]]
service = "calibre"
https_port = 8443

[backup]
enable = true
retention = 10
on_calendar = "daily"
`

func writeFixture(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secretspec.toml"), []byte("[project]\nname=\"x\"\nrevision=\"1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "celine.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		filepath.Join(root, "media", "Calibre", "General"),
		filepath.Join(root, "media", "Kavita"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "media", "Calibre", "General", "metadata.db"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadResolvesPaths(t *testing.T) {
	root := writeFixture(t, validConfig)
	cfg, err := Load(filepath.Join(root, "celine.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resolved.MediaRoot != filepath.Join(root, "media") {
		t.Fatalf("media root = %q", cfg.Resolved.MediaRoot)
	}
	if cfg.Resolved.KavitaConfigDir != filepath.Join(root, "state", "kavita", "config") {
		t.Fatalf("kavita config dir = %q", cfg.Resolved.KavitaConfigDir)
	}
	if cfg.Timezone() != "America/Toronto" {
		t.Fatalf("timezone = %q", cfg.Timezone())
	}
	if cfg.CalibreImage() != "celine/calibre:9.18.0" {
		t.Fatalf("image = %q", cfg.CalibreImage())
	}
	if err := cfg.Validate(ValidateOptions{CheckFilesystem: true}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestRejectsUnknownKeys(t *testing.T) {
	root := writeFixture(t, validConfig+"\n[unknown]\nkey = 1\n")
	_, err := Load(filepath.Join(root, "celine.toml"))
	if err == nil || !strings.Contains(err.Error(), "unknown configuration keys") {
		t.Fatalf("expected unknown-key error, got %v", err)
	}
}

func TestRejectsEscapingLibraryPath(t *testing.T) {
	manifest := strings.Replace(validConfig, `path = "Calibre/General"`, `path = "../outside"`, 1)
	root := writeFixture(t, manifest)
	cfg, err := Load(filepath.Join(root, "celine.toml"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate(ValidateOptions{CheckFilesystem: false})
	if err == nil || !strings.Contains(err.Error(), "must stay inside the media root") {
		t.Fatalf("expected escape error, got %v", err)
	}
}

func TestRejectsMissingMetadata(t *testing.T) {
	root := writeFixture(t, validConfig)
	if err := os.Remove(filepath.Join(root, "media", "Calibre", "General", "metadata.db")); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(root, "celine.toml"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate(ValidateOptions{CheckFilesystem: true})
	if err == nil || !strings.Contains(err.Error(), "metadata.db") {
		t.Fatalf("expected metadata error, got %v", err)
	}
}

func TestRejectsDuplicateServePort(t *testing.T) {
	manifest := strings.Replace(validConfig, "https_port = 8443", "https_port = 443", 1)
	root := writeFixture(t, manifest)
	cfg, err := Load(filepath.Join(root, "celine.toml"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate(ValidateOptions{CheckFilesystem: false})
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expected duplicate-port error, got %v", err)
	}
}

func TestRejectsBadHTTPSPort(t *testing.T) {
	manifest := strings.Replace(validConfig, "https_port = 8443", "https_port = 1234", 1)
	root := writeFixture(t, manifest)
	cfg, err := Load(filepath.Join(root, "celine.toml"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate(ValidateOptions{CheckFilesystem: false})
	if err == nil || !strings.Contains(err.Error(), "443, 8443, or 10000") {
		t.Fatalf("expected HTTPS-port error, got %v", err)
	}
}

func TestRejectsNoServices(t *testing.T) {
	manifest := strings.Replace(validConfig, "enable = true\nversion = \"9.18.0\"", "enable = false\nversion = \"9.18.0\"", 1)
	manifest = strings.Replace(manifest, "enable = true\nimage = \"example/kavita:1\"", "enable = false\nimage = \"example/kavita:1\"", 1)
	root := writeFixture(t, manifest)
	cfg, err := Load(filepath.Join(root, "celine.toml"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate(ValidateOptions{CheckFilesystem: false})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("expected no-services error, got %v", err)
	}
}

func TestSetStateDirUpdatesDerivedPaths(t *testing.T) {
	root := writeFixture(t, validConfig)
	cfg, err := Load(filepath.Join(root, "celine.toml"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "elsewhere")
	if err := cfg.SetStateDir(target); err != nil {
		t.Fatal(err)
	}
	if cfg.Resolved.KavitaConfigDir != filepath.Join(target, "kavita", "config") {
		t.Fatalf("kavita config dir = %q", cfg.Resolved.KavitaConfigDir)
	}
	if cfg.Resolved.CalibreConfigDir != filepath.Join(target, "calibre") {
		t.Fatalf("calibre config dir = %q", cfg.Resolved.CalibreConfigDir)
	}
}
