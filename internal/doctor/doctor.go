// Package doctor performs read-only preflight and post-deploy checks.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Meillaya/celine/internal/backup"
	"github.com/Meillaya/celine/internal/compose"
	"github.com/Meillaya/celine/internal/config"
	"github.com/Meillaya/celine/internal/execx"
	"github.com/Meillaya/celine/internal/kavita"
	"github.com/Meillaya/celine/internal/render"
	"github.com/Meillaya/celine/internal/secrets"
	"github.com/Meillaya/celine/internal/tailnet"
)

// Level is a check outcome.
type Level string

const (
	// OK means the check passed.
	OK Level = "ok"
	// Warn means the deployment can run but attention is needed.
	Warn Level = "warn"
	// Fail means a required condition is missing.
	Fail Level = "fail"
)

// Check is one named result.
type Check struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// URL is a published service address.
type URL struct {
	Service string `json:"service"`
	URL     string `json:"url"`
}

// Report is the complete doctor result.
type Report struct {
	Version    string
	ConfigPath string
	Checks     []Check
	URLs       []URL
}

// Deps are the external handles used by the checks.
type Deps struct {
	ConfigPath string
	Config     *config.Config
	Runner     execx.Runner
	Secrets    *secrets.Resolver
	SQLite     string
	Docker     string
	Tailscale  string
	Version    string
	Now        func() time.Time
	HTTPClient *http.Client
	// ExpectRunning turns a free service port into a failure. Doctor remains a
	// valid preflight tool without it.
	ExpectRunning bool
	UID           int
	GID           int
	User          string
	SystemdDir    string
	IsNixOS       bool
	Systemctl     string
	Loginctl      string
}

func (r *Report) add(name string, level Level, detail, hint string) {
	r.Checks = append(r.Checks, Check{Name: name, Level: level, Detail: detail, Hint: hint})
}

// IsClean reports whether every check is at least OK (or Warn when strict is
// false).
func (r Report) IsClean(strict bool) bool {
	for _, check := range r.Checks {
		if check.Level == Fail {
			return false
		}
		if strict && check.Level == Warn {
			return false
		}
	}
	return true
}

// Counts returns the number of checks per level.
func (r Report) Counts() map[Level]int {
	counts := map[Level]int{}
	for _, check := range r.Checks {
		counts[check.Level]++
	}
	return counts
}

