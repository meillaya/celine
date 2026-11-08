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

	"gopkg.in/yaml.v3"

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
	TokenMatches    *bool
	Discoveries     []string
}

// Run copies the legacy config into the state directory and seeds SecretSpec.
// Preflight, staging, integrity checks, the legacy archive, and the signing-key
// import all happen before the live state directory is replaced.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Runner == nil {
		return Result{}, fmt.Errorf("migration requires a command runner")
	}
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
	legacyDB := filepath.Join(opts.LegacyConfigDir, "kavita.db")
	if info, err := os.Stat(legacyDB); err != nil || info.IsDir() {
		return Result{}, fmt.Errorf("legacy Kavita database not found at %s", legacyDB)
	}

	targetDir := filepath.Join(opts.StateDir, "kavita", "config")
	exists, content, err := pathState(targetDir)
	if err != nil {
		return Result{}, err
	}
	if exists && content && !opts.Force {
		return Result{}, fmt.Errorf("state already contains files at %s\nrerun with --force to replace it (the legacy config is still untouched)", targetDir)
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create backup directory: %w", err)
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
	token, tokenErr := readTokenKey(filepath.Join(stagedConfig, "appsettings.json"))
	tokenPresent := tokenErr == nil && strings.TrimSpace(token) != ""

	stamp := now().UTC().Format("20060102T150405Z")
	backupPath := uniquePath(opts.BackupDir, "legacy-config-"+stamp, ".tar.gz")
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

	result := Result{
		BackupArchive:   backupPath,
		StateConfigDir:  targetDir,
		KavitaVersion:   inventory.Version,
		KavitaUsers:     len(inventory.Users),
		KavitaLibraries: len(inventory.Libraries),
		TokenPresent:    tokenPresent,
	}
	if opts.ImportToken && tokenPresent {
		imported, matches, err := ensureSecret(ctx, opts, sops, token)
		if err != nil {
			return result, fmt.Errorf("%w; state was not replaced (legacy archive: %s)", err, backupPath)
		}
		result.TokenImported = imported
		result.TokenMatches = matches
		if imported {
			result.Discoveries = append(result.Discoveries, "legacy Kavita signing key imported into SecretSpec/SOPS; existing sessions stay valid")
		} else if matches != nil && *matches {
			result.Discoveries = append(result.Discoveries, "existing SOPS store already contains the legacy signing key; left unchanged")
		} else {
			result.Discoveries = append(result.Discoveries, "existing SOPS store contains a different signing key; left unchanged, so existing sessions will be invalidated")
		}
	} else if opts.ImportToken && !tokenPresent {
		result.Discoveries = append(result.Discoveries, "legacy appsettings.json has no readable TokenKey; SecretSpec will generate one on first start")
	}

	if err := os.MkdirAll(filepath.Dir(targetDir), 0o755); err != nil {
		return result, err
	}
	if exists && content {
		preMigrate := uniquePath(filepath.Join(opts.StateDir, "kavita"), "config.pre-migrate-"+stamp, "")
		if err := os.Rename(targetDir, preMigrate); err != nil {
			return result, fmt.Errorf("move existing state aside: %w", err)
		}
		if err := os.Rename(stagedConfig, targetDir); err != nil {
			// Put the old state back so a failed install never leaves a
			// half-migrated deployment.
			_ = os.Rename(preMigrate, targetDir)
			return result, fmt.Errorf("install migrated config: %w", err)
		}
	} else {
		if exists {
			if err := os.Remove(targetDir); err != nil {
				return result, fmt.Errorf("replace empty state directory: %w", err)
			}
		}
		if err := os.Rename(stagedConfig, targetDir); err != nil {
			return result, fmt.Errorf("install migrated config: %w", err)
		}
	}

	if err := os.Chmod(targetDir, 0o700); err != nil {
		result.Discoveries = append(result.Discoveries, fmt.Sprintf("could not secure %s: %v", targetDir, err))
	}
	for _, name := range []string{"appsettings.json", "kavita.db"} {
		path := filepath.Join(targetDir, name)
		if _, err := os.Stat(path); err == nil {
			if err := os.Chmod(path, 0o600); err != nil {
				result.Discoveries = append(result.Discoveries, fmt.Sprintf("could not secure %s: %v", path, err))
			}
		}
	}

	marker := map[string]any{
		"migrated_at":      now().UTC().Format(time.RFC3339),
		"source":           opts.LegacyConfigDir,
		"backup":           backupPath,
		"kavita_version":   result.KavitaVersion,
		"kavita_users":     result.KavitaUsers,
		"kavita_libraries": result.KavitaLibraries,
		"token_imported":   result.TokenImported,
		"token_present":    result.TokenPresent,
	}
	if result.TokenMatches != nil {
		marker["token_matches"] = *result.TokenMatches
	}
	markerBytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		result.Discoveries = append(result.Discoveries, "could not marshal migration marker: "+err.Error())
	} else if err := os.WriteFile(filepath.Join(opts.StateDir, ".migration.json"), append(markerBytes, '\n'), 0o600); err != nil {
		result.Discoveries = append(result.Discoveries, "could not write migration marker: "+err.Error())
	}
	return result, nil
}

