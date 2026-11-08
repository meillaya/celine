// Package backup archives the configuration state that cannot be recreated:
// the Kavita database and app settings, bookmarks, and Calibre server config.
// Media files are never archived. Secret values in Kavita settings are redacted
// before archiving; Celine re-renders them from SecretSpec on the next start.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Meillaya/celine/internal/archiveutil"
	"github.com/Meillaya/celine/internal/execx"
	"github.com/Meillaya/celine/internal/kavita"
)

const (
	formatVersion = 1
	archivePrefix = "celine-"
	archiveSuffix = ".tar.gz"
)

// secretKeys are removed from archived Kavita settings. Matching is
// case-insensitive so TokenKey, tokenkey, and ClientSecret are all covered.
var secretKeys = map[string]bool{
	"tokenkey":     true,
	"secret":       true,
	"clientsecret": true,
	"password":     true,
	"apikey":       true,
	"api_key":      true,
	"privatekey":   true,
	"private_key":  true,
}

// Options configures one backup run.
type Options struct {
	KavitaConfigDir  string
	CalibreConfigDir string
	BackupDir        string
	ConfigPath       string
	KavitaEnabled    bool
	CalibreEnabled   bool
	Retention        int
	// SkipPrune is set for the pre-restore snapshot, which must not delete the
	// archive that is about to be restored.
	SkipPrune   bool
	ToolVersion string
	Runner      execx.Runner
	SQLite      string
	Now         func() time.Time
}

// Manifest is stored inside every archive.
type Manifest struct {
	Format          int               `json:"format"`
	Tool            string            `json:"tool"`
	Version         string            `json:"version"`
	CreatedAt       string            `json:"created_at"`
	ConfigSHA256    string            `json:"config_sha256,omitempty"`
	KavitaVersion   string            `json:"kavita_version,omitempty"`
	KavitaUsers     int               `json:"kavita_users"`
	KavitaLibraries int               `json:"kavita_libraries"`
	Files           []string          `json:"files"`
	FileSHA256      map[string]string `json:"file_sha256,omitempty"`
	Excluded        []string          `json:"excluded,omitempty"`
}

// Result describes a completed backup.
type Result struct {
	Archive  string
	Files    int
	Bytes    int64
	Redacted []string
}

