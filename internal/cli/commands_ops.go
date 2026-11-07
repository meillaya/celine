package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Meillaya/celine/internal/archiveutil"
	"github.com/Meillaya/celine/internal/backup"
	"github.com/Meillaya/celine/internal/doctor"
	"github.com/Meillaya/celine/internal/migrate"
	"github.com/Meillaya/celine/internal/render"
	"github.com/Meillaya/celine/internal/secrets"
	"github.com/Meillaya/celine/internal/tailnet"
)

func (a App) cmdTailnet(args []string) int {
	if len(args) == 0 {
		a.errOut("usage: celine tailnet apply|status|down [flags]")
		return 2
	}
	sub := args[0]
	set, common := a.newFlagSet("tailnet " + sub)
	if code, done := parseFlags(set, args[1:]); done {
		return code
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	if !cfg.Tailnet.Enable {
		a.out("tailnet exposure is disabled in celine.toml")
		return 0
	}
	ctx := context.Background()
	switch sub {
	case "apply":
		endpoints, hostname, err := a.tailnetEndpoints(ctx, cfg)
		if err != nil {
			return a.fail(err)
		}
		if len(endpoints) == 0 {
			a.out("no tailnet endpoints configured")
			return 0
		}
		if err := tailnet.Apply(ctx, a.Runner, a.Tailscale, endpoints); err != nil {
			return a.fail(err)
		}
		a.out("tailnet serve applied on %s:", hostname)
		a.printURLs(cfg, endpoints)
		return 0
	case "status":
		summary, err := tailnet.ReadStatus(ctx, a.Runner, a.Tailscale)
		if err != nil {
			return a.fail(err)
		}
		a.out("backend: %s", summary.BackendState)
		a.out("node: %s", summary.DNSName)
		a.out("certificate domains: %s", strings.Join(summary.CertDomains, ", "))
		a.out("serve: %s", summary.Serve)
		if endpoints, _, err := a.tailnetEndpoints(ctx, cfg); err == nil {
			a.printURLs(cfg, endpoints)
		}
		return 0
	case "down":
		endpoints, _, err := a.tailnetEndpoints(ctx, cfg)
		if err != nil {
			return a.fail(err)
		}
		if err := tailnet.Clear(ctx, a.Runner, a.Tailscale, endpoints); err != nil {
			return a.fail(err)
		}
		a.out("removed celine tailnet handlers")
		return 0
	default:
		a.errOut("unknown tailnet command %q (use apply, status, or down)", sub)
		return 2
	}
}

func (a App) cmdDoctor(args []string) int {
	set, common := a.newFlagSet("doctor")
	strict := set.Bool("strict", false, "treat warnings as failures")
	asJSON := set.Bool("json", false, "print JSON")
	if code, done := parseFlags(set, args); done {
		return code
	}
	cfg, cfgErr := common.load(a, false)
	if cfgErr != nil {
		if *asJSON {
			data, _ := marshalJSON(map[string]any{"ok": false, "error": cfgErr.Error()})
			fmt.Fprint(a.Stdout, string(data))
		} else {
			a.errOut("[fail] configuration: %v", cfgErr)
		}
		return 1
	}
	report := doctor.Run(context.Background(), doctor.Deps{
		ConfigPath: cfg.Path,
		Config:     cfg,
		Runner:     a.Runner,
		Secrets:    a.resolverFor(cfg, "celine doctor"),
		SQLite:     a.SQLite,
		Docker:     a.Docker,
		Tailscale:  a.Tailscale,
		Version:    a.Version,
		Now:        a.Now,
		HTTPClient: a.HTTPClient,
	})
	if *asJSON {
		data, err := marshalJSON(map[string]any{
			"ok":     report.IsClean(*strict),
			"checks": report.Checks,
			"urls":   report.URLs,
		})
		if err != nil {
			return a.fail(err)
		}
		fmt.Fprint(a.Stdout, string(data))
	} else {
		for _, check := range report.Checks {
			a.out("[%s] %s: %s", check.Level, check.Name, check.Detail)
			if check.Hint != "" && check.Level != doctor.OK {
				a.out("       hint: %s", check.Hint)
			}
		}
		if len(report.URLs) > 0 {
			a.out("")
			a.out("urls:")
			for _, item := range report.URLs {
				a.out("  %-8s %s", item.Service+":", item.URL)
			}
		}
		counts := report.Counts()
		a.out("")
		a.out("checks: %d ok, %d warn, %d fail", counts[doctor.OK], counts[doctor.Warn], counts[doctor.Fail])
	}
	if !report.IsClean(*strict) {
		return 1
	}
	return 0
}

func (a App) cmdBackup(args []string) int {
	set, common := a.newFlagSet("backup")
	dryRun := set.Bool("dry-run", false, "print the plan without writing an archive")
	stateDir := set.String("state-dir", "", "override the state directory")
	backupDir := set.String("backup-dir", "", "override the backup directory")
	retention := set.Int("retention", 0, "override the archive retention count")
	if code, done := parseFlags(set, args); done {
		return code
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	if *stateDir != "" {
		if err := cfg.SetStateDir(*stateDir); err != nil {
			return a.fail(err)
		}
	}
	if *backupDir != "" {
		if err := cfg.SetBackupDir(*backupDir); err != nil {
			return a.fail(err)
		}
	}
	if *retention > 0 {
		cfg.Backup.Retention = *retention
	}
	dbPath := filepath.Join(cfg.Resolved.KavitaConfigDir, "kavita.db")
	if *dryRun {
		a.out("would archive:")
		a.out("  %s", dbPath)
		a.out("  %s", filepath.Join(cfg.Resolved.KavitaConfigDir, "appsettings.json"))
		a.out("  bookmarks and Calibre config")
		a.out("  into %s (retention %d)", cfg.Resolved.BackupDir, cfg.Backup.Retention)
		return 0
	}
	result, err := backup.Run(context.Background(), backup.Options{
		KavitaConfigDir:  cfg.Resolved.KavitaConfigDir,
		CalibreConfigDir: cfg.Resolved.CalibreConfigDir,
		BackupDir:        cfg.Resolved.BackupDir,
		ConfigPath:       cfg.Path,
		Retention:        cfg.Backup.Retention,
		ToolVersion:      a.Version,
		Runner:           a.Runner,
		SQLite:           a.SQLite,
		Now:              a.Now,
	})
	if err != nil {
		return a.fail(err)
	}
	a.out("wrote %s", result.Archive)
	a.out("  files: %d", result.Files)
	a.out("  size:  %.1f MiB", float64(result.Bytes)/(1<<20))
	return 0
}

func (a App) cmdRestore(args []string) int {
	set, common := a.newFlagSet("restore")
	yes := set.Bool("yes", false, "confirm the restore")
	dryRun := set.Bool("dry-run", false, "verify the archive without writing")
	force := set.Bool("force", false, "overwrite the existing state")
	stateDir := set.String("state-dir", "", "override the state directory")
	if code, done := parseFlags(set, reorderFlags(args, map[string]bool{"--state-dir": true, "-state-dir": true})); done {
		return code
	}
	if set.NArg() != 1 {
		a.errOut("usage: celine restore [flags] <archive>")
		return 2
	}
	archive := set.Arg(0)
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	if *stateDir != "" {
		if err := cfg.SetStateDir(*stateDir); err != nil {
			return a.fail(err)
		}
	}
	entries, err := archiveutil.ListTarGz(archive)
	if err != nil {
		return a.fail(fmt.Errorf("read archive: %w", err))
	}
	if *dryRun {
		a.out("archive: %s", archive)
		a.out("entries: %d", len(entries))
		a.out("target:  %s", cfg.Resolved.StateDir)
		a.out("would restore; rerun with --yes to write")
		return 0
	}
	if !*yes {
		return a.fail(fmt.Errorf("restore is destructive; rerun with --yes"))
	}
	ctx := context.Background()
	// Only touch the live stack when restoring the configured state directory.
	// A --state-dir override targets a separate copy.
	live := *stateDir == ""
	if live {
		composePath := filepath.Join(cfg.Root, "build", "compose.yaml")
		if _, err := os.Stat(composePath); err == nil {
			a.out("stopping services for restore")
			if err := a.composeFor(cfg).Down(ctx); err != nil {
				a.errOut("warning: could not stop services: %v", err)
			}
		}
		if _, err := os.Stat(filepath.Join(cfg.Resolved.KavitaConfigDir, "kavita.db")); err == nil {
			a.out("taking a pre-restore backup")
			if _, err := backup.Run(ctx, backup.Options{
				KavitaConfigDir:  cfg.Resolved.KavitaConfigDir,
				CalibreConfigDir: cfg.Resolved.CalibreConfigDir,
				BackupDir:        cfg.Resolved.BackupDir,
				ConfigPath:       cfg.Path,
				Retention:        cfg.Backup.Retention,
				ToolVersion:      a.Version,
				Runner:           a.Runner,
				SQLite:           a.SQLite,
				Now:              a.Now,
			}); err != nil {
				a.errOut("warning: pre-restore backup failed: %v", err)
			}
		}
	}
	result, err := backup.Restore(ctx, backup.RestoreOptions{
		Archive:  archive,
		StateDir: cfg.Resolved.StateDir,
		// --yes is the destructive confirmation; a pre-restore archive was
		// already written above.
		Force: *force || *yes,
	})
	if err != nil {
		return a.fail(err)
	}
	a.out("restored %d files into %s", result.Files, cfg.Resolved.StateDir)
	a.out("archive created at %s", result.Manifest.CreatedAt)
	a.out("start services with `celine up`")
	return 0
}

func (a App) cmdMigrate(args []string) int {
	set, common := a.newFlagSet("migrate")
	yes := set.Bool("yes", false, "confirm the migration")
	force := set.Bool("force", false, "replace an existing state database")
	noSecret := set.Bool("no-secret-import", false, "let SecretSpec generate a new signing key")
	stateDir := set.String("state-dir", "", "override the state directory")
	backupDir := set.String("backup-dir", "", "override the backup directory")
	secretFile := set.String("secret-file", "", "path to the SOPS secret store")
	if code, done := parseFlags(set, args); done {
		return code
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	if *stateDir != "" {
		if err := cfg.SetStateDir(*stateDir); err != nil {
			return a.fail(err)
		}
	}
	if *backupDir != "" {
		if err := cfg.SetBackupDir(*backupDir); err != nil {
			return a.fail(err)
		}
	}
	if !*yes {
		return a.fail(fmt.Errorf("migration copies the legacy config and writes a secret file; rerun with --yes"))
	}
	secretsPath := *secretFile
	if secretsPath == "" {
		secretsPath = filepath.Join(cfg.Root, "secrets", "celine.enc.yaml")
	}
	if err := os.MkdirAll(cfg.Resolved.StateDir, 0o755); err != nil {
		return a.fail(err)
	}
	result, err := migrate.Run(context.Background(), migrate.Options{
		Root:            cfg.Root,
		LegacyConfigDir: filepath.Join(cfg.Root, "config"),
		StateDir:        cfg.Resolved.StateDir,
		BackupDir:       cfg.Resolved.BackupDir,
		SecretFile:      secretsPath,
		Runner:          a.Runner,
		SQLite:          a.SQLite,
		Sops:            a.Sops,
		ToolVersion:     a.Version,
		Force:           *force,
		ImportToken:     !*noSecret,
		Now:             a.Now,
	})
	if err != nil {
		return a.fail(err)
	}
	a.out("migration complete")
	a.out("  legacy backup: %s", result.BackupArchive)
	a.out("  state config:  %s", result.StateConfigDir)
	a.out("  kavita:        %d users, %d libraries, version %s", result.KavitaUsers, result.KavitaLibraries, result.KavitaVersion)
	if result.TokenImported {
		a.out("  signing key:   imported into SecretSpec/SOPS")
	} else if result.TokenPresent {
		a.out("  signing key:   left for SecretSpec to generate")
	}
	for _, discovery := range result.Discoveries {
		a.out("  note:          %s", discovery)
	}
	a.out("next: celine up --build")
	return 0
}

func (a App) cmdSecrets(args []string) int {
	if len(args) == 0 || args[0] != "materialize" {
		a.errOut("usage: celine secrets materialize --name NAME --out FILE [flags]")
		return 2
	}
	set, common := a.newFlagSet("secrets materialize")
	name := set.String("name", secrets.NameKavitaTokenKey, "secret name")
	out := set.String("out", "", "output file")
	profile := set.String("profile", "", "SecretSpec profile")
	provider := set.String("provider", "", "SecretSpec provider")
	if code, done := parseFlags(set, args[1:]); done {
		return code
	}
	if *out == "" {
		a.errOut("usage: celine secrets materialize --name NAME --out FILE")
		return 2
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	resolver := a.resolverFor(cfg, "celine secrets materialize")
	if *profile != "" {
		resolver.Profile = *profile
	}
	if *provider != "" {
		resolver.Provider = *provider
	}
	if err := resolver.Materialize(context.Background(), *name, *out); err != nil {
		return a.fail(err)
	}
	a.out("wrote %s (mode 0600)", *out)
	return 0
}

func (a App) cmdSystemd(args []string) int {
	if len(args) == 0 {
		a.errOut("usage: celine systemd render|install [flags]")
		return 2
	}
	sub := args[0]
	set, common := a.newFlagSet("systemd " + sub)
	outDir := set.String("out", "", "directory for rendered units")
	systemDir := set.String("dir", "/etc/systemd/system", "install target directory")
	celineBin := set.String("celine", "", "binary path to embed in the generated units")
	if code, done := parseFlags(set, args[1:]); done {
		return code
	}
	cfg, err := common.load(a, false)
	if err != nil {
		return a.fail(err)
	}
	dockerHost := envValue(a, "DOCKER_HOST", "")
	if dockerHost == "" && cfg.Runtime.Rootless {
		dockerHost = fmt.Sprintf("unix:///run/user/%d/podman/podman.sock", a.UID)
	}
	bin := a.CelineBin
	if *celineBin != "" {
		bin, err = filepath.Abs(*celineBin)
		if err != nil {
			return a.fail(err)
		}
	}
	units, err := render.SystemdUnits(render.SystemdOptions{
		CelineBin:  bin,
		ConfigPath: cfg.Path,
		User:       a.Username,
		Group:      a.Username,
		Home:       a.Home,
		DockerHost: dockerHost,
		MediaRoot:  cfg.Resolved.MediaRoot,
		StateDir:   cfg.Resolved.StateDir,
		BackupDir:  cfg.Resolved.BackupDir,
		OnCalendar: cfg.Backup.OnCalendar,
		PathEnv:    unitPathEnv(),
	})
	if err != nil {
		return a.fail(err)
	}
	switch sub {
	case "render":
		dir := *outDir
		if dir == "" {
			dir = filepath.Join(cfg.Root, "build", "systemd")
		}
		for name, content := range units {
			path := filepath.Join(dir, name)
			if err := writeAtomic(path, []byte(content), 0o644); err != nil {
				return a.fail(err)
			}
			a.out("rendered %s", path)
		}
		a.out("install with: sudo install -m644 %s/*.service %s/*.timer %s/ && sudo systemctl daemon-reload && sudo systemctl enable --now celine.service celine-backup.timer", dir, dir, *systemDir)
		return 0
	case "install":
		for name, content := range units {
			path := filepath.Join(*systemDir, name)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				a.errOut("error: install %s: %v", path, err)
				a.errOut("hint: rerun `celine systemd render` and install the files with sudo")
				return 1
			}
			a.out("installed %s", path)
		}
		a.out("run: sudo systemctl daemon-reload && sudo systemctl enable --now celine.service celine-backup.timer")
		return 0
	default:
		a.errOut("unknown systemd command %q (use render or install)", sub)
		return 2
	}
}

// unitPathEnv builds a compact PATH for the boot units from the directories
// that actually provide the runtime tools. A shell PATH is often long and
// contains transient entries that do not exist after a reboot.
func unitPathEnv() string {
	seen := map[string]bool{}
	dirs := make([]string, 0, 10)
	add := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".nix-profile", "bin"))
	}
	// Prefer stable system/profile directories so the units survive a store
	// garbage collection or a dev-shell exit.
	for _, dir := range []string{
		"/run/current-system/sw/bin",
		"/run/wrappers/bin",
		"/run/system-manager/sw/bin",
		"/usr/local/sbin",
		"/usr/local/bin",
		"/usr/sbin",
		"/usr/bin",
		"/sbin",
		"/bin",
	} {
		add(dir)
	}
	for _, name := range []string{"docker", "tailscale", "secretspec", "sops", "sqlite3"} {
		if path, err := exec.LookPath(name); err == nil {
			add(filepath.Dir(path))
		}
	}
	return strings.Join(dirs, ":")
}
