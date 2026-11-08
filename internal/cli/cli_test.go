package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Meillaya/celine/internal/archiveutil"
	"github.com/Meillaya/celine/internal/execx"
)

const calibreOnlyConfig = `
[meta]
schema = 1
[paths]
media_root = "media"
state_dir = "state"
backup_dir = "backups"
[runtime]
rootless = true
timezone = "UTC"
[calibre]
enable = true
version = "9.18.0"
port = 18080
[[calibre.libraries]]
name = "General"
path = "General"
[kavita]
enable = false
[tailnet]
enable = false
[backup]
enable = true
retention = 3
on_calendar = "daily"
`

type testEnvironment struct {
	root   string
	app    App
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	runner *execx.Fake
	env    map[string]string
}

func newTestEnvironment(t *testing.T, config string) *testEnvironment {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "media", "General"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "media", "Kavita"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "media", "General", "metadata.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "celine.toml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secretspec.toml"), []byte("[project]\nname=\"x\"\nrevision=\"1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "backups"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "celine-bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runner := &execx.Fake{}
	env := map[string]string{}
	app := App{
		Version:    "test",
		Stdout:     stdout,
		Stderr:     stderr,
		Runner:     runner,
		SQLite:     "sqlite3",
		Docker:     "docker",
		Tailscale:  "tailscale",
		Sops:       "sops",
		Secretspec: "secretspec",
		CelineBin:  bin,
		Now:        func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
		LookupEnv: func(name string) (string, bool) {
			value, ok := env[name]
			return value, ok
		},
		Workdir:  root,
		UID:      1000,
		GID:      1000,
		Username: "tester",
		Home:     root,
	}
	return &testEnvironment{root: root, app: app, stdout: stdout, stderr: stderr, runner: runner, env: env}
}

func TestHelpAndFlagOrdering(t *testing.T) {
	env := newTestEnvironment(t, calibreOnlyConfig)
	if code := Run([]string{"help", "status"}, env.app); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	if !strings.Contains(env.stderr.String(), "-strict") {
		t.Fatalf("help did not print status flags: %s", env.stderr.String())
	}
	// A common flag after a positional must still be parsed as a flag.
	code := Run([]string{"logs", "calibre", "--config", filepath.Join(env.root, "celine.toml")}, env.app)
	if code != 0 {
		t.Fatalf("logs with trailing --config exit = %d: %s", code, env.stderr.String())
	}
	if len(env.runner.Calls) != 1 || !strings.Contains(env.runner.CommandLines()[0], "logs --tail=200 calibre") {
		t.Fatalf("logs calls = %v", env.runner.CommandLines())
	}
}

func TestStatusTreats503AsDownAndStrictFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	config := strings.Replace(calibreOnlyConfig, "port = 18080", "port = "+itoa(port), 1)
	env := newTestEnvironment(t, config)
	env.runner.Func = func(call execx.Call) (execx.Result, error) {
		return execx.Result{}, nil
	}
	code := Run([]string{"status", "--strict"}, env.app)
	if code != 1 {
		t.Fatalf("strict status exit = %d\n%s", code, env.stdout.String())
	}
	if !strings.Contains(env.stdout.String(), "calibre  down") {
		t.Fatalf("status did not report down:\n%s", env.stdout.String())
	}
	env.stdout.Reset()
	if code := Run([]string{"status", "--json"}, env.app); code != 0 {
		t.Fatalf("json status exit = %d", code)
	}
	var report map[string]any
	if err := json.Unmarshal(env.stdout.Bytes(), &report); err != nil {
		t.Fatalf("status json: %v\n%s", err, env.stdout.String())
	}
	if report["ok"] != false {
		t.Fatalf("json ok = %v", report["ok"])
	}
}