// Run creates a verified archive. A failed verification leaves the archive in
// place for diagnosis but returns an error and does not prune older archives.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Runner == nil {
		return Result{}, fmt.Errorf("backup requires a command runner")
	}
	if !opts.KavitaEnabled && !opts.CalibreEnabled {
		return Result{}, fmt.Errorf("backup requires at least one enabled service")
	}
	if !opts.SkipPrune && opts.Retention < 1 {
		return Result{}, fmt.Errorf("retention must be at least 1")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	sqlite := opts.SQLite
	if sqlite == "" {
		sqlite = "sqlite3"
	}

	if opts.KavitaEnabled {
		dbPath := filepath.Join(opts.KavitaConfigDir, "kavita.db")
		if info, err := os.Stat(dbPath); err != nil || info.IsDir() {
			return Result{}, fmt.Errorf("Kavita database not found at %s; run `celine migrate` first", dbPath)
		}
	}
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create backup directory: %w", err)
	}

	stage, err := os.MkdirTemp(opts.BackupDir, ".stage-")
	if err != nil {
		return Result{}, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	var liveInventory kavita.Inventory
	var redactedPaths []string
	if opts.KavitaEnabled {
		dbPath := filepath.Join(opts.KavitaConfigDir, "kavita.db")
		liveInventory, err = kavita.ReadInventory(ctx, opts.Runner, sqlite, dbPath)
		if err != nil {
			return Result{}, err
		}
		if err := kavita.IntegrityCheck(ctx, opts.Runner, sqlite, dbPath); err != nil {
			return Result{}, fmt.Errorf("refusing to back up a database that fails its integrity check: %w", err)
		}

		kavitaDest := filepath.Join(stage, "kavita", "config")
		if err := os.MkdirAll(kavitaDest, 0o700); err != nil {
			return Result{}, err
		}
		if err := kavita.Backup(ctx, opts.Runner, sqlite, dbPath, filepath.Join(kavitaDest, "kavita.db")); err != nil {
			return Result{}, err
		}
		// SQLite may leave WAL/SHM sidecars next to the copy. They describe the
		// state of the source database, not the extracted archive, and copying
		// them over a restored database is exactly what corrupts restores.
		for _, sidecar := range []string{"kavita.db-wal", "kavita.db-shm"} {
			if err := os.Remove(filepath.Join(kavitaDest, sidecar)); err != nil && !os.IsNotExist(err) {
				return Result{}, fmt.Errorf("remove staged %s: %w", sidecar, err)
			}
		}
		if err := kavita.IntegrityCheck(ctx, opts.Runner, sqlite, filepath.Join(kavitaDest, "kavita.db")); err != nil {
			return Result{}, fmt.Errorf("staged database failed its integrity check: %w", err)
		}
		stagedInventory, err := kavita.ReadInventory(ctx, opts.Runner, sqlite, filepath.Join(kavitaDest, "kavita.db"))
		if err != nil {
			return Result{}, err
		}
		if len(stagedInventory.Users) != len(liveInventory.Users) || len(stagedInventory.Libraries) != len(liveInventory.Libraries) {
			return Result{}, fmt.Errorf("Kavita database changed during backup (live %d users/%d libraries, staged %d users/%d libraries); retry",
				len(liveInventory.Users), len(liveInventory.Libraries), len(stagedInventory.Users), len(stagedInventory.Libraries))
		}

		redacted, err := stageAppSettings(opts.KavitaConfigDir, kavitaDest)
		if err != nil {
			return Result{}, err
		}
		redactedPaths = redacted
		bookmarks := filepath.Join(opts.KavitaConfigDir, "bookmarks")
		if info, err := os.Stat(bookmarks); err == nil && info.IsDir() {
			if err := archiveutil.CopyTree(bookmarks, filepath.Join(kavitaDest, "bookmarks"), nil); err != nil {
				return Result{}, fmt.Errorf("copy bookmarks: %w", err)
			}
		}
	}
	if opts.CalibreEnabled && opts.CalibreConfigDir != "" {
		if info, err := os.Stat(opts.CalibreConfigDir); err == nil && info.IsDir() {
			skip := func(rel string) bool {
				base := filepath.Base(rel)
				return base == "cache" || base == "caches" || base == ".cache" || base == "logs" || strings.HasSuffix(base, ".log")
			}
			if err := archiveutil.CopyTree(opts.CalibreConfigDir, filepath.Join(stage, "calibre"), skip); err != nil {
				return Result{}, fmt.Errorf("copy Calibre config: %w", err)
			}
		}
	}

	// Opening the staged database for the integrity and inventory checks can
	// recreate WAL/SHM sidecars even though they were removed after .backup.
	// Remove them after every SQLite operation, immediately before listing the
	// files that go into the archive.
	for _, sidecar := range []string{"kavita.db-wal", "kavita.db-shm"} {
		if err := os.Remove(filepath.Join(stage, "kavita", "config", sidecar)); err != nil && !os.IsNotExist(err) {
			return Result{}, fmt.Errorf("remove staged %s: %w", sidecar, err)
		}
	}

	files, err := archiveutil.FilesUnder(stage)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 {
		return Result{}, fmt.Errorf("nothing to archive")
	}
	hashes := make(map[string]string, len(files))
	for _, name := range files {
		sum, err := archiveutil.SHA256File(filepath.Join(stage, filepath.FromSlash(name)))
		if err != nil {
			return Result{}, fmt.Errorf("hash %s: %w", name, err)
		}
		hashes[name] = sum
	}
	configHash := ""
	if opts.ConfigPath != "" {
		configHash, _ = archiveutil.SHA256File(opts.ConfigPath)
	}
	manifest := Manifest{
		Format:          formatVersion,
		Tool:            "celine",
		Version:         opts.ToolVersion,
		CreatedAt:       opts.Now().UTC().Format(time.RFC3339),
		ConfigSHA256:    configHash,
		KavitaVersion:   liveInventory.Version,
		KavitaUsers:     len(liveInventory.Users),
		KavitaLibraries: len(liveInventory.Libraries),
		Files:           files,
		FileSHA256:      hashes,
		Excluded: []string{
			"kavita caches, covers, themes, fonts, templates, favicons, images, logs, temp, and internal backups",
			"calibre caches and logs",
			"media libraries",
			"secret values in appsettings.json (TokenKey and nested secrets are redacted)",
		},
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), append(manifestBytes, '\n'), 0o600); err != nil {
		return Result{}, fmt.Errorf("write manifest: %w", err)
	}

	final := uniqueArchivePath(opts.BackupDir, opts.Now().UTC())
	tmp, err := tempArchivePath(opts.BackupDir)
	if err != nil {
		return Result{}, err
	}
	if err := archiveutil.TarGz(stage, tmp); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return Result{}, fmt.Errorf("install backup archive: %w", err)
	}
	result := Result{Archive: final, Files: len(files) + 1, Redacted: redactedPaths}
	if err := verifyFinal(final, opts, files); err != nil {
		return result, fmt.Errorf("new archive failed verification: %w", err)
	}
	if !opts.SkipPrune {
		if err := Prune(opts.BackupDir, opts.Retention, final); err != nil {
			return result, err
		}
	}
	info, err := os.Stat(final)
	if err != nil {
		return result, err
	}
	result.Bytes = info.Size()
	return result, nil
}

