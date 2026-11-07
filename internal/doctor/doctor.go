// Package doctor performs read-only preflight and post-deploy checks.
package doctor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	Name   string
	Level  Level
	Detail string
	Hint   string
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
					missing := 0
					for _, folder := range inventory.FolderPaths {
						hostPath, ok := kavita.HostPath(folder.Path, cfg.Kavita.LibraryMount, cfg.KavitaMediaHostPath())
						if !ok {
							report.add("kavita folder", Warn, "unmapped path "+folder.Path, "the library is outside "+cfg.Kavita.LibraryMount)
							continue
						}
						if info, err := os.Stat(hostPath); err != nil || !info.IsDir() {
							missing++
							report.add("kavita folder", Fail, "missing "+hostPath, "restore the media drive or update the library path")
						}
					}
					if missing == 0 {
						report.add("kavita folders", OK, "all declared library paths resolve", "")
					}
				}
			}
		}
	}

	// Secrets.
	if deps.Secrets != nil {
		if _, err := deps.Secrets.Check(ctx); err != nil {
			report.add("secrets", Fail, err.Error(), "run `secretspec check` and provide KAVITA_TOKEN_KEY")
		} else {
			report.add("secrets", OK, "required SecretSpec values resolve (value-free check)", "")
		}
	}

	// Runtime.
	if deps.Runner != nil {
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
		level, detail := checkPort(ctx, deps.HTTPClient, service, port)
		report.add(service+" port", level, detail, "stop the process using the port or change it in celine.toml")
	}

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
		} else if latest, err := latestArchive(cfg.Resolved.BackupDir); err != nil {
			report.add("backups", Warn, err.Error(), "")
		} else if latest == "" {
			report.add("backups", Warn, "no celine archive yet", "run `celine backup`")
		} else {
			info, _ := os.Stat(latest)
			age := time.Since(info.ModTime())
			if age > 48*time.Hour {
				report.add("backups", Warn, fmt.Sprintf("latest archive is %s old", age.Round(time.Hour)), "run `celine backup` or enable the timer")
			} else {
				report.add("backups", OK, fmt.Sprintf("latest archive is %s old", age.Round(time.Minute)), "")
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

	// Rendered artifacts.
	composeFile := filepath.Join(cfg.Root, "build", "compose.yaml")
	if _, err := os.Stat(composeFile); err != nil {
		report.add("rendered compose", Warn, "missing "+composeFile, "run `celine render`")
	} else {
		report.add("rendered compose", OK, composeFile, "")
	}
	return report
}

func checkPort(ctx context.Context, client *http.Client, service string, port int) (Level, string) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		listener.Close()
		return OK, fmt.Sprintf("127.0.0.1:%d is free", port)
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
	result, err := runner.Run(ctx, sqlite, []string{"-readonly", "-json", dbPath, "SELECT COUNT(*) AS count FROM books"}, execx.Options{})
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(string(result.Stdout))
	if raw == "" {
		return 0, nil
	}
	// The JSON shape is [{"count":N}].
	start := strings.Index(raw, "\"count\":")
	if start < 0 {
		return 0, fmt.Errorf("unexpected sqlite output")
	}
	rest := raw[start+len("\"count\":"):]
	end := strings.IndexAny(rest, "}")
	if end >= 0 {
		rest = rest[:end]
	}
	value, err := strconv.Atoi(strings.TrimSpace(strings.TrimRight(rest, ",")))
	if err != nil {
		return 0, fmt.Errorf("parse book count: %w", err)
	}
	return value, nil
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

func latestArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var newest string
	var newestTime time.Time
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "celine-") || !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newest = filepath.Join(dir, name)
			newestTime = info.ModTime()
		}
	}
	return newest, nil
}