// pathState reports whether path exists and whether it is a non-empty
// directory. A regular file is treated as content so --force can replace it.
func pathState(path string) (exists, content bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}
	if !info.IsDir() {
		return true, true, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return true, true, err
	}
	return true, len(entries) > 0, nil
}

func uniquePath(dir, base, suffix string) string {
	for index := 0; ; index++ {
		name := base + suffix
		if index > 0 {
			name = fmt.Sprintf("%s-%d%s", base, index, suffix)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return path
		}
	}
}

// ensureSecret returns imported=true when it wrote a new SOPS store. When the
// store already exists it is decrypted and compared, never rewritten.
func ensureSecret(ctx context.Context, opts Options, sops, token string) (bool, *bool, error) {
	if info, err := os.Stat(opts.SecretFile); err == nil && !info.IsDir() {
		decrypted, err := opts.Runner.Run(ctx, sops, []string{"--decrypt", opts.SecretFile}, execx.Options{Dir: opts.Root})
		if err != nil {
			return false, nil, fmt.Errorf("existing secret store %s cannot be decrypted: %w", opts.SecretFile, err)
		}
		stored, err := tokenFromStore(decrypted.Stdout)
		if err != nil {
			return false, nil, fmt.Errorf("existing secret store %s is malformed: %w", opts.SecretFile, err)
		}
		match := stored == token
		return false, &match, nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.SecretFile), 0o700); err != nil {
		return false, nil, err
	}
	plain, err := os.CreateTemp(opts.StateDir, ".legacy-secret-*.yaml")
	if err != nil {
		return false, nil, err
	}
	plainPath := plain.Name()
	defer os.Remove(plainPath)
	if err := plain.Chmod(0o600); err != nil {
		plain.Close()
		return false, nil, err
	}
	content := "celine:\n  default:\n    KAVITA_TOKEN_KEY: " + strconv.Quote(token) + "\n"
	if _, err := plain.WriteString(content); err != nil {
		plain.Close()
		return false, nil, err
	}
	if err := plain.Close(); err != nil {
		return false, nil, err
	}

	encrypted, err := os.CreateTemp(filepath.Dir(opts.SecretFile), ".celine-secret-*.yaml")
	if err != nil {
		return false, nil, err
	}
	encryptedPath := encrypted.Name()
	if err := encrypted.Close(); err != nil {
		os.Remove(encryptedPath)
		return false, nil, err
	}
	os.Remove(encryptedPath)
	defer os.Remove(encryptedPath)

	args := []string{
		"encrypt",
		"--filename-override", "secrets/celine.enc.yaml",
		"--output", encryptedPath,
		plainPath,
	}
	if _, err := opts.Runner.Run(ctx, sops, args, execx.Options{Dir: opts.Root}); err != nil {
		return false, nil, fmt.Errorf("encrypt legacy signing key with SOPS: %w\nhint: ensure %s/.sops.yaml matches secrets/celine.enc.yaml and the age key is available", err, opts.Root)
	}
	if err := os.Chmod(encryptedPath, 0o600); err != nil {
		return false, nil, err
	}
	decrypted, err := opts.Runner.Run(ctx, sops, []string{"--decrypt", encryptedPath}, execx.Options{Dir: opts.Root})
	if err != nil {
		return false, nil, fmt.Errorf("verify encrypted signing key: %w", err)
	}
	stored, err := tokenFromStore(decrypted.Stdout)
	if err != nil {
		return false, nil, fmt.Errorf("verify encrypted signing key: %w", err)
	}
	if stored != token {
		return false, nil, fmt.Errorf("encrypted signing key did not round-trip to the legacy value")
	}
	if err := os.Rename(encryptedPath, opts.SecretFile); err != nil {
		return false, nil, fmt.Errorf("install encrypted signing key: %w", err)
	}
	if err := os.Chmod(opts.SecretFile, 0o600); err != nil {
		return false, nil, err
	}
	match := true
	return true, &match, nil
}

func tokenFromStore(data []byte) (string, error) {
	var store struct {
		Celine map[string]map[string]string `yaml:"celine"`
	}
	if err := yaml.Unmarshal(data, &store); err != nil {
		return "", err
	}
	profiles := store.Celine
	if profiles == nil {
		return "", fmt.Errorf("store has no celine section")
	}
	keys := profiles["default"]
	if keys == nil {
		return "", fmt.Errorf("store has no default profile")
	}
	token := strings.TrimSpace(keys["KAVITA_TOKEN_KEY"])
	if token == "" {
		return "", fmt.Errorf("store does not contain KAVITA_TOKEN_KEY")
	}
	return token, nil
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
