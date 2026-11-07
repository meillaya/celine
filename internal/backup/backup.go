// Package backup archives the configuration state that cannot be recreated:
// the Kavita database and app settings, bookmarks, and Calibre server config.
// Media files are never archived.
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

// Options configures one backup run.
type Options struct {
	KavitaConfigDir  string
	CalibreConfigDir string
	BackupDir        string
	ConfigPath       string
	Retention        int
	ToolVersion      string
	Runner           execx.Runner
	SQLite           string
	Now              func() time.Time
}

// Manifest is stored inside every archive.
type Manifest struct {
	Format          int      `json:"format"`
	Tool            string   `json:"tool"`
	Version         string   `json:"version"`
	CreatedAt       string   `json:"created_at"`
	ConfigSHA256    string   `json:"config_sha256,omitempty"`
	KavitaVersion   string   `json:"kavita_version,omitempty"`
	KavitaUsers     int      `json:"kavita_users"`
	KavitaLibraries int      `json:"kavita_libraries"`
	Files           []string `json:"files"`
	Excluded        []string `json:"excluded,omitempty"`
}

// Result describes a completed backup.
type Result struct {
	Archive string
	Files   int
	Bytes   int64
}

// Run creates an archive and prunes old ones.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Runner == nil {
		return Result{}, fmt.Errorf("backup requires a command runner")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	sqlite := opts.SQLite
	if sqlite == "" {
		sqlite = "sqlite3"
	}
	dbPath := filepath.Join(opts.KavitaConfigDir, "kavita.db")
	if info, err := os.Stat(dbPath); err != nil || info.IsDir() {
		return Result{}, fmt.Errorf("Kavita database not found at %s; run `celine migrate` first", dbPath)
	}
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create backup directory: %w", err)
	}

	stage, err := os.MkdirTemp(opts.BackupDir, ".stage-")
	if err != nil {
		return Result{}, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	inventory, err := kavita.ReadInventory(ctx, opts.Runner, sqlite, dbPath)
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
	if err := kavita.IntegrityCheck(ctx, opts.Runner, sqlite, filepath.Join(kavitaDest, "kavita.db")); err != nil {
		return Result{}, fmt.Errorf("staged database failed its integrity check: %w", err)
	}

	for _, name := range []string{"appsettings.json"} {
		src := filepath.Join(opts.KavitaConfigDir, name)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Result{}, fmt.Errorf("stat %s: %w", name, err)
		}
		if err := archiveutil.CopyFile(src, filepath.Join(kavitaDest, name), 0o600); err != nil {
			return Result{}, fmt.Errorf("copy %s: %w", name, err)
		}
	}
	bookmarks := filepath.Join(opts.KavitaConfigDir, "bookmarks")
	if info, err := os.Stat(bookmarks); err == nil && info.IsDir() {
		if err := archiveutil.CopyTree(bookmarks, filepath.Join(kavitaDest, "bookmarks"), nil); err != nil {
			return Result{}, fmt.Errorf("copy bookmarks: %w", err)
		}
	}
	if opts.CalibreConfigDir != "" {
		if info, err := os.Stat(opts.CalibreConfigDir); err == nil && info.IsDir() {
			skip := func(rel string) bool {
				base := filepath.Base(rel)
				return base == "cache" || base == "caches" || base == "logs" || strings.HasSuffix(base, ".log")
			}
			if err := archiveutil.CopyTree(opts.CalibreConfigDir, filepath.Join(stage, "calibre"), skip); err != nil {
				return Result{}, fmt.Errorf("copy Calibre config: %w", err)
			}
		}
	}

	files, err := archiveutil.FilesUnder(stage)
	if err != nil {
		return Result{}, err
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
		KavitaVersion:   inventory.Version,
		KavitaUsers:     len(inventory.Users),
		KavitaLibraries: len(inventory.Libraries),
		Files:           files,
		Excluded:        []string{"kavita caches and covers", "logs", "media libraries"},
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), append(manifestBytes, '\n'), 0o600); err != nil {
		return Result{}, fmt.Errorf("write manifest: %w", err)
	}

	stamp := opts.Now().UTC().Format("20060102T150405Z")
	name := archivePrefix + stamp + archiveSuffix
	final := filepath.Join(opts.BackupDir, name)
	tmp := final + ".tmp"
	if err := archiveutil.TarGz(stage, tmp); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return Result{}, fmt.Errorf("install backup archive: %w", err)
	}
	info, err := os.Stat(final)
	if err != nil {
		return Result{}, err
	}
	if err := prune(opts.BackupDir, opts.Retention); err != nil {
		return Result{}, err
	}
	fileCount := len(files) + 1
	return Result{Archive: final, Files: fileCount, Bytes: info.Size()}, nil
}

// List returns archive paths, newest first.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var archives []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, archivePrefix) || !strings.HasSuffix(name, archiveSuffix) {
			continue
		}
		archives = append(archives, filepath.Join(dir, name))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(archives)))
	return archives, nil
}

// Latest returns the newest archive, if any.
func Latest(dir string) (string, error) {
	archives, err := List(dir)
	if err != nil || len(archives) == 0 {
		return "", err
	}
	return archives[0], nil
}

func prune(dir string, retention int) error {
	if retention < 1 {
		return fmt.Errorf("retention must be at least 1")
	}
	archives, err := List(dir)
	if err != nil {
		return err
	}
	for _, archive := range archives[min(len(archives), retention):] {
		if err := os.Remove(archive); err != nil {
			return fmt.Errorf("prune %s: %w", archive, err)
		}
	}
	return nil
}

// RestoreOptions configures an archive restore.
type RestoreOptions struct {
	Archive  string
	StateDir string
	Force    bool
}

// RestoreResult describes a completed restore.
type RestoreResult struct {
	Manifest Manifest
	Files    int
}

// Restore extracts an archive over the state directory. Callers must stop the
// services and take a pre-restore backup first.
func Restore(_ context.Context, opts RestoreOptions) (RestoreResult, error) {
	if opts.Archive == "" {
		return RestoreResult{}, fmt.Errorf("archive path is required")
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		return RestoreResult{}, err
	}
	staging, err := os.MkdirTemp(opts.StateDir, ".restore-")
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.RemoveAll(staging)
	if err := archiveutil.ExtractTarGz(opts.Archive, staging); err != nil {
		return RestoreResult{}, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	if err != nil {
		return RestoreResult{}, fmt.Errorf("archive has no manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return RestoreResult{}, fmt.Errorf("parse archive manifest: %w", err)
	}
	if manifest.Format != formatVersion {
		return RestoreResult{}, fmt.Errorf("unsupported archive format %d", manifest.Format)
	}
	targetDB := filepath.Join(opts.StateDir, "kavita", "config", "kavita.db")
	if _, err := os.Stat(targetDB); err == nil && !opts.Force {
		return RestoreResult{}, fmt.Errorf("refusing to overwrite existing Kavita database at %s without --force", targetDB)
	}

	files := 0
	for source, target := range map[string]string{
		filepath.Join(staging, "kavita", "config"): filepath.Join(opts.StateDir, "kavita", "config"),
		filepath.Join(staging, "calibre"):          filepath.Join(opts.StateDir, "calibre"),
	} {
		if _, err := os.Stat(source); err != nil {
			continue
		}
		list, err := archiveutil.FilesUnder(source)
		if err != nil {
			return RestoreResult{}, err
		}
		if err := archiveutil.CopyTree(source, target, nil); err != nil {
			return RestoreResult{}, err
		}
		files += len(list)
	}
	return RestoreResult{Manifest: manifest, Files: files}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