func TestRestoreDryRunVerifiesArchive(t *testing.T) {
	env := newTestEnvironment(t, calibreOnlyConfig)
	dir := filepath.Join(env.root, "not-an-archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(env.root, "arbitrary.tar.gz")
	if err := archiveutil.TarGz(dir, archive); err != nil {
		t.Fatal(err)
	}
	// The flag after the positional used to be parsed as a second positional.
	code := Run([]string{"restore", archive, "--dry-run", "--config", filepath.Join(env.root, "celine.toml")}, env.app)
	if code != 1 {
		t.Fatalf("arbitrary archive restore exit = %d\n%s", code, env.stdout.String())
	}
	if !strings.Contains(env.stderr.String(), "manifest") {
		t.Fatalf("expected manifest verification error, got: %s", env.stderr.String())
	}
}

func TestBackupDryRunCalibreOnly(t *testing.T) {
	env := newTestEnvironment(t, calibreOnlyConfig)
	if code := Run([]string{"backup", "--dry-run"}, env.app); code != 0 {
		t.Fatalf("backup dry run exit = %d: %s", code, env.stderr.String())
	}
	if !strings.Contains(env.stdout.String(), "state/calibre") {
		t.Fatalf("backup dry run output = %s", env.stdout.String())
	}
}

func TestSystemdRenderRejectsEphemeralBinary(t *testing.T) {
	env := newTestEnvironment(t, calibreOnlyConfig)
	env.app.CelineBin = "/tmp/go-build123/b001/exe/celine"
	out := filepath.Join(env.root, "units")
	if code := Run([]string{"systemd", "render", "--out", out}, env.app); code != 1 {
		t.Fatalf("ephemeral binary render exit = %d\n%s", code, env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "temporary binary") {
		t.Fatalf("stderr = %s", env.stderr.String())
	}
}

func TestSystemdRenderRejectsRoot(t *testing.T) {
	env := newTestEnvironment(t, calibreOnlyConfig)
	isRoot := true
	env.app.IsRoot = &isRoot
	if code := Run([]string{"systemd", "render"}, env.app); code != 1 {
		t.Fatalf("root render exit = %d", code)
	}
	if !strings.Contains(env.stderr.String(), "operator account") {
		t.Fatalf("stderr = %s", env.stderr.String())
	}
}

func TestSystemdRenderHonorsBackupEnable(t *testing.T) {
	env := newTestEnvironment(t, strings.Replace(calibreOnlyConfig, "enable = true\nretention", "enable = false\nretention", 1))
	out := filepath.Join(env.root, "units")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"celine-backup.service", "celine-backup.timer"} {
		if err := os.WriteFile(filepath.Join(out, name), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code := Run([]string{"systemd", "render", "--out", out}, env.app); code != 0 {
		t.Fatalf("render exit = %d: %s", code, env.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, "celine-backup.timer")); !os.IsNotExist(err) {
		t.Fatalf("backup timer rendered while backup.enable=false: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "celine.service")); err != nil {
		t.Fatalf("stack unit missing: %v", err)
	}
}

func TestMigrateRefusesRunningStack(t *testing.T) {
	env := newTestEnvironment(t, calibreOnlyConfig)
	if err := os.WriteFile(filepath.Join(env.root, "build", "compose.yaml"), []byte("name: celine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.runner.Func = func(call execx.Call) (execx.Result, error) {
		if strings.Contains(strings.Join(call.Args, " "), "ps -q") {
			return execx.Result{Stdout: []byte("container-id\n")}, nil
		}
		return execx.Result{}, nil
	}
	code := Run([]string{"migrate", "--yes"}, env.app)
	if code != 1 {
		t.Fatalf("migrate exit = %d\n%s", code, env.stdout.String())
	}
	if !strings.Contains(env.stderr.String(), "refusing to replace state under a running stack") {
		t.Fatalf("stderr = %s", env.stderr.String())
	}
}

func TestUpRestartsKavitaWhenSettingsAreNewer(t *testing.T) {
	calibre := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer calibre.Close()
	kavita := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer kavita.Close()
	config := `
[meta]
schema = 1
[paths]
media_root = "media"
state_dir = "state"
backup_dir = "backups"
[runtime]
rootless = true
timezone = "UTC"
[calibre]
enable = true
version = "9.18.0"
port = ` + itoa(calibre.Listener.Addr().(*net.TCPAddr).Port) + `
[[calibre.libraries]]
name = "General"
path = "General"
[kavita]
enable = true
image = "example/kavita:1"
port = ` + itoa(kavita.Listener.Addr().(*net.TCPAddr).Port) + `
[tailnet]
enable = false
[backup]
enable = true
retention = 3
on_calendar = "daily"
`
	env := newTestEnvironment(t, config)
	env.env["DOCKER_HOST"] = "unix:///run/user/1000/podman/podman.sock"
	env.env["KAVITA_TOKEN_KEY"] = "new-token"
	configDir := filepath.Join(env.root, "state", "kavita", "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "appsettings.json"), []byte(`{"TokenKey":"old-token","Custom":"keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env.runner.Func = func(call execx.Call) (execx.Result, error) {
		joined := strings.Join(call.Args, " ")
		switch {
		case strings.Contains(joined, "info --format"):
			return execx.Result{Stdout: []byte("5.8.7 test\n")}, nil
		case strings.Contains(joined, " image inspect"):
			return execx.Result{}, nil
		case strings.Contains(joined, " ps -q kavita"):
			return execx.Result{Stdout: []byte("container-id\n")}, nil
		case strings.Contains(joined, "inspect --format"):
			return execx.Result{Stdout: []byte("2020-01-01T00:00:00.000000000Z\n")}, nil
		default:
			return execx.Result{}, nil
		}
	}
	code := Run([]string{"up", "--no-tailnet", "--pull"}, env.app)
	if code != 0 {
		t.Fatalf("up exit = %d\nstdout:\n%s\nstderr:\n%s", code, env.stdout.String(), env.stderr.String())
	}
	if !strings.Contains(env.stdout.String(), "restarting kavita to apply appsettings.json") {
		t.Fatalf("up did not restart Kavita:\n%s", env.stdout.String())
	}
	restarted := false
	pulled := false
	for _, line := range env.runner.CommandLines() {
		if strings.Contains(line, "restart kavita") {
			restarted = true
		}
		if strings.Contains(line, "pull kavita") {
			pulled = true
		}
	}
	if !restarted {
		t.Fatalf("restart command missing: %v", env.runner.CommandLines())
	}
	if !pulled {
		t.Fatalf("pull command missing: %v", env.runner.CommandLines())
	}
	if !strings.Contains(env.stderr.String(), "KAVITA_TOKEN_KEY is set") {
		t.Fatalf("missing environment override warning: %s", env.stderr.String())
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
