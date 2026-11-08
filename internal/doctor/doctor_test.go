package doctor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Meillaya/celine/internal/config"
	"github.com/Meillaya/celine/internal/execx"
)

func levelFor(report Report, name string) Level {
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Level
		}
	}
	return ""
}

func TestCheckPortWithExpectRunning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	level, _ := checkPort(context.Background(), &http.Client{}, "calibre", port, false)
	if level != OK {
		t.Fatalf("preflight free port level = %s", level)
	}
	level, _ = checkPort(context.Background(), &http.Client{}, "calibre", port, true)
	if level != Fail {
		t.Fatalf("expect-running free port level = %s", level)
	}
}

func TestCheckPortReportsWrongService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	level, detail := checkPort(context.Background(), server.Client(), "calibre", port, true)
	if level != Warn || !strings.Contains(detail, "does not answer") {
		t.Fatalf("level = %s, detail = %s", level, detail)
	}
}

func TestCheckBootReportsMissingUnitsAndLinger(t *testing.T) {
	systemd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "systemd", "user", "sockets.target.wants"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "systemd", "user", "sockets.target.wants", "podman.socket"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &execx.Fake{Func: func(call execx.Call) (execx.Result, error) {
		joined := strings.Join(call.Args, " ")
		switch {
		case strings.Contains(joined, "show-user"):
			return execx.Result{Stdout: []byte("Linger=no\n")}, nil
		case strings.Contains(joined, "is-failed"):
			return execx.Result{Stdout: []byte("active\n")}, nil
		case strings.Contains(joined, "is-active"):
			return execx.Result{Stdout: []byte("active\n")}, nil
		default:
			return execx.Result{Stdout: []byte("enabled\n")}, nil
		}
	}}
	cfg := config.Default()
	cfg.Runtime.Rootless = true
	cfg.Backup.Enable = true
	report := Report{}
	checkBoot(&report, cfg, Deps{SystemdDir: systemd, User: "tester", Runner: runner})
	if levelFor(report, "boot units") != Warn {
		t.Fatalf("boot units = %s", levelFor(report, "boot units"))
	}
	if levelFor(report, "boot linger") != Warn {
		t.Fatalf("boot linger = %s", levelFor(report, "boot linger"))
	}
	if levelFor(report, "podman socket") != OK {
		t.Fatalf("podman socket = %s", levelFor(report, "podman socket"))
	}

	for _, name := range []string{"celine.service", "celine-backup.timer"} {
		if err := os.WriteFile(filepath.Join(systemd, name), []byte("unit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	report = Report{}
	checkBoot(&report, cfg, Deps{SystemdDir: systemd, User: "tester", Runner: runner})
	if levelFor(report, "boot service") != OK {
		t.Fatalf("boot service = %s", levelFor(report, "boot service"))
	}
	if levelFor(report, "backup timer") != OK {
		t.Fatalf("backup timer = %s", levelFor(report, "backup timer"))
	}
}

func TestCheckAgeIdentityModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	identity := filepath.Join(home, "age.key")
	if err := os.WriteFile(identity, []byte("AGE-SECRET-KEY"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", identity)
	report := Report{}
	checkAgeIdentity(&report, Deps{})
	if levelFor(report, "age identity") != Fail {
		t.Fatalf("age identity level = %s", levelFor(report, "age identity"))
	}
	if err := os.Chmod(identity, 0o600); err != nil {
		t.Fatal(err)
	}
	report = Report{}
	checkAgeIdentity(&report, Deps{})
	if levelFor(report, "age identity") != OK {
		t.Fatalf("age identity level after chmod = %s", levelFor(report, "age identity"))
	}
}

func TestCheckSecretPermissions(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Root = root
	cfg.Resolved.KavitaConfigDir = filepath.Join(root, "state", "kavita", "config")
	cfg.Resolved.CalibreConfigDir = filepath.Join(root, "state", "calibre")
	cfg.Resolved.BackupDir = filepath.Join(root, "backups")
	for _, dir := range []string{cfg.Resolved.KavitaConfigDir, cfg.Resolved.CalibreConfigDir, cfg.Resolved.BackupDir, filepath.Join(root, "config")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Resolved.KavitaConfigDir, "appsettings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "appsettings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Resolved.BackupDir, "kavita_config_1.tar.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Report{}
	checkSecretPermissions(&report, cfg)
	if levelFor(report, "state permissions") != Fail {
		t.Fatalf("state permissions = %s", levelFor(report, "state permissions"))
	}
	if levelFor(report, "legacy permissions") != Warn {
		t.Fatalf("legacy permissions = %s", levelFor(report, "legacy permissions"))
	}
}

func TestCountCalibreBooksUsesJSON(t *testing.T) {
	runner := &execx.Fake{Func: func(call execx.Call) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`[{"count":42}]`)}, nil
	}}
	count, err := countCalibreBooks(context.Background(), runner, "sqlite3", "/tmp/metadata.db")
	if err != nil {
		t.Fatal(err)
	}
	if count != 42 {
		t.Fatalf("count = %d", count)
	}
	if !strings.Contains(runner.CommandLines()[0], "-cmd .timeout 5000") {
		t.Fatalf("book count lacks busy timeout: %s", runner.CommandLines()[0])
	}
}
