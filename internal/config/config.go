// Package config loads and validates the single source of truth, celine.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the complete deployment description.
type Config struct {
	Meta    Meta    `toml:"meta"`
	Paths   Paths   `toml:"paths"`
	Runtime Runtime `toml:"runtime"`
	Calibre Calibre `toml:"calibre"`
	Kavita  Kavita  `toml:"kavita"`
	Tailnet Tailnet `toml:"tailnet"`
	Backup  Backup  `toml:"backup"`

	// Derived at load time, never decoded from TOML.
	Path     string   `toml:"-"`
	Root     string   `toml:"-"`
	Resolved Resolved `toml:"-"`
}

// Meta identifies the schema version.
type Meta struct {
	Schema int `toml:"schema"`
}

// Paths holds root directories, relative to the config file unless absolute.
type Paths struct {
	MediaRoot string `toml:"media_root"`
	StateDir  string `toml:"state_dir"`
	BackupDir string `toml:"backup_dir"`
}

// Runtime controls container user mapping and timezone.
type Runtime struct {
	Rootless bool   `toml:"rootless"`
	Timezone string `toml:"timezone"`
}

// Calibre describes the Calibre content server.
type Calibre struct {
	Enable    bool      `toml:"enable"`
	Version   string    `toml:"version"`
	Image     string    `toml:"image"`
	Port      int       `toml:"port"`
	Libraries []Library `toml:"libraries"`
}

// Library is one Calibre library directory.
type Library struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
}

// Kavita describes the Kavita server.
type Kavita struct {
	Enable        bool   `toml:"enable"`
	Image         string `toml:"image"`
	Port          int    `toml:"port"`
	LibrarySubdir string `toml:"library_subdir"`
	LibraryMount  string `toml:"library_mount"`
	Cache         int    `toml:"cache"`
	BaseURL       string `toml:"base_url"`
	AllowIFraming bool   `toml:"allow_iframing"`
}

// Tailnet describes Tailscale Serve exposure.
type Tailnet struct {
	Enable   bool          `toml:"enable"`
	Hostname string        `toml:"hostname"`
	Serve    []ServeTarget `toml:"serve"`
}

// ServeTarget maps one service to an HTTPS port on the tailnet.
type ServeTarget struct {
	Service   string `toml:"service"`
	HTTPSPort int    `toml:"https_port"`
}

// Backup describes archives and retention.
type Backup struct {
	Enable     bool   `toml:"enable"`
	Retention  int    `toml:"retention"`
	OnCalendar string `toml:"on_calendar"`
}

// Resolved holds absolute paths derived from Paths.
type Resolved struct {
	MediaRoot        string
	StateDir         string
	BackupDir        string
	CalibreConfigDir string
	KavitaDataDir    string
	KavitaConfigDir  string
	SecretspecPath   string
}

// Default returns the documented defaults. It is exported for tests and for
// documenting the schema in one place.
func Default() *Config {
	return &Config{
		Meta: Meta{Schema: 1},
		Runtime: Runtime{
			Rootless: true,
		},
		Calibre: Calibre{
			Enable: true,
			Port:   8080,
		},
		Kavita: Kavita{
			Enable:        true,
			Port:          5000,
			LibrarySubdir: "Kavita",
			LibraryMount:  "/data",
			Cache:         75,
			BaseURL:       "/",
		},
		Tailnet: Tailnet{Enable: true},
		Backup: Backup{
			Enable:     true,
			Retention:  10,
			OnCalendar: "daily",
		},
	}
}

// Load reads, strictly decodes, and resolves celine.toml.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg := Default()
	md, err := toml.DecodeFile(abs, cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	if unknown := md.Undecoded(); len(unknown) > 0 {
		keys := make([]string, 0, len(unknown))
		for _, key := range unknown {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("unknown configuration keys in %s: %s", abs, strings.Join(keys, ", "))
	}
	cfg.Path = abs
	cfg.Root = filepath.Dir(abs)
	cfg.resolve()
	return cfg, nil
}

// Find walks up from start until it finds celine.toml.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "celine.toml")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("celine.toml not found from %s", start)
		}
		dir = parent
	}
}

func (c *Config) resolve() {
	c.Resolved.MediaRoot = c.resolvePath(c.Paths.MediaRoot)
	c.Resolved.StateDir = c.resolvePath(c.Paths.StateDir)
	c.Resolved.BackupDir = c.resolvePath(c.Paths.BackupDir)
	c.Resolved.CalibreConfigDir = filepath.Join(c.Resolved.StateDir, "calibre")
	c.Resolved.KavitaDataDir = filepath.Join(c.Resolved.StateDir, "kavita")
	c.Resolved.KavitaConfigDir = filepath.Join(c.Resolved.KavitaDataDir, "config")
	c.Resolved.SecretspecPath = filepath.Join(c.Root, "secretspec.toml")
}

