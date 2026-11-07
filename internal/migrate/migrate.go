// Package migrate imports the legacy Kavita state and signing key into the
// declarative layout without mutating the original files.
package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Meillaya/celine/internal/archiveutil"
	"github.com/Meillaya/celine/internal/execx"
	"github.com/Meillaya/celine/internal/kavita"
)

// Options configures a migration.
type Options struct {
	Root            string
	LegacyConfigDir string
	StateDir        string
	BackupDir       string
	SecretFile      string
	Runner          execx.Runner
	SQLite          string
	Sops            string
	ToolVersion     string
	Force           bool
	ImportToken     bool
	Now             func() time.Time
}

// Result describes a completed migration.
type Result struct {
	BackupArchive   string
	StateConfigDir  string
	KavitaVersion   string
	KavitaUsers     int
	KavitaLibraries int
	TokenImported   bool
	TokenPresent    bool
	Discoveries     []string
}

// Run copies the legacy config into the state directory and seeds SecretSpec.
func Run(ctx context.Context, opts Options) (Result, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sqlite := opts.SQLite
	if sqlite == "" {
		sqlite = "sqlite3"
	}
	sops := opts.Sops
	if sops == "" {
		sops = "sops"
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create state directory: %w", err)
	}
	legacyDB := filepath.Join(opts.LegacyConfigDir, "kavita.db")
	if info, err := os.Stat(legacyDB); err != nil || info.IsDir() {
		return Result{}, fmt.Errorf("legacy Kavita database not found at %s", legacyDB)
	}
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create backup directory: %w", err)
	}

	stamp := now().UTC().Format("20060102T150405Z")
	backupPath := filepath.Join(opts.BackupDir, "legacy-config-"+stamp+".tar.gz")
	tmpBackup := backupPath + ".tmp"
	if err := archiveutil.TarGz(opts.LegacyConfigDir, tmpBackup); err != nil {
		return Result{}, fmt.Errorf("archive legacy config: %w", err)
	}
	if err := os.Rename(tmpBackup, backupPath); err != nil {
		os.Remove(tmpBackup)
		return Result{}, err
	}
	entries, err := archiveutil.ListTarGz(backupPath)
	if err != nil {
		return Result{}, fmt.Errorf("verify legacy archive: %w", err)
	}
	foundDB := false
	for _, entry := range entries {
		if entry == "kavita.db" {
			foundDB = true
			break
		}
	}
	if !foundDB {
		return Result{}, fmt.Errorf("legacy archive %s does not contain kavita.db", backupPath)
	}

	targetDir := filepath.Join(opts.StateDir, "kavita", "config")
	targetDB := filepath.Join(targetDir, "kavita.db")
	targetExists := false
	if _, err := os.Stat(targetDB); err == nil {
		targetExists = true
		if !opts.Force {
			return Result{}, fmt.Errorf("state already contains a Kavita database at %s\nrerun with --force to replace it (the legacy config is still untouched)", targetDB)
		}
	}

	staging, err := os.MkdirTemp(opts.StateDir, ".migrate-")
	if err != nil {
		return Result{}, fmt.Errorf("create staging directory (does %s exist?): %w", opts.StateDir, err)
	}
	defer os.RemoveAll(staging)
	stagedConfig := filepath.Join(staging, "kavita", "config")
	if err := archiveutil.CopyTree(opts.LegacyConfigDir, stagedConfig, nil); err != nil {
		return Result{}, fmt.Errorf("copy legacy config: %w", err)
	}
	if err := kavita.IntegrityCheck(ctx, opts.Runner, sqlite, filepath.Join(stagedConfig, "kavita.db")); err != nil {
		return Result{}, fmt.Errorf("copied database failed its integrity check: %w", err)
	}
	inventory, err := kavita.ReadInventory(ctx, opts.Runner, sqlite, filepath.Join(stagedConfig, "kavita.db"))
	if err != nil {
		return Result{}, err
	}
	if len(inventory.Users) == 0 {
		return Result{}, fmt.Errorf("copied database has no users; refusing to install it")
	}

	if targetExists {
		preMigrate := filepath.Join(opts.StateDir, "kavita", "config.pre-migrate-"+stamp)
		if err := os.Rename(targetDir, preMigrate); err != nil {
			return Result{}, fmt.Errorf("move existing state aside: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(targetDir), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.Rename(stagedConfig, targetDir); err != nil {
		return Result{}, fmt.Errorf("install migrated config: %w", err)
	}

	result := Result{
		BackupArchive:   backupPath,
		StateConfigDir:  targetDir,
		KavitaVersion:   inventory.Version,
		KavitaUsers:     len(inventory.Users),
		KavitaLibraries: len(inventory.Libraries),
	}
	if !opts.ImportToken {
		return result, nil
	}
	token, err := readTokenKey(filepath.Join(targetDir, "appsettings.json"))
	if err != nil {
		result.Discoveries = append(result.Discoveries, "legacy appsettings.json has no readable TokenKey; SecretSpec will generate one on first start")
		return result, nil
	}
	result.TokenPresent = true

	present, importErr := ensureSecret(ctx, opts, sops, token)
	if importErr != nil {
		result.Discoveries = append(result.Discoveries, importErr.Error())
		return result, nil
	}
	result.TokenImported = present
	if present {
		result.Discoveries = append(result.Discoveries, "legacy Kavita signing key imported into SecretSpec/SOPS; existing sessions stay valid")
	}

	marker := map[string]any{
		"migrated_at":      now().UTC().Format(time.RFC3339),
		"source":           opts.LegacyConfigDir,
		"backup":           backupPath,
		"kavita_version":   result.KavitaVersion,
		"kavita_users":     result.KavitaUsers,
		"kavita_libraries": result.KavitaLibraries,
		"token_imported":   result.TokenImported,
	}
	markerBytes, _ := json.MarshalIndent(marker, "", "  ")
	_ = os.WriteFile(filepath.Join(opts.StateDir, ".migration.json"), append(markerBytes, '\n'), 0o600)
	return result, nil
}

// ensureSecret returns true when the SOPS file now exists with a usable token.
func ensureSecret(ctx context.Context, opts Options, sops, token string) (bool, error) {
	if info, err := os.Stat(opts.SecretFile); err == nil && !info.IsDir() {
		if _, err := opts.Runner.Run(ctx, sops, []string{"--decrypt", opts.SecretFile}, execx.Options{Dir: opts.Root}); err != nil {
			return false, fmt.Errorf("existing secret store %s cannot be decrypted: %w", opts.SecretFile, err)
		}
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.SecretFile), 0o700); err != nil {
		return false, err
	}
	plain, err := os.CreateTemp(opts.StateDir, ".legacy-secret-*.yaml")
	if err != nil {
		return false, err
	}
	plainPath := plain.Name()
	defer os.Remove(plainPath)
	if err := plain.Chmod(0o600); err != nil {
		plain.Close()
		return false, err
	}
	content := "celine:\n  default:\n    KAVITA_TOKEN_KEY: " + strconv.Quote(token) + "\n"
	if _, err := plain.WriteString(content); err != nil {
		plain.Close()
		return false, err
	}
	if err := plain.Close(); err != nil {
		return false, err
	}

	args := []string{
		"encrypt",
		"--filename-override", "secrets/celine.enc.yaml",
		"--output", opts.SecretFile,
		plainPath,
	}
	if _, err := opts.Runner.Run(ctx, sops, args, execx.Options{Dir: opts.Root}); err != nil {
		return false, fmt.Errorf("encrypt legacy signing key with SOPS: %w\nhint: ensure %s/.sops.yaml matches secrets/celine.enc.yaml and the age key is available", err, opts.Root)
	}
	if err := os.Chmod(opts.SecretFile, 0o600); err != nil {
		return false, err
	}
	decrypted, err := opts.Runner.Run(ctx, sops, []string{"--decrypt", opts.SecretFile}, execx.Options{Dir: opts.Root})
	if err != nil {
		return false, fmt.Errorf("verify encrypted signing key: %w", err)
	}
	if !strings.Contains(string(decrypted.Stdout), "KAVITA_TOKEN_KEY") {
		return false, fmt.Errorf("encrypted signing key did not contain the expected key name")
	}
	return true, nil
}

func readTokenKey(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var settings struct {
		TokenKey string `json:"TokenKey"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", err
	}
	if strings.TrimSpace(settings.TokenKey) == "" {
		return "", fmt.Errorf("TokenKey is empty")
	}
	return settings.TokenKey, nil
}