func verifyFinal(archive string, opts Options, staged []string) error {
	entries, err := archiveutil.ListTarGz(archive)
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for _, entry := range entries {
		found[entry] = true
	}
	if !found["manifest.json"] {
		return fmt.Errorf("archive is missing manifest.json")
	}
	if opts.KavitaEnabled && !found["kavita/config/kavita.db"] {
		return fmt.Errorf("archive is missing kavita/config/kavita.db")
	}
	for _, name := range staged {
		if !found[name] {
			return fmt.Errorf("archive is missing %s", name)
		}
	}
	verified, err := Verify(context.Background(), VerifyOptions{
		Archive:         archive,
		Runner:          opts.Runner,
		SQLite:          opts.SQLite,
		TempDir:         opts.BackupDir,
		RequireKavitaDB: opts.KavitaEnabled,
	})
	if err != nil {
		return err
	}
	if verified.SecretsPresent {
		return fmt.Errorf("archive still contains secret values after redaction")
	}
	return nil
}

func stageAppSettings(srcDir, destDir string) ([]string, error) {
	src := filepath.Join(srcDir, "appsettings.json")
	raw, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read appsettings.json: %w", err)
	}
	redacted, paths, err := redactAppSettings(raw)
	if err != nil {
		return nil, fmt.Errorf("redact appsettings.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "appsettings.json"), redacted, 0o600); err != nil {
		return nil, fmt.Errorf("stage appsettings.json: %w", err)
	}
	return paths, nil
}

// redactAppSettings removes known secret keys while preserving all other
// settings. It returns the dotted paths that were removed.
func redactAppSettings(raw []byte) ([]byte, []string, error) {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, nil, err
	}
	if data == nil {
		data = map[string]any{}
	}
	var removed []string
	redactValue(data, "", &removed)
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(removed)
	return append(out, '\n'), removed, nil
}

func redactValue(value any, path string, removed *[]string) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if secretKeys[strings.ToLower(key)] {
				delete(current, key)
				*removed = append(*removed, childPath)
				continue
			}
			redactValue(child, childPath, removed)
		}
	case []any:
		for index, child := range current {
			redactValue(child, fmt.Sprintf("%s[%d]", path, index), removed)
		}
	}
}

func containsSecret(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if secretKeys[strings.ToLower(key)] && !emptyJSON(child) {
				return true
			}
			if containsSecret(child) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if containsSecret(child) {
				return true
			}
		}
	}
	return false
}

func emptyJSON(value any) bool {
	switch current := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(current) == ""
	case float64:
		return current == 0
	case bool:
		return !current
	case map[string]any:
		return len(current) == 0
	case []any:
		return len(current) == 0
	}
	return false
}

// VerifyOptions configures archive verification without writing state.
type VerifyOptions struct {
	Archive         string
	Runner          execx.Runner
	SQLite          string
	TempDir         string
	RequireKavitaDB bool
}

// VerifyResult describes a verified archive.
type VerifyResult struct {
	Manifest       Manifest
	Files          int
	SecretsPresent bool
}

