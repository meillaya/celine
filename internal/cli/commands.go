package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Meillaya/celine/internal/backup"
	"github.com/Meillaya/celine/internal/compose"
	"github.com/Meillaya/celine/internal/config"
	"github.com/Meillaya/celine/internal/doctor"
	"github.com/Meillaya/celine/internal/render"
	"github.com/Meillaya/celine/internal/secrets"
	"github.com/Meillaya/celine/internal/tailnet"
)

func (a App) resolverFor(cfg *config.Config, reason string) *secrets.Resolver {
	resolver := secrets.New(a.Runner, cfg.Resolved.SecretspecPath)
	resolver.Profile = envValue(a, "SECRETSPEC_PROFILE", "default")
	resolver.Provider = envValue(a, "SECRETSPEC_PROVIDER", "")
	resolver.Reason = reason
	resolver.Version = a.Version
	resolver.Secretspec = a.Secretspec
	resolver.LookupEnv = a.LookupEnv
	return resolver
}

func (a App) composeFor(cfg *config.Config) *compose.Client {
	client := compose.New(a.Runner, filepath.Join(cfg.Root, "build", "compose.yaml"))
	client.Docker = a.Docker
	client.Stdout = a.Stdout
	client.Stderr = a.Stderr
	return client
}

func (a App) composeBytes(cfg *config.Config) ([]byte, error) {
	return render.Compose(cfg, a.UID, a.GID)
}

func (a App) ensureStateDirs(cfg *config.Config) error {
	for _, dir := range []string{
		cfg.Resolved.StateDir,
		cfg.Resolved.CalibreConfigDir,
		cfg.Resolved.KavitaConfigDir,
		cfg.Resolved.BackupDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	for _, dir := range []string{cfg.Resolved.CalibreConfigDir, cfg.Resolved.KavitaConfigDir} {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure state directory %s: %w", dir, err)
		}
	}
	return nil
}

func (a App) writeAppSettings(ctx context.Context, cfg *config.Config, reason string) (bool, error) {
	path := filepath.Join(cfg.Resolved.KavitaConfigDir, "appsettings.json")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read existing appsettings.json: %w", err)
	}
	if value, ok := a.LookupEnv(secrets.NameKavitaTokenKey); ok && strings.TrimSpace(value) != "" {
		a.errOut("warning: KAVITA_TOKEN_KEY is set in the environment; it overrides the SOPS store for this command")
	}
	resolver := a.resolverFor(cfg, reason)
	values, err := resolver.Resolve(ctx, secrets.NameKavitaTokenKey)
	if err != nil {
		return false, err
	}
	rendered, err := render.AppSettings(cfg, values[secrets.NameKavitaTokenKey], existing, render.TargetDocker)
	if err != nil {
		return false, err
	}
	if len(existing) > 0 && bytes.Equal(existing, rendered) {
		return false, nil
	}
	return true, writeAtomic(path, rendered, 0o600)
}

func (a App) waitForServices(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	for _, service := range []string{"calibre", "kavita"} {
		if !cfg.ServiceEnabled(service) {
			continue
		}
		port, _ := cfg.UpstreamPort(service)
		path := "/"
		if service == "kavita" {
			path = "/api/health"
		}
		url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
		if err := waitHTTP(ctx, a.HTTPClient, url); err != nil {
			return fmt.Errorf("%s did not become healthy: %w\nhint: run `celine logs %s`", service, err, service)
		}
	}
	return nil
}

func (a App) tailnetEndpoints(ctx context.Context, cfg *config.Config) ([]render.Endpoint, string, error) {
	if !cfg.Tailnet.Enable {
		return nil, "", nil
	}
	self, err := tailnet.ReadSelf(ctx, a.Runner, a.Tailscale)
	if err != nil {
		return nil, "", err
	}
	hostname := tailnet.Hostname(cfg.Tailnet.Hostname, self.DNSName)
	endpoints, err := render.Endpoints(cfg, hostname)
	if err != nil {
		return nil, "", err
	}
	return endpoints, hostname, nil
}

func (a App) applyTailnet(ctx context.Context, cfg *config.Config) error {
	endpoints, _, err := a.tailnetEndpoints(ctx, cfg)
	if err != nil {
		return err
	}
	if len(endpoints) == 0 {
		return nil
	}
	return tailnet.Apply(ctx, a.Runner, a.Tailscale, endpoints)
}

