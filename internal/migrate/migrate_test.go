package migrate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Meillaya/celine/internal/archiveutil"
	"github.com/Meillaya/celine/internal/execx"
)

func sqliteBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	return path
}

type migrateFixture struct {
	root       string
	legacyDir  string
	stateDir   string
	backupDir  string
	secretFile string
}

func newMigrateFixture(t *testing.T) migrateFixture {
	t.Helper()
	sqlite := sqliteBinary(t)
	root := t.TempDir()
	legacy := filepath.Join(root, "config")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(legacy, "kavita.db")
	schema := `
CREATE TABLE AspNetUsers (Id INTEGER PRIMARY KEY, UserName TEXT);
CREATE TABLE Library (Id INTEGER PRIMARY KEY, Name TEXT, Type INTEGER);
CREATE TABLE FolderPath (LibraryId INTEGER, Path TEXT);
CREATE TABLE ServerSetting (Key INTEGER PRIMARY KEY, Value TEXT);
INSERT INTO AspNetUsers VALUES (1, 'Buried');
INSERT INTO Library VALUES (1, 'Literature Series', 2);
INSERT INTO FolderPath VALUES (1, '/data/Books/Literature Series');
INSERT INTO ServerSetting VALUES (11, '0.9.0.2');
`
	if output, err := exec.Command(sqlite, dbPath, schema).CombinedOutput(); err != nil {
		t.Fatalf("sqlite fixture: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(legacy, "appsettings.json"), []byte(`{"TokenKey":"legacy-token","Port":5000,"Custom":"keep"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return migrateFixture{
		root:       root,
		legacyDir:  legacy,
		stateDir:   filepath.Join(root, "state"),
		backupDir:  filepath.Join(root, "backups"),
		secretFile: filepath.Join(root, "secrets", "celine.enc.yaml"),
	}
}

func migrateOptions(t *testing.T, f migrateFixture, runner execx.Runner) Options {
	t.Helper()
	return Options{
		Root:            f.root,
		LegacyConfigDir: f.legacyDir,
		StateDir:        f.stateDir,
		BackupDir:       f.backupDir,
		SecretFile:      f.secretFile,
		Runner:          runner,
		SQLite:          sqliteBinary(t),
		Sops:            "sops",
		ToolVersion:     "test",
		Now:             func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}
}

type sopsRunner struct {
	decrypt func() (execx.Result, error)
	encrypt func(call execx.Call) (execx.Result, error)
}

func (r sopsRunner) Run(ctx context.Context, name string, args []string, opts execx.Options) (execx.Result, error) {
	if name != "sops" {
		return (execx.OS{}).Run(ctx, name, args, opts)
	}
	for _, arg := range args {
		if arg == "--decrypt" {
			if r.decrypt != nil {
				return r.decrypt()
			}
			return execx.Result{}, context.Canceled
		}
	}
	if r.encrypt != nil {
		return r.encrypt(execx.Call{Name: name, Args: args, Opts: opts})
	}
	return execx.Result{}, context.Canceled
}

func TestMigrateInstallsStateWithoutSecretImport(t *testing.T) {
	f := newMigrateFixture(t)
	result, err := Run(context.Background(), migrateOptions(t, f, execx.OS{}))
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(f.stateDir, "kavita", "config")
	if result.StateConfigDir != configDir {
		t.Fatalf("state config dir = %q", result.StateConfigDir)
	}
	if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
		t.Fatalf("state config dir missing: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("state config dir mode = %o", info.Mode().Perm())
	}
	for _, name := range []string{"kavita.db", "appsettings.json"} {
		info, err := os.Stat(filepath.Join(configDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o", name, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, ".migration.json")); err != nil {
		t.Fatalf("migration marker missing: %v", err)
	}
	entries, err := archiveutil.ListTarGz(result.BackupArchive)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(entries, "\n")
	if !strings.Contains(joined, "kavita.db") || !strings.Contains(joined, "appsettings.json") {
		t.Fatalf("legacy archive incomplete: %v", entries)
	}
}

func TestMigratePreflightDoesNotArchiveOnConflict(t *testing.T) {
	f := newMigrateFixture(t)
	targetDir := filepath.Join(f.stateDir, "kavita", "config")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "appsettings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), migrateOptions(t, f, execx.OS{}))
	if err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("expected conflict error, got %v", err)
	}
	entries, err := os.ReadDir(f.backupDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("conflict created legacy archives: %v", entries)
	}
	data, _ := os.ReadFile(filepath.Join(targetDir, "appsettings.json"))
	if string(data) != "{}" {
		t.Fatal("target was modified during a refused migration")
	}
}

func TestMigrateForceReplacesNonEmptyTarget(t *testing.T) {
	f := newMigrateFixture(t)
	targetDir := filepath.Join(f.stateDir, "kavita", "config")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "appsettings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := migrateOptions(t, f, execx.OS{})
	opts.Force = true
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(result.StateConfigDir); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(f.stateDir, "kavita", "config.pre-migrate-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("pre-migrate directory = %v, %v", matches, err)
	}
}

func TestMigrateExistingStoreIsCompared(t *testing.T) {
	f := newMigrateFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.secretFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.secretFile, []byte("encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := sopsRunner{decrypt: func() (execx.Result, error) {
		return execx.Result{Stdout: []byte("celine:\n  default:\n    KAVITA_TOKEN_KEY: legacy-token\n")}, nil
	}}
	opts := migrateOptions(t, f, runner)
	opts.ImportToken = true
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.TokenImported {
		t.Fatal("existing store was reported as newly imported")
	}
	if result.TokenMatches == nil || !*result.TokenMatches {
		t.Fatalf("token match = %v", result.TokenMatches)
	}
}

func TestMigrateExistingStoreMismatchReportsDiscovery(t *testing.T) {
	f := newMigrateFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.secretFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.secretFile, []byte("encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := sopsRunner{decrypt: func() (execx.Result, error) {
		return execx.Result{Stdout: []byte("celine:\n  default:\n    KAVITA_TOKEN_KEY: different\n")}, nil
	}}
	opts := migrateOptions(t, f, runner)
	opts.ImportToken = true
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.TokenMatches == nil || *result.TokenMatches {
		t.Fatalf("token match = %v", result.TokenMatches)
	}
	if !strings.Contains(strings.Join(result.Discoveries, "\n"), "different signing key") {
		t.Fatalf("discoveries = %v", result.Discoveries)
	}
}