// Run executes all checks. It never changes the system.
func Run(ctx context.Context, deps Deps) Report {
	report := Report{Version: deps.Version, ConfigPath: deps.ConfigPath}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.SQLite == "" {
		deps.SQLite = "sqlite3"
	}
	if deps.Docker == "" {
		deps.Docker = "docker"
	}
	if deps.Tailscale == "" {
		deps.Tailscale = "tailscale"
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 2 * time.Second}
	}
	if deps.UID == 0 && deps.GID == 0 {
		deps.UID = os.Getuid()
		deps.GID = os.Getgid()
	}
	if deps.User == "" {
		deps.User = os.Getenv("USER")
	}
	if deps.SystemdDir == "" {
		deps.SystemdDir = "/etc/systemd/system"
	}
	if deps.Systemctl == "" {
		deps.Systemctl = "systemctl"
	}
	if deps.Loginctl == "" {
		deps.Loginctl = "loginctl"
	}
	if !deps.IsNixOS {
		if _, err := os.Stat("/etc/NIXOS"); err == nil {
			deps.IsNixOS = true
		}
	}
	cfg := deps.Config
	if cfg == nil {
		report.add("configuration", Fail, "configuration was not loaded", "fix celine.toml and rerun")
		return report
	}

	report.add("configuration", OK, cfg.Path, "")

	// Media root and mount.
	mediaLevel := OK
	mediaDetail := cfg.Resolved.MediaRoot
	if info, err := os.Stat(cfg.Resolved.MediaRoot); err != nil {
		mediaLevel = Fail
		mediaDetail = err.Error()
	} else if !info.IsDir() {
		mediaLevel = Fail
		mediaDetail = "not a directory"
	} else if mount, ok := lookupMount(cfg.Resolved.MediaRoot); ok {
		mediaDetail = fmt.Sprintf("%s on %s (%s)", cfg.Resolved.MediaRoot, mount.source, mount.fstype)
	} else {
		mediaLevel = Warn
		mediaDetail = cfg.Resolved.MediaRoot + " is not a separate mount"
	}
	report.add("media root", mediaLevel, mediaDetail, "mount the media drive before starting services")

	if cfg.Calibre.Enable {
		report.add("calibre libraries", OK, fmt.Sprintf("%d declared", len(cfg.Calibre.Libraries)), "")
		for _, library := range cfg.Calibre.Libraries {
			hostPath := filepath.Join(cfg.Resolved.MediaRoot, library.Path)
			metadata := filepath.Join(hostPath, "metadata.db")
			if _, err := os.Stat(metadata); err != nil {
				report.add("calibre "+library.Name, Fail, "missing metadata.db at "+metadata, "check the library path in celine.toml")
				continue
			}
			count, err := countCalibreBooks(ctx, deps.Runner, deps.SQLite, metadata)
			if err != nil {
				report.add("calibre "+library.Name, Warn, "metadata.db readable, book count failed: "+err.Error(), "")
				continue
			}
			report.add("calibre "+library.Name, OK, fmt.Sprintf("%d books", count), "")
		}
	}

	if cfg.Kavita.Enable {
		dbPath := filepath.Join(cfg.Resolved.KavitaConfigDir, "kavita.db")
		if info, err := os.Stat(dbPath); err != nil || info.IsDir() {
			report.add("kavita database", Fail, "not found at "+dbPath, "run `celine migrate --yes` to import the legacy config, or start Kavita once")
		} else {
			if err := kavita.IntegrityCheck(ctx, deps.Runner, deps.SQLite, dbPath); err != nil {
				report.add("kavita database", Fail, err.Error(), "restore the latest `celine backup` archive")
			} else {
				inventory, err := kavita.ReadInventory(ctx, deps.Runner, deps.SQLite, dbPath)
				if err != nil {
					report.add("kavita database", Fail, err.Error(), "")
				} else {
					detail := fmt.Sprintf("%d users, %d libraries, %d folders, DB version %s", len(inventory.Users), len(inventory.Libraries), len(inventory.FolderPaths), inventory.Version)
					if len(inventory.Users) == 0 {
						report.add("kavita database", Warn, detail, "create an admin account in the UI")
					} else {
						report.add("kavita database", OK, detail, "")
					}
					unresolved := 0
					for _, folder := range inventory.FolderPaths {
						hostPath, ok := kavita.HostPath(folder.Path, cfg.Kavita.LibraryMount, cfg.KavitaMediaHostPath())
						if !ok {
							unresolved++
							report.add("kavita folder", Warn, "unmapped path "+folder.Path, "the library is outside "+cfg.Kavita.LibraryMount)
							continue
						}
						if info, err := os.Stat(hostPath); err != nil || !info.IsDir() {
							unresolved++
							report.add("kavita folder", Fail, "missing "+hostPath, "restore the media drive or update the library path")
						}
					}
					if unresolved == 0 {
						report.add("kavita folders", OK, "all declared library paths resolve", "")
					}
				}
			}
		}
	}

	// Secrets.
	if deps.Secrets != nil && cfg.Kavita.Enable {
		if _, err := deps.Secrets.Check(ctx); err != nil {
			report.add("secrets", Fail, err.Error(), "run `secretspec check` and provide KAVITA_TOKEN_KEY")
		} else {
			report.add("secrets", OK, "required SecretSpec values resolve (value-free check)", "")
		}
	}

	// Runtime. The native NixOS path runs host services, not Compose.
	if deps.Runner != nil && !deps.IsNixOS {
		client := compose.New(deps.Runner, filepath.Join(cfg.Root, "build", "compose.yaml"))
		client.Docker = deps.Docker
		if info, err := client.RuntimeInfo(ctx); err != nil {
			report.add("container runtime", Fail, err.Error(), "set DOCKER_HOST to the rootless Podman socket")
		} else if version, err := client.ComposeVersion(ctx); err != nil {
			report.add("compose", Fail, err.Error(), "")
		} else {
			report.add("container runtime", OK, info+"; "+version, "")
		}
		if exists, err := client.ImageExists(ctx, cfg.CalibreImage()); err == nil && !exists && cfg.Calibre.Enable {
			report.add("calibre image", Warn, cfg.CalibreImage()+" is not built", "run `celine up --build`")
		}
		if exists, err := client.ImageExists(ctx, cfg.Kavita.Image); err == nil && !exists && cfg.Kavita.Enable {
			report.add("kavita image", Warn, cfg.Kavita.Image+" is not pulled", "run `celine up`")
		}
	}

	// Ports and health.
	for _, service := range []string{"calibre", "kavita"} {
		if !cfg.ServiceEnabled(service) {
			continue
		}
		port, _ := cfg.UpstreamPort(service)
		level, detail := checkPort(ctx, deps.HTTPClient, service, port, deps.ExpectRunning)
		report.add(service+" port", level, detail, "stop the process using the port or change it in celine.toml")
	}

	checkBoot(&report, cfg, deps)
	if cfg.Kavita.Enable {
		checkAgeIdentity(&report, deps)
	}
	checkSecretPermissions(&report, cfg)

	// Tailnet.
	endpoints := []render.Endpoint{}
	if cfg.Tailnet.Enable && deps.Runner != nil {
		self, err := tailnet.ReadSelf(ctx, deps.Runner, deps.Tailscale)
		if err != nil {
			report.add("tailscale", Fail, err.Error(), "start tailscaled and run `tailscale up`")
		} else {
			hostname := tailnet.Hostname(cfg.Tailnet.Hostname, self.DNSName)
			if self.BackendState == "Running" {
				report.add("tailscale", OK, self.BackendState+" as "+hostname, "")
			} else {
				report.add("tailscale", Fail, "backend state "+self.BackendState, "run `tailscale up`")
			}
			if !self.CertDomainReady(hostname) {
				report.add("tailscale https", Warn, "no certificate domain for "+hostname, "enable HTTPS certificates for the tailnet")
			} else {
				report.add("tailscale https", OK, "certificate domain available", "")
			}
			expanded, err := render.Endpoints(cfg, hostname)
			if err != nil {
				report.add("tailscale serve", Fail, err.Error(), "")
			} else {
				endpoints = expanded
				for _, endpoint := range endpoints {
					report.URLs = append(report.URLs, URL{Service: endpoint.Service, URL: endpoint.URL()})
				}
				raw, err := tailnet.ServeStatus(ctx, deps.Runner, deps.Tailscale)
				if err != nil {
					report.add("tailscale serve", Warn, err.Error(), "run `celine tailnet apply`")
				} else {
					missing := 0
					for _, endpoint := range endpoints {
						needle := fmt.Sprintf("127.0.0.1:%d", endpoint.UpstreamPort)
						if !strings.Contains(string(raw), needle) {
							missing++
						}
					}
					if missing == 0 {
						report.add("tailscale serve", OK, "all configured ports are served", "")
					} else {
						report.add("tailscale serve", Warn, fmt.Sprintf("%d endpoint(s) are not applied", missing), "run `celine tailnet apply`")
					}
				}
			}
		}
	}

	// Backups.
	if cfg.Backup.Enable {
		if info, err := os.Stat(cfg.Resolved.BackupDir); err != nil || !info.IsDir() {
			report.add("backups", Warn, "backup directory does not exist yet", "run `celine backup`")
		} else if latest, err := backup.Latest(cfg.Resolved.BackupDir); err != nil {
			report.add("backups", Warn, err.Error(), "")
		} else if latest == "" {
			report.add("backups", Warn, "no celine archive yet", "run `celine backup`")
		} else if info, err := os.Stat(latest); err != nil {
			report.add("backups", Warn, err.Error(), "")
		} else {
			age := time.Since(info.ModTime())
			if age > 48*time.Hour {
				report.add("backups", Warn, fmt.Sprintf("latest archive is %s old", age.Round(time.Hour)), "run `celine backup` or enable the timer")
			} else {
				report.add("backups", OK, fmt.Sprintf("latest archive is %s old", age.Round(time.Minute)), "")
			}
			if deps.Runner != nil {
				verified, err := backup.Verify(ctx, backup.VerifyOptions{
					Archive:         latest,
					Runner:          deps.Runner,
					SQLite:          deps.SQLite,
					RequireKavitaDB: cfg.Kavita.Enable,
				})
				switch {
				case err != nil:
					report.add("backup integrity", Fail, err.Error(), "run `celine backup` and keep the previous archive until a good one exists")
				case verified.SecretsPresent:
					report.add("backup integrity", Warn, "latest archive still contains a plaintext Kavita signing key", "run `celine backup` with this version to write a redacted archive")
				default:
					report.add("backup integrity", OK, fmt.Sprintf("%d files verified", verified.Files), "")
				}
			}
		}
	}

	// Disk space.
	for _, dir := range []string{cfg.Resolved.MediaRoot, cfg.Resolved.BackupDir} {
		if free, err := freeBytes(dir); err == nil {
			detail := fmt.Sprintf("%s free on %s", humanBytes(free), dir)
			level := OK
			if free < 512<<20 {
				level = Fail
			} else if free < 5<<30 {
				level = Warn
			}
			report.add("disk space", level, detail, "")
		}
	}

	// Rendered artifacts. The native NixOS path does not use Compose.
	if deps.IsNixOS {
		report.add("rendered compose", OK, "not used by the native NixOS path", "")
		return report
	}
	composeFile := filepath.Join(cfg.Root, "build", "compose.yaml")
	rendered, renderErr := render.Compose(cfg, deps.UID, deps.GID)
	existing, readErr := os.ReadFile(composeFile)
	switch {
	case readErr != nil:
		report.add("rendered compose", Warn, "missing "+composeFile, "run `celine render`")
	case renderErr != nil:
		report.add("rendered compose", Warn, renderErr.Error(), "run `celine render`")
	case !bytes.Equal(existing, rendered):
		report.add("rendered compose", Warn, composeFile+" is out of date", "run `celine render` or `celine up`")
	default:
		report.add("rendered compose", OK, composeFile, "")
	}
	return report
}