func (a App) printURLs(cfg *config.Config, endpoints []render.Endpoint) {
	published := 0
	for _, endpoint := range endpoints {
		a.out("  %-8s %s", endpoint.Service+":", endpoint.URL())
		published++
	}
	if published == 0 {
		for _, service := range []string{"calibre", "kavita"} {
			if !cfg.ServiceEnabled(service) {
				continue
			}
			port, _ := cfg.UpstreamPort(service)
			a.out("  %-8s http://127.0.0.1:%d", service+":", port)
		}
	}
}

func (a App) cmdValidate(args []string) int {
	set, common := a.newFlagSet("validate")
	noFilesystem := set.Bool("no-filesystem", false, "skip media path checks")
	if code, done := parseFlags(set, args); done {
		return code
	}
	if code, done := noPositional(set, a); done {
		return code
	}
	cfg, err := common.load(a, !*noFilesystem)
	if err != nil {
		return a.fail(err)
	}
	a.out("configuration ok: %s", cfg.Path)
	a.out("  media root: %s", cfg.Resolved.MediaRoot)
	if cfg.Calibre.Enable {
		a.out("  calibre: %d libraries on port %d", len(cfg.Calibre.Libraries), cfg.Calibre.Port)
	}
	if cfg.Kavita.Enable {
		a.out("  kavita: %s on port %d", cfg.Kavita.Image, cfg.Kavita.Port)
	}
	return 0
}

