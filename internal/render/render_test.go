package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Meillaya/celine/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Path = "/repo/celine.toml"
	cfg.Root = "/repo"
	cfg.Paths = config.Paths{MediaRoot: "/media", StateDir: "state", BackupDir: "backups"}
	cfg.Runtime.Timezone = "UTC"
	cfg.Calibre.Version = "9.18.0"
	cfg.Calibre.Port = 8080
	cfg.Calibre.Libraries = []config.Library{
		{Name: "General", Path: "Calibre/General"},
		{Name: "Theory", Path: "Calibre/Theory"},
	}
	cfg.Kavita.Image = "example/kavita:1"
	cfg.Kavita.Port = 5000
	cfg.Tailnet.Hostname = "node.example.ts.net"
	cfg.Tailnet.Serve = []config.ServeTarget{
		{Service: "kavita", HTTPSPort: 443},
		{Service: "calibre", HTTPSPort: 8443},
	}
	cfg.Backup.Retention = 10
	if err := cfg.SetStateDir("/repo/state"); err != nil {
		t.Fatal(err)
	}
	// SetStateDir writes into Resolved; MediaRoot comes from resolve(), which
	// is unexported and only runs through Load. Set the field directly for
	// these render tests.
	cfg.Resolved.MediaRoot = "/media"
	return cfg
}

func TestComposeContainsBothServicesAndNoSecrets(t *testing.T) {
	cfg := testConfig(t)
	out, err := Compose(cfg, 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		"name: celine",
		"celine/calibre:9.18.0",
		"example/kavita:1",
		"127.0.0.1:8080:8080",
		"127.0.0.1:5000:5000",
		"/media:/media",
		"/repo/state/kavita/config:/config",
		"PUID: \"0\"",
		"PGID: \"0\"",
		"stop_grace_period: 15s",
		"stop_grace_period: 30s",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("compose missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "TokenKey") || strings.Contains(text, "KAVITA_TOKEN_KEY") {
		t.Errorf("compose leaked secret material:\n%s", text)
	}
}

func TestComposeRootfulUsesHostIDs(t *testing.T) {
	cfg := testConfig(t)
	cfg.Runtime.Rootless = false
	out, err := Compose(cfg, 1000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "PUID: \"1000\"") || !strings.Contains(text, "PGID: \"1001\"") {
		t.Errorf("rootful PUID/PGID missing:\n%s", text)
	}
	if !strings.Contains(text, "user: 1000:1001") {
		t.Errorf("rootful calibre user missing:\n%s", text)
	}
}

func TestAppSettingsPreservesUnknownKeys(t *testing.T) {
	cfg := testConfig(t)
	out, err := AppSettings(cfg, "new-token", []byte(`{"TokenKey":"old","Custom":"keep","OpenIdConnectSettings":{"Enabled":false}}`), TargetDocker)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["TokenKey"] != "new-token" {
		t.Errorf("token = %v", decoded["TokenKey"])
	}
	if decoded["Custom"] != "keep" {
		t.Errorf("unknown key not preserved: %v", decoded["Custom"])
	}
	if decoded["IpAddresses"] != "0.0.0.0,::" {
		t.Errorf("docker ip = %v", decoded["IpAddresses"])
	}
	if decoded["Port"] != float64(5000) {
		t.Errorf("port = %v", decoded["Port"])
	}
}

func TestAppSettingsNativeBindsLoopback(t *testing.T) {
	cfg := testConfig(t)
	out, err := AppSettings(cfg, "token", nil, TargetNative)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"IpAddresses": "127.0.0.1"`) {
		t.Fatalf("native appsettings did not bind loopback:\n%s", out)
	}
}

func TestAppSettingsNullDoesNotPanic(t *testing.T) {
	cfg := testConfig(t)
	out, err := AppSettings(cfg, "token", []byte("null"), TargetDocker)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["TokenKey"] != "token" {
		t.Fatalf("token = %v", decoded["TokenKey"])
	}
}

func TestTailnetEndpoints(t *testing.T) {
	cfg := testConfig(t)
	endpoints, err := Endpoints(cfg, "node.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %d", len(endpoints))
	}
	if endpoints[0].URL() != "https://node.example.ts.net" {
		t.Errorf("kavita URL = %q", endpoints[0].URL())
	}
	if endpoints[1].URL() != "https://node.example.ts.net:8443" {
		t.Errorf("calibre URL = %q", endpoints[1].URL())
	}
}

func TestSystemdUnits(t *testing.T) {
	units, err := SystemdUnits(SystemdOptions{
		CelineBin:     "/nix/store/celine/bin/celine",
		ConfigPath:    "/repo/celine.toml",
		User:          "mei",
		Group:         "users",
		Home:          "/home/mei",
		DockerHost:    "unix:///run/user/1000/podman/podman.sock",
		MediaRoot:     "/media",
		StateDir:      "/repo/state",
		BackupDir:     "/repo/backups",
		OnCalendar:    "daily",
		PathEnv:       "/home/mei/.nix-profile/bin:/usr/bin:/bin",
		Rootless:      true,
		UID:           1000,
		BackupEnabled: true,
		ExtraEnv: map[string]string{
			"SOPS_AGE_KEY_FILE": "/home/mei/.config/sops/age/keys.txt",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := units["celine.service"]
	for _, want := range []string{
		"RequiresMountsFor=/media /repo/state /repo/backups",
		"User=mei",
		"After=network-online.target tailscaled.service user@1000.service",
		"Environment=DOCKER_HOST=unix:///run/user/1000/podman/podman.sock",
		"Environment=PATH=/home/mei/.nix-profile/bin:/usr/bin:/bin",
		"Environment=XDG_RUNTIME_DIR=/run/user/1000",
		"Environment=SOPS_AGE_KEY_FILE=/home/mei/.config/sops/age/keys.txt",
		"ExecStart=/nix/store/celine/bin/celine up --config /repo/celine.toml",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service missing %q:\n%s", want, service)
		}
	}
	if !strings.Contains(service, "ExecStartPre=/bin/sh") {
		t.Errorf("rootless service has no podman socket wait:\n%s", service)
	}
	if !strings.Contains(units["celine-backup.timer"], "OnCalendar=daily") {
		t.Errorf("timer missing calendar:\n%s", units["celine-backup.timer"])
	}
	if !strings.Contains(units["celine-backup.service"], "RequiresMountsFor=/media /repo/state /repo/backups") {
		t.Errorf("backup service missing mounts:\n%s", units["celine-backup.service"])
	}
}

func TestSystemdUnitsSkipsBackupWhenDisabled(t *testing.T) {
	units, err := SystemdUnits(SystemdOptions{
		CelineBin:  "/bin/celine",
		ConfigPath: "/repo/celine.toml",
		User:       "mei",
		Group:      "mei",
		Home:       "/home/mei",
		MediaRoot:  "/media",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units["celine.service"] == "" {
		t.Fatalf("units = %v", units)
	}
}

func TestSystemdUnitsQuotesPathsWithSpaces(t *testing.T) {
	units, err := SystemdUnits(SystemdOptions{
		CelineBin:     "/opt/celine bin/celine",
		ConfigPath:    "/repo with space/celine.toml",
		User:          "mei",
		Group:         "mei",
		Home:          "/home/mei",
		MediaRoot:     "/media with space",
		BackupEnabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := units["celine.service"]
	for _, want := range []string{
		"RequiresMountsFor=\"/media with space\"",
		"ExecStart=\"/opt/celine bin/celine\" up --config \"/repo with space/celine.toml\"",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service missing %q:\n%s", want, service)
		}
	}
}
