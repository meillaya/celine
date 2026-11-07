// Package cli implements the celine command surface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/Meillaya/celine/internal/config"
	"github.com/Meillaya/celine/internal/execx"
)

// App holds the process dependencies so commands are testable.
type App struct {
	Version    string
	Stdout     io.Writer
	Stderr     io.Writer
	Stdin      io.Reader
	Runner     execx.Runner
	SQLite     string
	Docker     string
	Tailscale  string
	Sops       string
	Secretspec string
	CelineBin  string
	Now        func() time.Time
	LookupEnv  func(string) (string, bool)
	Workdir    string
	UID        int
	GID        int
	Username   string
	Home       string
	HTTPClient *http.Client
}

func (a App) withDefaults() App {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if a.Stdin == nil {
		a.Stdin = os.Stdin
	}
	if a.Runner == nil {
		a.Runner = execx.OS{}
	}
	if a.SQLite == "" {
		a.SQLite = "sqlite3"
	}
	if a.Docker == "" {
		a.Docker = "docker"
	}
	if a.Tailscale == "" {
		a.Tailscale = "tailscale"
	}
	if a.Sops == "" {
		a.Sops = "sops"
	}
	if a.Secretspec == "" {
		a.Secretspec = "secretspec"
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.LookupEnv == nil {
		a.LookupEnv = os.LookupEnv
	}
	if a.Workdir == "" {
		a.Workdir, _ = os.Getwd()
	}
	if a.UID == 0 && a.GID == 0 {
		a.UID = os.Getuid()
		a.GID = os.Getgid()
	}
	if a.Username == "" || a.Home == "" {
		if current, err := user.Current(); err == nil {
			if a.Username == "" {
				a.Username = current.Username
			}
			if a.Home == "" {
				a.Home = current.HomeDir
			}
		}
	}
	if a.Home == "" {
		a.Home, _ = os.UserHomeDir()
	}
	if a.CelineBin == "" {
		if executable, err := os.Executable(); err == nil {
			a.CelineBin = executable
		}
	}
	if a.HTTPClient == nil {
		a.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return a
}

func (a App) out(format string, values ...any) {
	fmt.Fprintf(a.Stdout, format+"\n", values...)
}

func (a App) errOut(format string, values ...any) {
	fmt.Fprintf(a.Stderr, format+"\n", values...)
}

// Run dispatches one command invocation and returns the process exit code.
func Run(args []string, app App) int {
	app = app.withDefaults()
	if len(args) == 0 {
		app.printUsage()
		return 2
	}
	command := args[0]
	rest := args[1:]
	switch command {
	case "help", "-h", "--help":
		app.printUsage()
		return 0
	case "version", "--version", "-V":
		app.out("celine %s", app.Version)
		return 0
	case "validate":
		return app.cmdValidate(rest)
	case "render":
		return app.cmdRender(rest)
	case "up":
		return app.cmdUp(rest)
	case "down":
		return app.cmdDown(rest)
	case "status":
		return app.cmdStatus(rest)
	case "logs":
		return app.cmdLogs(rest)
	case "tailnet":
		return app.cmdTailnet(rest)
	case "doctor":
		return app.cmdDoctor(rest)
	case "backup":
		return app.cmdBackup(rest)
	case "restore":
		return app.cmdRestore(rest)
	case "migrate":
		return app.cmdMigrate(rest)
	case "secrets":
		return app.cmdSecrets(rest)
	case "systemd":
		return app.cmdSystemd(rest)
	default:
		app.errOut("unknown command %q", command)
		app.printUsage()
		return 2
	}
}

func (a App) printUsage() {
	a.out(`celine - declarative Calibre and Kavita over Tailscale

Usage:
  celine <command> [flags]

Commands:
  validate            Validate celine.toml and the media paths
  render              Render build/compose.yaml (--check for drift)
  up                  Render, build if needed, start services, apply Tailscale
  down                Stop and remove the containers
  status              Show health, URLs, and Tailscale state
  logs                Show container logs (-f to follow)
  tailnet apply       Apply the configured Tailscale Serve endpoints
  tailnet status      Show Tailscale and Serve state
  tailnet down        Remove the configured Tailscale Serve endpoints
  doctor              Run read-only preflight and health checks
  backup              Archive the config state
  restore <archive>   Restore an archive (requires --yes)
  migrate             Import the legacy Kavita config into state
  secrets materialize Resolve one SecretSpec value into a file
  systemd render      Render boot and backup units
  systemd install     Install the rendered units
  version             Print the version

Common flags:
  --config PATH       Path to celine.toml (default: walk up from cwd)
  --root PATH         Treat PATH as the repository root
  --secretspec PATH   Path to secretspec.toml (default: next to celine.toml)

Deploy:
  celine migrate --yes
  celine up --build
  celine tailnet apply
  celine doctor --strict`)
}

type commonFlags struct {
	configPath     string
	root           string
	secretspecPath string
}

func (c *commonFlags) register(set *flag.FlagSet) {
	set.StringVar(&c.configPath, "config", "", "path to celine.toml")
	set.StringVar(&c.root, "root", "", "repository root containing celine.toml")
	set.StringVar(&c.secretspecPath, "secretspec", "", "path to secretspec.toml")
}

func (c *commonFlags) path(app App) (string, error) {
	if c.configPath != "" {
		return filepath.Abs(c.configPath)
	}
	if c.root != "" {
		root, err := filepath.Abs(c.root)
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "celine.toml"), nil
	}
	return config.Find(app.Workdir)
}

func (c *commonFlags) load(app App, filesystem bool) (*config.Config, error) {
	path, err := c.path(app)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if c.secretspecPath != "" {
		abs, err := filepath.Abs(c.secretspecPath)
		if err != nil {
			return nil, err
		}
		cfg.Resolved.SecretspecPath = abs
	}
	opts := config.ValidateOptions{CheckFilesystem: filesystem}
	if err := cfg.Validate(opts); err != nil {
		return nil, err
	}
	return cfg, nil
}

// parseFlags reports whether the command should stop, and with which exit code.
// It treats -h/--help as a successful stop instead of a usage error.
func parseFlags(set *flag.FlagSet, args []string) (int, bool) {
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	return 0, false
}

func (a App) newFlagSet(name string) (*flag.FlagSet, *commonFlags) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(a.Stderr)
	common := &commonFlags{}
	common.register(set)
	return set, common
}

func (a App) fail(err error) int {
	a.errOut("error: %v", err)
	return 1
}

// writeAtomic writes a file with the requested mode through a same-directory
// temporary file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".celine-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func marshalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// waitHTTP polls a URL until it answers or the context expires.
func waitHTTP(ctx context.Context, client *http.Client, url string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var last string
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			if last == "" {
				last = ctx.Err().Error()
			}
			return fmt.Errorf("%s did not become healthy: %s", url, last)
		case <-ticker.C:
		}
	}
}

// reorderFlags lets users put positional arguments before flags, which the
// standard flag package does not allow. valueFlags names flags that consume the
// following argument.
func reorderFlags(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			name := strings.SplitN(arg, "=", 2)[0]
			if valueFlags[name] && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return append(flags, positional...)
}

func envValue(app App, name, fallback string) string {
	if value, ok := app.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
