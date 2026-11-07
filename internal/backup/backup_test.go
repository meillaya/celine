package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func fixture(t *testing.T) string {
	t.Helper()
	sqlite := sqliteBinary(t)
	configDir := filepath.Join(t.TempDir(), "state", "kavita", "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(configDir, "kavita.db")
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
		t.Fatalf("create sqlite fixture: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(configDir, "appsettings.json"), []byte(`{"TokenKey":"fixture-token","Port":5000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return configDir
}

func TestRunAndRestore(t *testing.T) {
	configDir := fixture(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	result, err := Run(context.Background(), Options{
		KavitaConfigDir:  configDir,
		CalibreConfigDir: "",
		BackupDir:        backupDir,
		ConfigPath:       "",
		Retention:        5,
		ToolVersion:      "test",
		Runner:           execx.OS{},
		SQLite:           sqliteBinary(t),
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files == 0 {
		t.Fatal("no files archived")
	}
	entries, err := archiveutil.ListTarGz(result.Archive)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, entry := range entries {
		found[entry] = true
	}
	for _, want := range []string{"manifest.json", "kavita/config/kavita.db", "kavita/config/appsettings.json"} {
		if !found[want] {
			t.Errorf("archive missing %q: %v", want, entries)
		}
	}

	restoreState := t.TempDir()
	restored, err := Restore(context.Background(), RestoreOptions{
		Archive:  result.Archive,
		StateDir: restoreState,
		Force:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Files == 0 {
		t.Fatal("restored no files")
	}
	dbPath := filepath.Join(restoreState, "kavita", "config", "kavita.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("restored database missing: %v", err)
	}
	if restored.Manifest.KavitaUsers != 1 || restored.Manifest.KavitaLibraries != 1 {
		t.Fatalf("manifest = %+v", restored.Manifest)
	}
}

func TestRetentionPrunes(t *testing.T) {
	configDir := fixture(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	base := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		_, err := Run(context.Background(), Options{
			KavitaConfigDir: configDir,
			BackupDir:       backupDir,
			Retention:       2,
			ToolVersion:     "test",
			Runner:          execx.OS{},
			SQLite:          sqliteBinary(t),
			Now:             func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	archives, err := List(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 2 {
		t.Fatalf("retention kept %d archives, want 2: %v", len(archives), archives)
	}
}