// Verify extracts an archive to a temporary directory and checks the manifest,
// every file hash, the database integrity, and the recorded inventory. It never
// writes to the deployment state.
func Verify(_ context.Context, opts VerifyOptions) (VerifyResult, error) {
	staging, result, err := extractVerified(opts)
	if staging != "" {
		defer os.RemoveAll(staging)
	}
	return result, err
}

func extractVerified(opts VerifyOptions) (string, VerifyResult, error) {
	if opts.Archive == "" {
		return "", VerifyResult{}, fmt.Errorf("archive path is required")
	}
	staging, err := os.MkdirTemp(opts.TempDir, ".celine-verify-")
	if err != nil {
		return "", VerifyResult{}, fmt.Errorf("create verification directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(staging)
		}
	}()
	if err := archiveutil.ExtractTarGz(opts.Archive, staging); err != nil {
		return "", VerifyResult{}, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	if err != nil {
		return "", VerifyResult{}, fmt.Errorf("archive has no manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return "", VerifyResult{}, fmt.Errorf("parse archive manifest: %w", err)
	}
	if manifest.Format != formatVersion {
		return "", VerifyResult{}, fmt.Errorf("unsupported archive format %d", manifest.Format)
	}
	if len(manifest.Files) == 0 {
		return "", VerifyResult{}, fmt.Errorf("archive manifest lists no files")
	}
	for _, name := range manifest.Files {
		rel, err := safeManifestPath(name)
		if err != nil {
			return "", VerifyResult{}, err
		}
		path := filepath.Join(staging, rel)
		info, err := os.Stat(path)
		if err != nil {
			return "", VerifyResult{}, fmt.Errorf("archive is missing %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return "", VerifyResult{}, fmt.Errorf("archive entry %s is not a regular file", name)
		}
		if want := manifest.FileSHA256[name]; want != "" {
			got, err := archiveutil.SHA256File(path)
			if err != nil {
				return "", VerifyResult{}, err
			}
			if got != want {
				return "", VerifyResult{}, fmt.Errorf("archive file %s failed its SHA-256 check", name)
			}
		}
	}
	hasDB := false
	if info, err := os.Stat(filepath.Join(staging, "kavita", "config", "kavita.db")); err == nil && info.Mode().IsRegular() {
		hasDB = true
	}
	if opts.RequireKavitaDB && !hasDB {
		return "", VerifyResult{}, fmt.Errorf("archive does not contain kavita/config/kavita.db")
	}
	if hasDB {
		if opts.Runner == nil {
			return "", VerifyResult{}, fmt.Errorf("archive verification requires a command runner")
		}
		sqlite := opts.SQLite
		if sqlite == "" {
			sqlite = "sqlite3"
		}
		dbPath := filepath.Join(staging, "kavita", "config", "kavita.db")
		if err := kavita.IntegrityCheck(context.Background(), opts.Runner, sqlite, dbPath); err != nil {
			return "", VerifyResult{}, fmt.Errorf("archived database failed its integrity check: %w", err)
		}
		inventory, err := kavita.ReadInventory(context.Background(), opts.Runner, sqlite, dbPath)
		if err != nil {
			return "", VerifyResult{}, err
		}
		if len(inventory.Users) != manifest.KavitaUsers || len(inventory.Libraries) != manifest.KavitaLibraries {
			return "", VerifyResult{}, fmt.Errorf("archived database inventory (%d users, %d libraries) does not match the manifest (%d users, %d libraries)",
				len(inventory.Users), len(inventory.Libraries), manifest.KavitaUsers, manifest.KavitaLibraries)
		}
	}
	secretsPresent := false
	if raw, err := os.ReadFile(filepath.Join(staging, "kavita", "config", "appsettings.json")); err == nil {
		var settings map[string]any
		if err := json.Unmarshal(raw, &settings); err == nil {
			secretsPresent = containsSecret(settings)
		}
	}
	cleanup = false
	return staging, VerifyResult{Manifest: manifest, Files: len(manifest.Files), SecretsPresent: secretsPresent}, nil
}

func safeManifestPath(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive manifest contains unsafe path %q", name)
	}
	return clean, nil
}

// RestoreOptions configures an archive restore.
type RestoreOptions struct {
	Archive         string
	StateDir        string
	Force           bool
	RequireKavitaDB bool
	Runner          execx.Runner
	SQLite          string
	Now             func() time.Time
}

// RestoreResult describes a completed restore.
type RestoreResult struct {
	Manifest Manifest
	Files    int
}

// Restore verifies an archive, then copies it over the state directory. The
// database is installed through a temporary file and renamed into place; stale
// WAL/SHM sidecars are removed first. Callers must stop the services and take a
// pre-restore backup of live state.
func Restore(ctx context.Context, opts RestoreOptions) (RestoreResult, error) {
	if opts.StateDir == "" {
		return RestoreResult{}, fmt.Errorf("state directory is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		return RestoreResult{}, err
	}
	targetDB := filepath.Join(opts.StateDir, "kavita", "config", "kavita.db")
	if _, err := os.Stat(targetDB); err == nil && !opts.Force {
		return RestoreResult{}, fmt.Errorf("refusing to overwrite existing Kavita database at %s without --force", targetDB)
	}

	staging, verified, err := extractVerified(VerifyOptions{
		Archive:         opts.Archive,
		Runner:          opts.Runner,
		SQLite:          opts.SQLite,
		TempDir:         opts.StateDir,
		RequireKavitaDB: opts.RequireKavitaDB,
	})
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.RemoveAll(staging)

	if err := applyRestore(staging, opts.StateDir, now()); err != nil {
		return RestoreResult{}, err
	}
	// The staged database was verified before the copy. Check the installed
	// file again so a partial or corrupt rename cannot pass silently.
	if _, err := os.Stat(targetDB); err == nil && opts.RequireKavitaDB {
		if opts.Runner == nil {
			return RestoreResult{}, fmt.Errorf("restored database %s requires a command runner for verification", targetDB)
		}
		sqlite := opts.SQLite
		if sqlite == "" {
			sqlite = "sqlite3"
		}
		if err := kavita.IntegrityCheck(ctx, opts.Runner, sqlite, targetDB); err != nil {
			return RestoreResult{}, fmt.Errorf("restored database failed its integrity check: %w", err)
		}
		inventory, err := kavita.ReadInventory(ctx, opts.Runner, sqlite, targetDB)
		if err != nil {
			return RestoreResult{}, err
		}
		if len(inventory.Users) != verified.Manifest.KavitaUsers || len(inventory.Libraries) != verified.Manifest.KavitaLibraries {
			return RestoreResult{}, fmt.Errorf("restored database inventory (%d users, %d libraries) does not match the manifest (%d users, %d libraries)",
				len(inventory.Users), len(inventory.Libraries), verified.Manifest.KavitaUsers, verified.Manifest.KavitaLibraries)
		}
		// The verification connection can recreate WAL/SHM sidecars. They
		// describe this verification, not the archived state, so remove them
		// before returning.
		for _, sidecar := range []string{"kavita.db-wal", "kavita.db-shm"} {
			if err := os.Remove(filepath.Join(filepath.Dir(targetDB), sidecar)); err != nil && !os.IsNotExist(err) {
				return RestoreResult{}, fmt.Errorf("remove verification %s: %w", sidecar, err)
			}
		}
	}
	return RestoreResult{Manifest: verified.Manifest, Files: verified.Files}, nil
}

func applyRestore(staging, stateDir string, now time.Time) error {
	targetKavita := filepath.Join(stateDir, "kavita", "config")
	if err := os.MkdirAll(targetKavita, 0o700); err != nil {
		return err
	}
	stagedKavita := filepath.Join(staging, "kavita", "config")
	if info, err := os.Stat(stagedKavita); err == nil && info.IsDir() {
		skipDB := func(rel string) bool {
			base := filepath.Base(rel)
			return base == "kavita.db" || base == "kavita.db-wal" || base == "kavita.db-shm" ||
				strings.HasSuffix(base, ".db-wal") || strings.HasSuffix(base, ".db-shm")
		}
		if err := archiveutil.CopyTree(stagedKavita, targetKavita, skipDB); err != nil {
			return err
		}
	}
	stagedCalibre := filepath.Join(staging, "calibre")
	if info, err := os.Stat(stagedCalibre); err == nil && info.IsDir() {
		calibreTarget := filepath.Join(stateDir, "calibre")
		if err := archiveutil.CopyTree(stagedCalibre, calibreTarget, nil); err != nil {
			return err
		}
		if err := os.Chmod(calibreTarget, 0o700); err != nil {
			return fmt.Errorf("secure Calibre state directory: %w", err)
		}
		for _, name := range []string{"server-users.sqlite", "global.py.json", "gui.json"} {
			path := filepath.Join(calibreTarget, name)
			if _, err := os.Stat(path); err == nil {
				if err := os.Chmod(path, 0o600); err != nil {
					return fmt.Errorf("secure %s: %w", path, err)
				}
			}
		}
	}
	// Any WAL/SHM from the previous database is stale once the database file is
	// replaced. SQLite can otherwise apply old pages to the restored database.
	for _, pattern := range []string{"*.db-wal", "*.db-shm"} {
		matches, err := filepath.Glob(filepath.Join(targetKavita, pattern))
		if err != nil {
			return err
		}
		for _, match := range matches {
			if err := os.Remove(match); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove stale sidecar %s: %w", match, err)
			}
		}
	}

	stagedDB := filepath.Join(stagedKavita, "kavita.db")
	if info, err := os.Stat(stagedDB); err == nil && info.Mode().IsRegular() {
		targetDB := filepath.Join(targetKavita, "kavita.db")
		tmp := targetDB + ".restore-" + now.UTC().Format("20060102T150405Z")
		if err := archiveutil.CopyFile(stagedDB, tmp, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, targetDB); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("install restored database: %w", err)
		}
	}
	if err := os.Chmod(targetKavita, 0o700); err != nil {
		return fmt.Errorf("secure Kavita config directory: %w", err)
	}
	for _, name := range []string{"appsettings.json", "kavita.db"} {
		path := filepath.Join(targetKavita, name)
		if _, err := os.Stat(path); err == nil {
			if err := os.Chmod(path, 0o600); err != nil {
				return fmt.Errorf("secure %s: %w", path, err)
			}
		}
	}
	return nil
}

// List returns archive paths, newest first. Ordering uses the file modification
// time so a clock step backwards cannot make a new archive look oldest.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type archive struct {
		path string
		mod  time.Time
	}
	var archives []archive
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, archivePrefix) || !strings.HasSuffix(name, archiveSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		archives = append(archives, archive{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].mod.Equal(archives[j].mod) {
			return archives[i].path > archives[j].path
		}
		return archives[i].mod.After(archives[j].mod)
	})
	paths := make([]string, 0, len(archives))
	for _, archive := range archives {
		paths = append(paths, archive.path)
	}
	return paths, nil
}