func TestMigrateSecretFailureLeavesStateUntouched(t *testing.T) {
	f := newMigrateFixture(t)
	runner := sopsRunner{encrypt: func(call execx.Call) (execx.Result, error) {
		return execx.Result{}, os.ErrPermission
	}}
	opts := migrateOptions(t, f, runner)
	opts.ImportToken = true
	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("expected secret-import failure")
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, "kavita", "config", "kavita.db")); !os.IsNotExist(err) {
		t.Fatalf("state was installed despite secret failure: %v", err)
	}
	if _, err := os.Stat(f.secretFile); !os.IsNotExist(err) {
		t.Fatalf("partial secret store was left behind: %v", err)
	}
}

func TestMigrateNewSecretStoreRoundTrip(t *testing.T) {
	f := newMigrateFixture(t)
	runner := sopsRunner{
		decrypt: func() (execx.Result, error) {
			return execx.Result{Stdout: []byte("celine:\n  default:\n    KAVITA_TOKEN_KEY: legacy-token\n")}, nil
		},
		encrypt: func(call execx.Call) (execx.Result, error) {
			// sops writes the encrypted document to --output.
			for i, arg := range call.Args {
				if arg == "--output" && i+1 < len(call.Args) {
					if err := os.WriteFile(call.Args[i+1], []byte("encrypted"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return execx.Result{}, nil
		},
	}
	opts := migrateOptions(t, f, runner)
	opts.ImportToken = true
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TokenImported {
		t.Fatal("new signing key was not reported as imported")
	}
	info, err := os.Stat(f.secretFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret store mode = %o", info.Mode().Perm())
	}
}

func TestMigrateMissingTokenRecordsDiscovery(t *testing.T) {
	f := newMigrateFixture(t)
	if err := os.Remove(filepath.Join(f.legacyDir, "appsettings.json")); err != nil {
		t.Fatal(err)
	}
	opts := migrateOptions(t, f, execx.OS{})
	opts.ImportToken = true
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.TokenPresent {
		t.Fatal("token was reported present")
	}
	if !strings.Contains(strings.Join(result.Discoveries, "\n"), "no readable TokenKey") {
		t.Fatalf("discoveries = %v", result.Discoveries)
	}
}