func (a App) cmdRender(args []string) int {
	set, common := a.newFlagSet("render")
	check := set.Bool("check", false, "fail when build/compose.yaml differs")
	outDir := set.String("out", "", "output directory (default: <root>/build)")
	if code, done := parseFlags(set, args); done {
		return code
	}
	if code, done := noPositional(set, a); done {
		return code
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	rendered, err := a.composeBytes(cfg)
	if err != nil {
		return a.fail(err)
	}
	dir := *outDir
	if dir == "" {
		dir = filepath.Join(cfg.Root, "build")
	}
	path := filepath.Join(dir, "compose.yaml")
	existing, readErr := os.ReadFile(path)
	if *check {
		if readErr != nil {
			return a.fail(fmt.Errorf("%s does not exist; run `celine render`", path))
		}
		if !bytes.Equal(existing, rendered) {
			return a.fail(fmt.Errorf("%s is out of date; run `celine render`", path))
		}
		a.out("rendered compose is up to date: %s", path)
		return 0
	}
	if err := writeAtomic(path, rendered, 0o644); err != nil {
		return a.fail(err)
	}
	a.out("rendered %s", path)
	return 0
}

func (a App) cmdUp(args []string) int {
	set, common := a.newFlagSet("up")
	build := set.Bool("build", false, "rebuild the Calibre image before starting")
	pull := set.Bool("pull", false, "pull the Kavita image even when it is already present")
	noTailnet := set.Bool("no-tailnet", false, "do not apply Tailscale Serve")
	if code, done := parseFlags(set, args); done {
		return code
	}
	if code, done := noPositional(set, a); done {
		return code
	}
	cfg, err := common.load(a, true)
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()

	if _, ok := a.LookupEnv("DOCKER_HOST"); !ok {
		a.errOut("warning: DOCKER_HOST is not set; if this is a rootless Podman host, export DOCKER_HOST=unix:///run/user/%d/podman/podman.sock", a.UID)
	}

	if err := a.ensureStateDirs(cfg); err != nil {
		return a.fail(err)
	}
	appSettingsChanged := false
	if cfg.Kavita.Enable {
		appSettingsChanged, err = a.writeAppSettings(ctx, cfg, "celine up")
		if err != nil {
			return a.fail(err)
		}
	}
	rendered, err := a.composeBytes(cfg)
	if err != nil {
		return a.fail(err)
	}
	composePath := filepath.Join(cfg.Root, "build", "compose.yaml")
	if err := writeAtomic(composePath, rendered, 0o644); err != nil {
		return a.fail(err)
	}

	client := a.composeFor(cfg)
	info, err := client.RuntimeInfo(ctx)
	if err != nil {
		return a.fail(fmt.Errorf("container runtime is not reachable: %w\nhint: set DOCKER_HOST to the rootless Podman socket", err))
	}
	a.out("runtime: %s", info)
	if err := client.ConfigQuiet(ctx); err != nil {
		return a.fail(fmt.Errorf("compose file failed validation: %w", err))
	}

	if cfg.Calibre.Enable {
		exists, err := client.ImageExists(ctx, cfg.CalibreImage())
		if err == nil && (!exists || *build) {
			a.out("building %s", cfg.CalibreImage())
			if err := client.Build(ctx, "calibre"); err != nil {
				return a.fail(err)
			}
		}
	}
	if cfg.Kavita.Enable {
		exists, err := client.ImageExists(ctx, cfg.Kavita.Image)
		if err == nil && (*pull || !exists) {
			a.out("pulling %s", cfg.Kavita.Image)
			if err := client.Pull(ctx, "kavita"); err != nil {
				return a.fail(err)
			}
		}
	}

	a.out("starting services")
	if err := client.Up(ctx); err != nil {
		return a.fail(err)
	}
	if cfg.Kavita.Enable {
		// appsettings.json is bind-mounted, not part of the Compose document.
		// Compose therefore leaves the running Kavita process alone when only
		// the settings file changed (token rotation, OIDC, BaseUrl, ...). Compare
		// the file mtime to the container start time and restart only when the
		// running process predates the current settings.
		appSettings := filepath.Join(cfg.Resolved.KavitaConfigDir, "appsettings.json")
		if fileInfo, statErr := os.Stat(appSettings); statErr == nil {
			started, running, startErr := client.StartedAt(ctx, "kavita")
			switch {
			case startErr != nil:
				if appSettingsChanged {
					a.errOut("warning: could not determine Kavita start time (%v); restarting to apply appsettings.json", startErr)
					if err := client.Restart(ctx, "kavita"); err != nil {
						return a.fail(err)
					}
				}
			case running && started.Before(fileInfo.ModTime()):
				a.out("restarting kavita to apply appsettings.json")
				if err := client.Restart(ctx, "kavita"); err != nil {
					return a.fail(err)
				}
			}
		}
	}
	if err := a.waitForServices(ctx, cfg); err != nil {
		return a.fail(err)
	}
	a.out("services are healthy")
	published := false
	if cfg.Tailnet.Enable && !*noTailnet {
		endpoints, _, err := a.tailnetEndpoints(ctx, cfg)
		if err != nil {
			a.errOut("warning: Tailscale Serve was not applied: %v", err)
		} else if err := tailnet.Apply(ctx, a.Runner, a.Tailscale, endpoints); err != nil {
			a.errOut("warning: Tailscale Serve was not applied: %v", err)
		} else {
			a.out("urls:")
			a.printURLs(cfg, endpoints)
			published = true
		}
	}
	if !published {
		a.out("urls (local only):")
		a.printURLs(cfg, nil)
	}
	return 0
}

func (a App) cmdDown(args []string) int {
	set, common := a.newFlagSet("down")
	if code, done := parseFlags(set, args); done {
		return code
	}
	if code, done := noPositional(set, a); done {
		return code
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	if err := a.composeFor(cfg).Down(ctx); err != nil {
		return a.fail(err)
	}
	a.out("services stopped")
	return 0
}

type serviceStatus struct {
	Service   string `json:"service"`
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable"`
	Status    string `json:"status"`
}

type statusReport struct {
	Config         string                 `json:"config"`
	OK             bool                   `json:"ok"`
	Services       []serviceStatus        `json:"services"`
	URLs           []doctor.URL           `json:"urls,omitempty"`
	Tailnet        *tailnet.StatusSummary `json:"tailnet,omitempty"`
	TailnetError   string                 `json:"tailnet_error,omitempty"`
	LatestBackup   string                 `json:"latest_backup,omitempty"`
	Runtime        string                 `json:"runtime,omitempty"`
	ComposeVersion string                 `json:"compose_version,omitempty"`
}

func (a App) cmdStatus(args []string) int {
	set, common := a.newFlagSet("status")
	asJSON := set.Bool("json", false, "print JSON")
	strict := set.Bool("strict", false, "exit 1 when an enabled service is down or Tailscale state is unavailable")
	if code, done := parseFlags(set, args); done {
		return code
	}
	if code, done := noPositional(set, a); done {
		return code
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	report := statusReport{Config: cfg.Path, OK: true}
	for _, service := range []string{"calibre", "kavita"} {
		if !cfg.ServiceEnabled(service) {
			continue
		}
		port, _ := cfg.UpstreamPort(service)
		status := serviceStatus{Service: service, Port: port}
		path := "/"
		if service == "kavita" {
			path = "/api/health"
		}
		detail, reachable := probeHealth(a.HTTPClient, fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
		status.Reachable = reachable
		status.Status = detail
		if !reachable {
			report.OK = false
		}
		report.Services = append(report.Services, status)
	}
	if cfg.Tailnet.Enable {
		summary, readErr := tailnet.ReadStatus(ctx, a.Runner, a.Tailscale)
		if readErr != nil {
			report.TailnetError = readErr.Error()
			report.OK = false
		} else {
			report.Tailnet = &summary
			if endpoints, _, endpointErr := a.tailnetEndpoints(ctx, cfg); endpointErr == nil {
				for _, endpoint := range endpoints {
					report.URLs = append(report.URLs, doctor.URL{Service: endpoint.Service, URL: endpoint.URL()})
				}
			} else if report.TailnetError == "" {
				report.TailnetError = endpointErr.Error()
				report.OK = false
			}
		}
	} else {
		for _, service := range []string{"calibre", "kavita"} {
			if !cfg.ServiceEnabled(service) {
				continue
			}
			port, _ := cfg.UpstreamPort(service)
			report.URLs = append(report.URLs, doctor.URL{Service: service, URL: fmt.Sprintf("http://127.0.0.1:%d", port)})
		}
	}
	if latest, err := backup.Latest(cfg.Resolved.BackupDir); err == nil {
		report.LatestBackup = latest
	}
	client := a.composeFor(cfg)
	if info, err := client.RuntimeInfo(ctx); err == nil {
		report.Runtime = info
	}
	if version, err := client.ComposeVersion(ctx); err == nil {
		report.ComposeVersion = version
	}

	if *asJSON {
		data, err := marshalJSON(report)
		if err != nil {
			return a.fail(err)
		}
		fmt.Fprint(a.Stdout, string(data))
		if *strict && !report.OK {
			return 1
		}
		return 0
	}

	a.out("config: %s", report.Config)
	for _, service := range report.Services {
		state := "down"
		if service.Reachable {
			state = "up"
		}
		a.out("%-8s %s on 127.0.0.1:%d (%s)", service.Service, state, service.Port, service.Status)
	}
	if report.Tailnet != nil {
		a.out("tailnet: %s %s", report.Tailnet.BackendState, report.Tailnet.DNSName)
	}
	if report.TailnetError != "" {
		a.out("tailnet error: %s", report.TailnetError)
	}
	for _, url := range report.URLs {
		a.out("url: %s %s", url.Service, url.URL)
	}
	if report.LatestBackup != "" {
		a.out("latest backup: %s", report.LatestBackup)
	}
	if report.Runtime != "" {
		a.out("runtime: %s; %s", report.Runtime, report.ComposeVersion)
	}
	if !report.OK {
		a.out("status: degraded")
		if *strict {
			return 1
		}
	}
	return 0
}

func (a App) cmdLogs(args []string) int {
	set, common := a.newFlagSet("logs")
	follow := set.Bool("f", false, "follow logs")
	if code, done := parseFlags(set, reorderFlags(set, args)); done {
		return code
	}
	if set.NArg() > 1 {
		a.errOut("usage: celine logs [service] [-f]")
		return 2
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	service := ""
	if set.NArg() > 0 {
		service = set.Arg(0)
	}
	if err := a.composeFor(cfg).Logs(context.Background(), *follow, service); err != nil {
		return a.fail(err)
	}
	return 0
}

// probeHealth is shared with doctor/status tests.
func probeHealth(client *http.Client, url string) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err.Error(), false
	}
	resp, err := client.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Status, resp.StatusCode >= 200 && resp.StatusCode < 400
}
