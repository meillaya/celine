package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
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

type fixture struct {
	configDir  string
	calibreDir string
	backupDir  string
	dbPath     string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	sqlite := sqliteBinary(t)
	root := t.TempDir()
	configDir := filepath.Join(root, "state", "kavita", "config")
	calibreDir := filepath.Join(root, "state", "calibre")
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(calibreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(configDir, "kavita.db")
	schema := `
PRAGMA journal_mode=WAL;
CREATE TABLE AspNetUsers (Id INTEGER PRIMARY KEY, UserName TEXT);
CREATE TABLE Library (Id INTEGER PRIMARY KEY, Name TEXT, Type INTEGER);
CREATE TABLE FolderPath (LibraryId INTEGER, Path TEXT);
CREATE TABLE ServerSetting (Key INTEGER PRIMARY KEY, Value TEXT);
INSERT INTO AspNetUsers VALUES (1, 'Buried');
INSERT INTO Library VALUES (1, 'Literature Series', 2);
INSERT INTO FolderPath VALUES (1, '/data/Books/Literature Series');
INSERT INTO ServerSetting VALUES (11, '0.9.1.4');
`
	if output, err := exec.Command(sqlite, dbPath, schema).CombinedOutput(); err != nil {
		t.Fatalf("create sqlite fixture: %v: %s", err, output)
	}
	settings := `{"TokenKey":"fixture-token","Port":5000,"Custom":"keep","OpenIdConnectSettings":{"Enabled":false,"Secret":"oidc-secret","Authority":"https://id.example"}}`
	if err := os.WriteFile(filepath.Join(configDir, "appsettings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "bookmarks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "bookmarks", "one.json"), []byte(`{"page":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(calibreDir, "caches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(calibreDir, "global.py.json"), []byte(`{"port":8080}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(calibreDir, "caches", "ignored"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture{configDir: configDir, calibreDir: calibreDir, backupDir: backupDir, dbPath: dbPath}
}

func runBackup(t *testing.T, f fixture, now time.Time, mutate func(*Options)) Result {
	t.Helper()
	opts := Options{
		KavitaConfigDir:  f.configDir,
		CalibreConfigDir: f.calibreDir,
		BackupDir:        f.backupDir,
		KavitaEnabled:    true,
		CalibreEnabled:   true,
		Retention:        5,
		ToolVersion:      "test",
		Runner:           execx.OS{},
		SQLite:           sqliteBinary(t),
		Now:              func() time.Time { return now },
	}
	if mutate != nil {
		mutate(&opts)
	}
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func restoreOpts(t *testing.T, archive, stateDir string) RestoreOptions {
	t.Helper()
	return RestoreOptions{
		Archive:         archive,
		StateDir:        stateDir,
		Force:           true,
		RequireKavitaDB: true,
		Runner:          execx.OS{},
		SQLite:          sqliteBinary(t),
	}
}

func TestRunAndRestore(t *testing.T) {
	f := newFixture(t)
	result := runBackup(t, f, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), nil)
	if result.Files == 0 || result.Bytes == 0 {
		t.Fatalf("result = %+v", result)
	}
	entries, err := archiveutil.ListTarGz(result.Archive)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, entry := range entries {
		found[entry] = true
	}
	for _, want := range []string{"manifest.json", "kavita/config/kavita.db", "kavita/config/appsettings.json", "kavita/config/bookmarks/one.json"} {
		if !found[want] {
			t.Errorf("archive missing %q: %v", want, entries)
		}
	}
	for _, unwanted := range []string{"kavita/config/kavita.db-wal", "kavita/config/kavita.db-shm"} {
		if found[unwanted] {
			t.Errorf("archive contains stale SQLite sidecar %q", unwanted)
		}
	}

	verify, err := Verify(context.Background(), VerifyOptions{Archive: result.Archive, Runner: execx.OS{}, SQLite: sqliteBinary(t), RequireKavitaDB: true})
	if err != nil {
		t.Fatal(err)
	}
	if verify.SecretsPresent {
		t.Fatal("redacted archive still reports secret material")
	}
	if verify.Manifest.KavitaUsers != 1 || verify.Manifest.KavitaLibraries != 1 {
		t.Fatalf("manifest = %+v", verify.Manifest)
	}

	restoreState := t.TempDir()
	configDir := filepath.Join(restoreState, "kavita", "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Stale sidecars from a previous database must not survive the restore.
	for _, name := range []string{"kavita.db-wal", "kavita.db-shm"} {
		if err := os.WriteFile(filepath.Join(configDir, name), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := Restore(context.Background(), restoreOpts(t, result.Archive, restoreState))
	if err != nil {
		t.Fatal(err)
	}
	if restored.Files == 0 {
		t.Fatal("restored no files")
	}
	for _, name := range []string{"kavita.db-wal", "kavita.db-shm"} {
		if _, err := os.Stat(filepath.Join(configDir, name)); !os.IsNotExist(err) {
			t.Fatalf("stale sidecar %s survived restore: %v", name, err)
		}
	}
	for path, mode := range map[string]os.FileMode{
		configDir:                                    0o700,
		filepath.Join(configDir, "kavita.db"):        0o600,
		filepath.Join(configDir, "appsettings.json"): 0o600,
		filepath.Join(restoreState, "calibre"):       0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
	data, err := os.ReadFile(filepath.Join(configDir, "appsettings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["TokenKey"]; ok {
		t.Fatal("restored appsettings contains TokenKey")
	}
	if _, ok := settings["Custom"]; !ok {
		t.Fatal("restored appsettings lost a non-secret setting")
	}
}

func TestRunRedactsSecrets(t *testing.T) {
	f := newFixture(t)
	result := runBackup(t, f, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), nil)
	if len(result.Redacted) < 2 {
		t.Fatalf("redacted paths = %v", result.Redacted)
	}
	raw := readArchiveFile(t, result.Archive, "kavita/config/appsettings.json")
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["TokenKey"]; ok {
		t.Fatal("TokenKey was archived")
	}
	oidc, _ := settings["OpenIdConnectSettings"].(map[string]any)
	if oidc == nil {
		t.Fatal("OIDC settings were dropped entirely")
	}
	if _, ok := oidc["Secret"]; ok {
		t.Fatal("OIDC client secret was archived")
	}
	if oidc["Authority"] != "https://id.example" {
		t.Fatalf("OIDC settings were not preserved: %v", oidc)
	}
	if settings["Custom"] != "keep" {
		t.Fatalf("custom setting was not preserved: %v", settings)
	}
}

func TestRunCalibreOnly(t *testing.T) {
	f := newFixture(t)
	result := runBackup(t, f, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), func(opts *Options) {
		opts.KavitaEnabled = false
		opts.CalibreEnabled = true
	})
	entries, err := archiveutil.ListTarGz(result.Archive)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(entries, "\n")
	if strings.Contains(joined, "kavita/") {
		t.Fatalf("Calibre-only archive contains Kavita state: %v", entries)
	}
	if !strings.Contains(joined, "calibre/global.py.json") {
		t.Fatalf("Calibre-only archive missing Calibre config: %v", entries)
	}
	if strings.Contains(joined, "caches") {
		t.Fatalf("Calibre cache was archived: %v", entries)
	}
}

func TestRestoreRefusesWithoutForce(t *testing.T) {
	f := newFixture(t)
	result := runBackup(t, f, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), nil)
	target := t.TempDir()
	configDir := filepath.Join(target, "kavita", "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(configDir, "kavita.db")
	if err := os.WriteFile(existing, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := restoreOpts(t, result.Archive, target)
	opts.Force = false
	_, err := Restore(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "without --force") {
		t.Fatalf("expected force refusal, got %v", err)
	}
	data, _ := os.ReadFile(existing)
	if string(data) != "existing" {
		t.Fatalf("existing database was modified: %q", data)
	}
}

func TestVerifyRejectsMissingDatabase(t *testing.T) {
	f := newFixture(t)
	result := runBackup(t, f, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), func(opts *Options) {
		opts.KavitaEnabled = false
	})
	if _, err := Verify(context.Background(), VerifyOptions{
		Archive: result.Archive, Runner: execx.OS{}, SQLite: sqliteBinary(t), RequireKavitaDB: true,
	}); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("expected missing-database error, got %v", err)
	}
}

func TestVerifyRejectsGarbageArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(path, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), VerifyOptions{Archive: path}); err == nil {
		t.Fatal("expected a verification error")
	}
}

func TestVerifyDetectsArchivedSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.tar.gz")
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(filepath.Join(stage, "kavita", "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "kavita", "config", "appsettings.json"), []byte(`{"TokenKey":"still-here","Other":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Format: formatVersion, Tool: "celine", Files: []string{"kavita/config/appsettings.json"}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveutil.TarGz(stage, path); err != nil {
		t.Fatal(err)
	}
	result, err := Verify(context.Background(), VerifyOptions{Archive: path})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SecretsPresent {
		t.Fatal("verification did not report an archived TokenKey")
	}
}

func TestSkipPruneKeepsExistingArchives(t *testing.T) {
	f := newFixture(t)
	runBackup(t, f, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), nil)
	runBackup(t, f, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC), nil)
	runBackup(t, f, time.Date(2026, 11, 7, 12, 2, 0, 0, time.UTC), func(opts *Options) {
		opts.Retention = 1
		opts.SkipPrune = true
	})
	archives, err := List(f.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 3 {
		t.Fatalf("SkipPrune pruned archives: %v", archives)
	}
}

func TestRetentionPrunes(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		runBackup(t, f, base.Add(time.Duration(i)*time.Minute), func(opts *Options) { opts.Retention = 2 })
	}
	archives, err := List(f.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 2 {
		t.Fatalf("retention kept %d archives, want 2: %v", len(archives), archives)
	}
}

func TestPruneAlwaysKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, "celine-20261107T12000"+string(rune('0'+i))+"Z.tar.gz")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		mod := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune(dir, 1, paths[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("keep archive was pruned: %v", err)
	}
}

func readArchiveFile(t *testing.T, archive, name string) []byte {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err != nil {
			t.Fatalf("archive entry %s not found", name)
		}
		if header.Name == name {
			data := make([]byte, header.Size)
			if _, err := io.ReadFull(tr, data); err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
}