func (c *Config) resolvePath(value string) string {
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(c.Root, value)
}

// SetStateDir overrides the resolved state directory for a command. It is used
// by the NixOS units, which pass an explicit /var/lib path.
func (c *Config) SetStateDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve state directory: %w", err)
	}
	c.Paths.StateDir = abs
	c.Resolved.StateDir = abs
	c.Resolved.CalibreConfigDir = filepath.Join(abs, "calibre")
	c.Resolved.KavitaDataDir = filepath.Join(abs, "kavita")
	c.Resolved.KavitaConfigDir = filepath.Join(c.Resolved.KavitaDataDir, "config")
	return nil
}

// SetBackupDir overrides the resolved backup directory for a command.
func (c *Config) SetBackupDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve backup directory: %w", err)
	}
	c.Paths.BackupDir = abs
	c.Resolved.BackupDir = abs
	return nil
}

// Timezone returns the configured timezone or the host's local zone.
func (c *Config) Timezone() string {
	if c.Runtime.Timezone != "" {
		return c.Runtime.Timezone
	}
	if value := os.Getenv("TZ"); value != "" {
		return value
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if index := strings.Index(target, "zoneinfo/"); index >= 0 {
			return target[index+len("zoneinfo/"):]
		}
	}
	return "UTC"
}

// CalibreImage returns the explicit image override or the pinned build tag.
func (c *Config) CalibreImage() string {
	if c.Calibre.Image != "" {
		return c.Calibre.Image
	}
	return "celine/calibre:" + c.Calibre.Version
}

// CalibreLibraryHostPaths returns absolute host paths in declaration order.
func (c *Config) CalibreLibraryHostPaths() []string {
	paths := make([]string, 0, len(c.Calibre.Libraries))
	for _, library := range c.Calibre.Libraries {
		paths = append(paths, filepath.Join(c.Resolved.MediaRoot, library.Path))
	}
	return paths
}

// CalibreLibraryContainerPaths returns the paths the container sees.
func (c *Config) CalibreLibraryContainerPaths() []string {
	paths := make([]string, 0, len(c.Calibre.Libraries))
	for _, library := range c.Calibre.Libraries {
		paths = append(paths, filepath.Join("/media", library.Path))
	}
	return paths
}

// KavitaMediaHostPath is the host library root.
func (c *Config) KavitaMediaHostPath() string {
	return filepath.Join(c.Resolved.MediaRoot, c.Kavita.LibrarySubdir)
}

// ValidateOptions controls environment-dependent validation.
type ValidateOptions struct {
	// CheckFilesystem verifies the media root and every Calibre metadata.db.
	CheckFilesystem bool
}