func checkPort(ctx context.Context, client *http.Client, service string, port int, expectRunning bool) (Level, string) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		listener.Close()
		if expectRunning {
			return Fail, fmt.Sprintf("127.0.0.1:%d is free; %s is not listening", port, service)
		}
		return OK, fmt.Sprintf("127.0.0.1:%d is free (service not started)", port)
	}
	paths := map[string]string{"calibre": "/", "kavita": "/api/health"}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, paths[service])
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if reqErr == nil {
		if resp, httpErr := client.Do(req); httpErr == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return OK, fmt.Sprintf("127.0.0.1:%d is serving %s", port, service)
			}
		}
	}
	return Warn, fmt.Sprintf("127.0.0.1:%d is in use and does not answer as %s", port, service)
}

func countCalibreBooks(ctx context.Context, runner execx.Runner, sqlite, dbPath string) (int, error) {
	result, err := runner.Run(ctx, sqlite, []string{"-readonly", "-cmd", ".timeout 5000", "-json", dbPath, "SELECT COUNT(*) AS count FROM books"}, execx.Options{})
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(string(result.Stdout))
	if raw == "" {
		return 0, nil
	}
	var rows []struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return 0, fmt.Errorf("decode sqlite book count: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Count, nil
}

func checkBoot(report *Report, cfg *config.Config, deps Deps) {
	if deps.IsNixOS {
		if deps.Runner == nil {
			return
		}
		result, err := deps.Runner.Run(context.Background(), deps.Systemctl, []string{"is-enabled", "celine.target"}, execx.Options{})
		if err != nil || strings.TrimSpace(string(result.Stdout)) != "enabled" {
			report.add("boot units", Warn, "celine.target is not enabled", "enable services.celine in the NixOS configuration and run nixos-rebuild switch")
		} else {
			report.add("boot units", OK, "celine.target is enabled", "")
		}
		return
	}
	servicePath := filepath.Join(deps.SystemdDir, "celine.service")
	timerPath := filepath.Join(deps.SystemdDir, "celine-backup.timer")
	serviceInstalled := fileExists(servicePath)
	if !serviceInstalled {
		report.add("boot units", Warn, "celine.service is not installed; boot start and scheduled backups are inactive", "run scripts/install-boot.sh")
	}
	if cfg.Runtime.Rootless {
		if deps.Runner != nil {
			result, err := deps.Runner.Run(context.Background(), deps.Loginctl, []string{"show-user", deps.User, "-p", "Linger"}, execx.Options{})
			if err != nil || !strings.Contains(string(result.Stdout), "Linger=yes") {
				report.add("boot linger", Warn, "user lingering is disabled; the rootless Podman socket is unavailable before login", "sudo loginctl enable-linger "+deps.User)
			} else {
				report.add("boot linger", OK, "user lingering is enabled", "")
			}
		}
		if !podmanSocketEnabled() {
			report.add("podman socket", Warn, "podman.socket is not enabled for the user", "systemctl --user enable --now podman.socket")
		} else {
			report.add("podman socket", OK, "podman.socket is enabled for the user", "")
		}
	}
	if !serviceInstalled || deps.Runner == nil {
		return
	}
	if result, err := deps.Runner.Run(context.Background(), deps.Systemctl, []string{"is-enabled", "celine.service"}, execx.Options{}); err != nil || strings.TrimSpace(string(result.Stdout)) != "enabled" {
		report.add("boot service", Warn, "celine.service is not enabled", "sudo systemctl enable celine.service")
	} else if result, err := deps.Runner.Run(context.Background(), deps.Systemctl, []string{"is-failed", "celine.service"}, execx.Options{}); err == nil && strings.TrimSpace(string(result.Stdout)) == "failed" {
		report.add("boot service", Fail, "celine.service is in the failed state", "journalctl -u celine.service")
	} else {
		report.add("boot service", OK, "celine.service is enabled", "")
	}
	if cfg.Backup.Enable {
		if !fileExists(timerPath) {
			report.add("backup timer", Warn, "celine-backup.timer is not installed", "run scripts/install-boot.sh")
		} else if result, err := deps.Runner.Run(context.Background(), deps.Systemctl, []string{"is-enabled", "celine-backup.timer"}, execx.Options{}); err != nil || strings.TrimSpace(string(result.Stdout)) != "enabled" {
			report.add("backup timer", Warn, "celine-backup.timer is not enabled", "sudo systemctl enable --now celine-backup.timer")
		} else if result, err := deps.Runner.Run(context.Background(), deps.Systemctl, []string{"is-active", "celine-backup.timer"}, execx.Options{}); err != nil || strings.TrimSpace(string(result.Stdout)) != "active" {
			report.add("backup timer", Warn, "celine-backup.timer is not active", "sudo systemctl restart celine-backup.timer")
		} else {
			report.add("backup timer", OK, "celine-backup.timer is active", "")
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func podmanSocketEnabled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	for _, path := range []string{
		filepath.Join(home, ".config", "systemd", "user", "sockets.target.wants", "podman.socket"),
		"/etc/systemd/user/sockets.target.wants/podman.socket",
	} {
		if fileExists(path) {
			return true
		}
	}
	return false
}

func checkAgeIdentity(report *Report, _ Deps) {
	var paths []string
	for _, name := range []string{"SOPS_AGE_KEY_FILE", "SOPS_AGE_SSH_PRIVATE_KEY_FILE"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			paths = append(paths, value)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "sops", "age", "keys.txt"))
	}
	seen := map[string]bool{}
	var bad []string
	found := false
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		found = true
		if info.Mode().Perm()&0o077 != 0 {
			bad = append(bad, fmt.Sprintf("%s is mode %04o", path, info.Mode().Perm()))
		}
	}
	if !found {
		return
	}
	if len(bad) > 0 {
		report.add("age identity", Fail, strings.Join(bad, "; "), "chmod 600 <identity file>")
		return
	}
	report.add("age identity", OK, "SOPS identity files are not group/world readable", "")
}

