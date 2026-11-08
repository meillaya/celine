package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Meillaya/celine/internal/backup"
	"github.com/Meillaya/celine/internal/doctor"
	"github.com/Meillaya/celine/internal/migrate"
	"github.com/Meillaya/celine/internal/render"
	"github.com/Meillaya/celine/internal/secrets"
	"github.com/Meillaya/celine/internal/tailnet"
)

func (a App) cmdTailnet(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		a.errOut("usage: celine tailnet apply|status|down [flags]")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub := args[0]
	set, common := a.newFlagSet("tailnet " + sub)
	if code, done := parseFlags(set, args[1:]); done {
		return code
	}
	if code, done := noPositional(set, a); done {
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
	expectRunning := set.Bool("expect-running", false, "treat stopped services and missing Serve handlers as failures")
	if code, done := parseFlags(set, args); done {
		return code
	}
	if code, done := noPositional(set, a); done {
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
		ConfigPath:    cfg.Path,
		Config:        cfg,
		Runner:        a.Runner,
		Secrets:       a.resolverFor(cfg, "celine doctor"),
		SQLite:        a.SQLite,
		Docker:        a.Docker,
		Tailscale:     a.Tailscale,
		Version:       a.Version,
		Now:           a.Now,
		HTTPClient:    a.HTTPClient,
		ExpectRunning: *expectRunning,
		UID:           a.UID,
		GID:           a.GID,
		User:          a.Username,
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
	if code, done := noPositional(set, a); done {
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
	if !cfg.Kavita.Enable && !cfg.Calibre.Enable {
		return a.fail(fmt.Errorf("no services are enabled"))
	}
	if *dryRun {
		a.out("would archive:")
		if cfg.Kavita.Enable {
			a.out("  %s", filepath.Join(cfg.Resolved.KavitaConfigDir, "kavita.db"))
			a.out("  %s", filepath.Join(cfg.Resolved.KavitaConfigDir, "appsettings.json"))
			a.out("  bookmarks (Kavita signing key and nested secrets redacted)")
		}
		if cfg.Calibre.Enable {
			a.out("  %s (caches and logs excluded)", cfg.Resolved.CalibreConfigDir)
		}
		a.out("  into %s (retention %d)", cfg.Resolved.BackupDir, cfg.Backup.Retention)
		return 0
	}
	result, err := backup.Run(context.Background(), backup.Options{
		KavitaConfigDir:  cfg.Resolved.KavitaConfigDir,
		CalibreConfigDir: cfg.Resolved.CalibreConfigDir,
		BackupDir:        cfg.Resolved.BackupDir,
		ConfigPath:       cfg.Path,
		KavitaEnabled:    cfg.Kavita.Enable,
		CalibreEnabled:   cfg.Calibre.Enable,
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
	if len(result.Redacted) > 0 {
		a.out("  redacted: %s", strings.Join(result.Redacted, ", "))
	}
	return 0
}

func (a App) cmdRestore(args []string) int {
	set, common := a.newFlagSet("restore")
	yes := set.Bool("yes", false, "confirm the restore")
	dryRun := set.Bool("dry-run", false, "verify the archive without writing")
	force := set.Bool("force", false, "accepted for compatibility; --yes permits overwriting the state")
	stateDir := set.String("state-dir", "", "override the state directory")
	if code, done := parseFlags(set, reorderFlags(set, args)); done {
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
	configuredState := cfg.Resolved.StateDir
	live := true
	if *stateDir != "" {
		live = samePath(*stateDir, configuredState)
		if err := cfg.SetStateDir(*stateDir); err != nil {
			return a.fail(err)
		}
	}
	ctx := context.Background()
	verify, err := backup.Verify(ctx, backup.VerifyOptions{
		Archive:         archive,
		Runner:          a.Runner,
		SQLite:          a.SQLite,
		RequireKavitaDB: cfg.Kavita.Enable,
	})
	if err != nil {
		return a.fail(err)
	}
	if verify.SecretsPresent {
		a.errOut("warning: archive still contains plaintext secret values (pre-redaction archive); `celine up` replaces the signing key after restore, but rotate/delete old archives and re-enter any OIDC client secret")
	}
	if *dryRun {
		a.out("archive: %s", archive)
		a.out("files:   %d", verify.Files)
		a.out("created: %s", verify.Manifest.CreatedAt)
		a.out("kavita:  %d users, %d libraries, DB version %s", verify.Manifest.KavitaUsers, verify.Manifest.KavitaLibraries, verify.Manifest.KavitaVersion)
		a.out("target:  %s", cfg.Resolved.StateDir)
		a.out("verified; rerun with --yes to write")
		return 0
	}
	if !*yes {
		return a.fail(fmt.Errorf("restore is destructive; rerun with --yes"))
	}
	if *force {
		a.errOut("hint: --force is accepted for compatibility; --yes already permits overwriting the state")
	}

	var preRestore string
	if live {
		composePath := filepath.Join(cfg.Root, "build", "compose.yaml")
		dbPath := filepath.Join(cfg.Resolved.KavitaConfigDir, "kavita.db")
		_, dbErr := os.Stat(dbPath)
		calibreHasState := cfg.Calibre.Enable && dirHasFiles(cfg.Resolved.CalibreConfigDir)
		hasState := dbErr == nil || calibreHasState
		if _, err := os.Stat(composePath); err == nil {
			running, err := a.composeFor(cfg).Running(ctx)
			if err != nil {
				return a.fail(fmt.Errorf("cannot determine whether the services are running: %w", err))
			}
			if running {
				a.out("stopping services for restore")
				if err := a.composeFor(cfg).Down(ctx); err != nil {
					return a.fail(fmt.Errorf("refusing to restore over running services: %w", err))
				}
			}
		} else if hasState {
			return a.fail(fmt.Errorf("refusing to restore over live state without the rendered Compose file; run `celine render` or `celine down` first"))
		}
		kavitaBackup := cfg.Kavita.Enable && dbErr == nil
		if kavitaBackup || calibreHasState {
			a.out("taking a pre-restore backup")
			result, err := backup.Run(ctx, backup.Options{
				KavitaConfigDir:  cfg.Resolved.KavitaConfigDir,
				CalibreConfigDir: cfg.Resolved.CalibreConfigDir,
				BackupDir:        cfg.Resolved.BackupDir,
				ConfigPath:       cfg.Path,
				KavitaEnabled:    kavitaBackup,
				CalibreEnabled:   calibreHasState,
				Retention:        cfg.Backup.Retention,
				// Never prune while the archive being restored is still needed.
				SkipPrune:   true,
				ToolVersion: a.Version,
				Runner:      a.Runner,
				SQLite:      a.SQLite,
				Now:         a.Now,
			})
			if err != nil {
				return a.fail(fmt.Errorf("refusing to restore without a pre-restore backup: %w", err))
			}
			preRestore = result.Archive
			a.out("pre-restore backup: %s", preRestore)
		}
	}
	result, err := backup.Restore(ctx, backup.RestoreOptions{
		Archive:         archive,
		StateDir:        cfg.Resolved.StateDir,
		Force:           true,
		RequireKavitaDB: cfg.Kavita.Enable,
		Runner:          a.Runner,
		SQLite:          a.SQLite,
	})
	if err != nil {
		return a.fail(err)
	}
	if preRestore != "" {
		if err := backup.Prune(cfg.Resolved.BackupDir, cfg.Backup.Retention, preRestore); err != nil {
			a.errOut("warning: restore succeeded but pruning old archives failed: %v", err)
		}
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
	stopServices := set.Bool("stop-services", false, "stop the running Celine stack before replacing live state")
	stateDir := set.String("state-dir", "", "override the state directory")
	backupDir := set.String("backup-dir", "", "override the backup directory")
	secretFile := set.String("secret-file", "", "path to the SOPS secret store")
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
	configuredState := cfg.Resolved.StateDir
	live := true
	if *stateDir != "" {
		live = samePath(*stateDir, configuredState)
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
		return a.fail(fmt.Errorf("migration copies the legacy config and may write a secret file; rerun with --yes"))
	}
	ctx := context.Background()
	if live && !*force {
		targetDir := filepath.Join(cfg.Resolved.StateDir, "kavita", "config")
		if dirHasFiles(targetDir) {
			return a.fail(fmt.Errorf("state already contains files at %s\nrerun with --force to replace it, or use --state-dir for a copy", targetDir))
		}
	}
	if live {
		composePath := filepath.Join(cfg.Root, "build", "compose.yaml")
		dbPath := filepath.Join(cfg.Resolved.KavitaConfigDir, "kavita.db")
		_, dbErr := os.Stat(dbPath)
		if _, err := os.Stat(composePath); err == nil {
			client := a.composeFor(cfg)
			running, err := client.Running(ctx)
			if err != nil {
				return a.fail(fmt.Errorf("cannot determine whether the services are running: %w\nhint: stop the stack with `celine down` and retry", err))
			}
			if running {
				if !*stopServices {
					return a.fail(fmt.Errorf("refusing to replace state under a running stack; run `celine down` first or pass --stop-services"))
				}
				a.out("stopping services for migration")
				if err := client.Down(ctx); err != nil {
					return a.fail(err)
				}
			}
		} else if dbErr == nil {
			return a.fail(fmt.Errorf("refusing to replace live state without the rendered Compose file; run `celine render` or `celine down` first"))
		}
	}
	secretsPath := *secretFile
	if secretsPath == "" {
		secretsPath = filepath.Join(cfg.Root, "secrets", "celine.enc.yaml")
	} else if abs, err := filepath.Abs(secretsPath); err == nil {
		secretsPath = abs
	}
	if err := os.MkdirAll(cfg.Resolved.StateDir, 0o755); err != nil {
		return a.fail(err)
	}
	result, err := migrate.Run(ctx, migrate.Options{
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
	switch {
	case result.TokenImported:
		a.out("  signing key:   imported into SecretSpec/SOPS")
	case result.TokenMatches != nil && *result.TokenMatches:
		a.out("  signing key:   existing SOPS store already has this key")
	case result.TokenMatches != nil:
		a.out("  signing key:   existing SOPS store has a different key (left unchanged)")
	case *noSecret:
		a.out("  signing key:   skipped (--no-secret-import)")
	case result.TokenPresent:
		a.out("  signing key:   left for SecretSpec to generate")
	default:
		a.out("  signing key:   missing; SecretSpec will generate one on first start")
	}
	for _, discovery := range result.Discoveries {
		a.out("  note:          %s", discovery)
	}
	a.out("next: celine up --build")
	return 0
}

func (a App) cmdSecrets(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		a.errOut("usage: celine secrets materialize --name NAME --out FILE [flags]")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "materialize" {
		a.errOut("unknown secrets command %q (use materialize)", args[0])
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
	if code, done := noPositional(set, a); done {
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
	if value, ok := a.LookupEnv(secrets.NameKavitaTokenKey); ok && strings.TrimSpace(value) != "" {
		a.errOut("warning: KAVITA_TOKEN_KEY is set in the environment; it overrides the SOPS store for this command")
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
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		a.errOut("usage: celine systemd render|install [flags]")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub := args[0]
	set, common := a.newFlagSet("systemd " + sub)
	outDir := set.String("out", "", "directory for rendered units")
	systemDir := set.String("dir", "/etc/systemd/system", "install target directory")
	celineBin := set.String("celine", "", "binary path to embed in the generated units")
	if code, done := parseFlags(set, args[1:]); done {
		return code
	}
	if code, done := noPositional(set, a); done {
		return code
	}
	isRoot := os.Geteuid() == 0
	if a.IsRoot != nil {
		isRoot = *a.IsRoot
	}
	if isRoot {
		return a.fail(fmt.Errorf("run celine systemd as the operator account, not root; the units must carry your uid, HOME, and DOCKER_HOST"))
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
	if bin == "" {
		return a.fail(fmt.Errorf("cannot determine the celine binary; pass --celine PATH"))
	}
	absBin, err := filepath.Abs(bin)
	if err != nil {
		return a.fail(err)
	}
	bin = absBin
	if strings.Contains(bin, string(filepath.Separator)+"go-build") {
		return a.fail(fmt.Errorf("refusing to embed the temporary binary %s in boot units; run `go build -o build/celine ./cmd/celine` or pass --celine PATH", bin))
	}
	if info, err := os.Stat(bin); err != nil {
		return a.fail(fmt.Errorf("celine binary %s is not available: %w", bin, err))
	} else if info.Mode().Perm()&0o111 == 0 {
		return a.fail(fmt.Errorf("celine binary %s is not executable", bin))
	}
	extraEnv := map[string]string{}
	for _, name := range []string{
		"SOPS_AGE_KEY_FILE",
		"SOPS_AGE_SSH_PRIVATE_KEY_FILE",
		"SECRETSPEC_PROFILE",
		"SECRETSPEC_PROVIDER",
	} {
		if value, ok := a.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			extraEnv[name] = value
		}
	}
	units, err := render.SystemdUnits(render.SystemdOptions{
		CelineBin:     bin,
		ConfigPath:    cfg.Path,
		User:          a.Username,
		Group:         primaryGroup(a.Username),
		Home:          a.Home,
		DockerHost:    dockerHost,
		MediaRoot:     cfg.Resolved.MediaRoot,
		StateDir:      cfg.Resolved.StateDir,
		BackupDir:     cfg.Resolved.BackupDir,
		OnCalendar:    cfg.Backup.OnCalendar,
		PathEnv:       unitPathEnv(a.Home),
		Rootless:      cfg.Runtime.Rootless,
		UID:           a.UID,
		ExtraEnv:      extraEnv,
		BackupEnabled: cfg.Backup.Enable,
	})
	if err != nil {
		return a.fail(err)
	}
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	sort.Strings(names)
	switch sub {
	case "render":
		dir := *outDir
		if dir == "" {
			dir = filepath.Join(cfg.Root, "build", "systemd")
		}
		for _, name := range names {
			path := filepath.Join(dir, name)
			if err := writeAtomic(path, []byte(units[name]), 0o644); err != nil {
				return a.fail(err)
			}
			a.out("rendered %s", path)
		}
		if cfg.Backup.Enable {
			a.out("install with: sudo install -m644 %s/*.service %s/*.timer %s/ && sudo systemctl daemon-reload && sudo systemctl enable --now celine.service celine-backup.timer", dir, dir, *systemDir)
		} else {
			if err := removeStaleBackupUnits(dir); err != nil {
				return a.fail(err)
			}
			a.out("backup.enable is false; only the stack unit was rendered")
		}
		return 0
	case "install":
		for _, name := range names {
			path := filepath.Join(*systemDir, name)
			if err := os.WriteFile(path, []byte(units[name]), 0o644); err != nil {
				a.errOut("error: install %s: %v", path, err)
				a.errOut("hint: rerun `celine systemd render` and install the files with sudo")
				return 1
			}
			a.out("installed %s", path)
		}
		if !cfg.Backup.Enable {
			if err := removeStaleBackupUnits(*systemDir); err != nil {
				return a.fail(err)
			}
		}
		if cfg.Backup.Enable {
			a.out("run: sudo systemctl daemon-reload && sudo systemctl enable --now celine.service celine-backup.timer")
		} else {
			a.out("run: sudo systemctl daemon-reload && sudo systemctl enable --now celine.service")
		}
		return 0
	default:
		a.errOut("unknown systemd command %q (use render or install)", sub)
		return 2
	}
}

// dirHasFiles reports whether a directory exists and contains at least one
// entry. It is used to decide whether a live restore needs a pre-restore
// snapshot even when Kavita is not enabled.
func dirHasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// removeStaleBackupUnits removes backup units left by an earlier render when
// [backup].enable has since been turned off. Without this, install would leave
// an enabled timer pointing at a unit that is no longer generated.
func removeStaleBackupUnits(dir string) error {
	for _, name := range []string{"celine-backup.service", "celine-backup.timer"} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale %s: %w", path, err)
		}
	}
	return nil
}

// unitPathEnv builds a compact PATH for the boot units from the directories
// that actually provide the runtime tools. A shell PATH is often long and
// contains transient entries that do not exist after a reboot.
func unitPathEnv(home string) string {
	seen := map[string]bool{}
	dirs := make([]string, 0, 10)
	add := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	if home != "" {
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