// Latest returns the newest archive, if any.
func Latest(dir string) (string, error) {
	archives, err := List(dir)
	if err != nil || len(archives) == 0 {
		return "", err
	}
	return archives[0], nil
}

// Prune removes archives beyond the retention count. keep, when non-empty, is
// always retained even if its timestamp or lexicographic name would otherwise
// place it outside the window.
func Prune(dir string, retention int, keep string) error {
	if retention < 1 {
		return fmt.Errorf("retention must be at least 1")
	}
	archives, err := List(dir)
	if err != nil {
		return err
	}
	keepPath := ""
	if keep != "" {
		keepPath = filepath.Clean(keep)
	}
	kept := 0
	for _, archive := range archives {
		if kept < retention {
			kept++
			continue
		}
		if keepPath != "" && filepath.Clean(archive) == keepPath {
			kept++
			continue
		}
		if err := os.Remove(archive); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("prune %s: %w", archive, err)
		}
	}
	return nil
}

func uniqueArchivePath(dir string, now time.Time) string {
	base := archivePrefix + now.UTC().Format("20060102T150405Z")
	for index := 0; ; index++ {
		name := base + archiveSuffix
		if index > 0 {
			name = fmt.Sprintf("%s-%d%s", base, index, archiveSuffix)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return path
		}
	}
}

func tempArchivePath(dir string) (string, error) {
	file, err := os.CreateTemp(dir, ".celine-*.tar.gz.tmp")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}