func checkSecretPermissions(report *Report, cfg *config.Config) {
	// A protected state parent makes readable children harmless (the native
	// NixOS path keeps /var/lib/celine at 0700).
	stateProtected := false
	if info, err := os.Stat(cfg.Resolved.StateDir); err == nil && info.Mode().Perm()&0o077 == 0 {
		stateProtected = true
	}
	var managed []string
	if !stateProtected {
		if info, err := os.Stat(cfg.Resolved.KavitaConfigDir); err == nil && info.Mode().Perm()&0o077 != 0 {
			managed = append(managed, fmt.Sprintf("%s is mode %04o", cfg.Resolved.KavitaConfigDir, info.Mode().Perm()))
		}
		if info, err := os.Stat(filepath.Join(cfg.Resolved.KavitaConfigDir, "appsettings.json")); err == nil && info.Mode().Perm()&0o077 != 0 {
			managed = append(managed, fmt.Sprintf("%s is mode %04o", filepath.Join(cfg.Resolved.KavitaConfigDir, "appsettings.json"), info.Mode().Perm()))
		}
		if info, err := os.Stat(cfg.Resolved.CalibreConfigDir); err == nil && info.Mode().Perm()&0o077 != 0 {
			managed = append(managed, fmt.Sprintf("%s is mode %04o", cfg.Resolved.CalibreConfigDir, info.Mode().Perm()))
		}
	}
	if len(managed) > 0 {
		report.add("state permissions", Fail, strings.Join(managed, "; "), "run `celine up` to reset managed state modes")
	}

	var legacy []string
	for _, path := range []string{
		filepath.Join(cfg.Root, "config"),
		filepath.Join(cfg.Root, "config.backup"),
		filepath.Join(cfg.Root, "config", "appsettings.json"),
		filepath.Join(cfg.Root, "config.backup", "appsettings.json"),
	} {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			legacy = append(legacy, fmt.Sprintf("%s is mode %04o", path, info.Mode().Perm()))
		}
	}
	if matches, err := filepath.Glob(filepath.Join(cfg.Resolved.BackupDir, "*.tar.gz")); err == nil {
		for _, path := range matches {
			base := filepath.Base(path)
			if !strings.HasPrefix(base, "celine-") && !strings.HasPrefix(base, "legacy-config-") && !strings.HasPrefix(base, "kavita_config_") {
				continue
			}
			if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
				legacy = append(legacy, fmt.Sprintf("%s is mode %04o", path, info.Mode().Perm()))
			}
		}
	}
	if len(legacy) > 0 {
		report.add("legacy permissions", Warn, strings.Join(legacy, "; "), "chmod -R go-rwx config config.backup && chmod 600 backups/*.tar.gz")
	}
}

type mountInfo struct {
	point  string
	source string
	fstype string
}

func lookupMount(path string) (mountInfo, bool) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return mountInfo{}, false
	}
	path = filepath.Clean(path)
	var best mountInfo
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		point := unescapeMount(fields[4])
		separator := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || separator+2 >= len(fields) {
			continue
		}
		if path == point || strings.HasPrefix(path, strings.TrimSuffix(point, "/")+"/") {
			if len(point) >= len(best.point) {
				best = mountInfo{point: point, fstype: fields[separator+1], source: fields[separator+2]}
			}
		}
	}
	if best.point == "" {
		return mountInfo{}, false
	}
	return best, true
}

func unescapeMount(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func freeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func humanBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