// Validate enforces the schema and deployment invariants.
func (c *Config) Validate(opts ValidateOptions) error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Meta.Schema != 1 {
		add("meta.schema must be 1, got %d", c.Meta.Schema)
	}
	if !c.Calibre.Enable && !c.Kavita.Enable {
		add("at least one of calibre.enable or kavita.enable must be true")
	}
	if strings.TrimSpace(c.Paths.MediaRoot) == "" {
		add("paths.media_root must not be empty")
	}
	if strings.TrimSpace(c.Paths.StateDir) == "" {
		add("paths.state_dir must not be empty")
	}

	if c.Calibre.Enable {
		if strings.TrimSpace(c.Calibre.Version) == "" {
			add("calibre.version must not be empty")
		}
		if err := validateLoopbackPort("calibre.port", c.Calibre.Port); err != "" {
			add("%s", err)
		}
		if len(c.Calibre.Libraries) == 0 {
			add("calibre.libraries must declare at least one library")
		}
		seenName := map[string]bool{}
		seenPath := map[string]bool{}
		for i, library := range c.Calibre.Libraries {
			label := fmt.Sprintf("calibre.libraries[%d]", i)
			if strings.TrimSpace(library.Name) == "" {
				add("%s.name must not be empty", label)
			} else if seenName[library.Name] {
				add("%s.name %q is duplicated", label, library.Name)
			}
			seenName[library.Name] = true

			clean, err := safeRelativePath(library.Path)
			if err != nil {
				add("%s.path: %v", label, err)
				continue
			}
			if seenPath[clean] {
				add("%s.path %q is duplicated", label, clean)
			}
			seenPath[clean] = true

			if opts.CheckFilesystem {
				hostPath := filepath.Join(c.Resolved.MediaRoot, clean)
				info, err := os.Stat(filepath.Join(hostPath, "metadata.db"))
				if err != nil {
					add("%s (%s) has no readable metadata.db", label, hostPath)
				} else if info.IsDir() {
					add("%s (%s) metadata.db is a directory", label, hostPath)
				}
			}
		}
	}

	if c.Kavita.Enable {
		if strings.TrimSpace(c.Kavita.Image) == "" {
			add("kavita.image must not be empty")
		}
		if err := validateLoopbackPort("kavita.port", c.Kavita.Port); err != "" {
			add("%s", err)
		}
		if _, err := safeRelativePath(c.Kavita.LibrarySubdir); err != nil {
			add("kavita.library_subdir: %v", err)
		}
		if !filepath.IsAbs(c.Kavita.LibraryMount) {
			add("kavita.library_mount must be absolute, got %q", c.Kavita.LibraryMount)
		}
		if c.Kavita.Cache < 0 {
			add("kavita.cache must not be negative")
		}
		if !strings.HasPrefix(c.Kavita.BaseURL, "/") {
			add("kavita.base_url must start with /, got %q", c.Kavita.BaseURL)
		}
	}

	if c.Tailnet.Enable {
		if len(c.Tailnet.Serve) == 0 {
			add("tailnet.serve must declare at least one target when tailnet.enable is true")
		}
		seenPort := map[int]bool{}
		seenService := map[string]bool{}
		for i, target := range c.Tailnet.Serve {
			label := fmt.Sprintf("tailnet.serve[%d]", i)
			switch target.Service {
			case "calibre":
				if !c.Calibre.Enable {
					add("%s references disabled service calibre", label)
				}
			case "kavita":
				if !c.Kavita.Enable {
					add("%s references disabled service kavita", label)
				}
			default:
				add("%s.service must be calibre or kavita, got %q", label, target.Service)
			}
			if seenService[target.Service] {
				add("%s.service %q is exposed more than once", label, target.Service)
			}
			seenService[target.Service] = true
			switch target.HTTPSPort {
			case 443, 8443, 10000:
			default:
				add("%s.https_port must be 443, 8443, or 10000, got %d", label, target.HTTPSPort)
			}
			if seenPort[target.HTTPSPort] {
				add("%s.https_port %d is already used", label, target.HTTPSPort)
			}
			seenPort[target.HTTPSPort] = true
		}
		if strings.Contains(c.Tailnet.Hostname, "/") || strings.Contains(c.Tailnet.Hostname, ":") {
			add("tailnet.hostname must be a bare DNS name, got %q", c.Tailnet.Hostname)
		}
	}

	if c.Backup.Enable {
		if c.Backup.Retention < 1 {
			add("backup.retention must be at least 1")
		}
		if strings.TrimSpace(c.Backup.OnCalendar) == "" {
			add("backup.on_calendar must not be empty")
		}
	}

	if _, err := os.Stat(c.Resolved.SecretspecPath); err != nil {
		add("secretspec.toml is missing next to celine.toml (%s)", c.Resolved.SecretspecPath)
	}

	if opts.CheckFilesystem {
		info, err := os.Stat(c.Resolved.MediaRoot)
		switch {
		case err != nil:
			add("paths.media_root is not readable: %v", err)
		case !info.IsDir():
			add("paths.media_root is not a directory: %s", c.Resolved.MediaRoot)
		}
		if c.Kavita.Enable {
			if info, err := os.Stat(c.KavitaMediaHostPath()); err != nil || !info.IsDir() {
				add("kavita library root is missing: %s", c.KavitaMediaHostPath())
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// UpstreamPort returns the loopback port for a served service.
func (c *Config) UpstreamPort(service string) (int, error) {
	switch service {
	case "calibre":
		return c.Calibre.Port, nil
	case "kavita":
		return c.Kavita.Port, nil
	default:
		return 0, fmt.Errorf("unknown service %q", service)
	}
}

// ServiceEnabled reports whether a service is enabled.
func (c *Config) ServiceEnabled(service string) bool {
	switch service {
	case "calibre":
		return c.Calibre.Enable
	case "kavita":
		return c.Kavita.Enable
	default:
		return false
	}
}

func validateLoopbackPort(label string, port int) string {
	if port < 1024 || port > 65535 {
		return fmt.Sprintf("%s must be between 1024 and 65535, got %d", label, port)
	}
	return ""
}

func safeRelativePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("must not be empty")
	}
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("must be relative, got absolute path %q", value)
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("must stay inside the media root, got %q", value)
	}
	return clean, nil
}
