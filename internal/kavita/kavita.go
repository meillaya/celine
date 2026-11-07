// Package kavita reads and copies the Kavita SQLite database without starting
// the application.
package kavita

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Meillaya/celine/internal/execx"
)

// User is one Kavita account.
type User struct {
	Id       int    `json:"Id"`
	UserName string `json:"UserName"`
}

// Library is one Kavita library row.
type Library struct {
	Id   int    `json:"Id"`
	Name string `json:"Name"`
	Type int    `json:"Type"`
}

// FolderPath is one library folder row.
type FolderPath struct {
	LibraryId int    `json:"LibraryId"`
	Path      string `json:"Path"`
}

// Inventory is the state used by doctor and migration.
type Inventory struct {
	Users       []User       `json:"users"`
	Libraries   []Library    `json:"libraries"`
	FolderPaths []FolderPath `json:"folderPaths"`
	Version     string       `json:"version"`
}

// ReadInventory queries the database read-only. The sqlite3 binary is passed
// explicitly so tests can fake it and Nix can wrap it.
func ReadInventory(ctx context.Context, runner execx.Runner, sqlite, dbPath string) (Inventory, error) {
	inv := Inventory{}
	if err := queryJSON(ctx, runner, sqlite, dbPath, "SELECT Id, UserName FROM AspNetUsers ORDER BY Id", &inv.Users); err != nil {
		return inv, fmt.Errorf("read Kavita users: %w", err)
	}
	if err := queryJSON(ctx, runner, sqlite, dbPath, "SELECT Id, Name, Type FROM Library ORDER BY Id", &inv.Libraries); err != nil {
		return inv, fmt.Errorf("read Kavita libraries: %w", err)
	}
	if err := queryJSON(ctx, runner, sqlite, dbPath, "SELECT LibraryId, Path FROM FolderPath ORDER BY LibraryId", &inv.FolderPaths); err != nil {
		return inv, fmt.Errorf("read Kavita folder paths: %w", err)
	}
	var version []struct {
		Value string `json:"Value"`
	}
	if err := queryJSON(ctx, runner, sqlite, dbPath, "SELECT COALESCE(Value, '') AS Value FROM ServerSetting WHERE Key = 11", &version); err != nil {
		return inv, fmt.Errorf("read Kavita version: %w", err)
	}
	if len(version) > 0 {
		inv.Version = version[0].Value
	}
	return inv, nil
}

func queryJSON(ctx context.Context, runner execx.Runner, sqlite, dbPath, query string, target any) error {
	result, err := runner.Run(ctx, sqlite, []string{"-readonly", "-json", dbPath, query}, execx.Options{})
	if err != nil {
		return err
	}
	raw := strings.TrimSpace(string(result.Stdout))
	if raw == "" {
		raw = "[]"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("decode sqlite JSON: %w", err)
	}
	return nil
}

// IntegrityCheck verifies the database file.
func IntegrityCheck(ctx context.Context, runner execx.Runner, sqlite, dbPath string) error {
	result, err := runner.Run(ctx, sqlite, []string{"-readonly", dbPath, "PRAGMA integrity_check;"}, execx.Options{})
	if err != nil {
		return err
	}
	out := strings.TrimSpace(string(result.Stdout))
	if out != "ok" {
		return fmt.Errorf("integrity check failed: %s", out)
	}
	return nil
}

// Backup performs an online SQLite backup through the sqlite3 CLI.
func Backup(ctx context.Context, runner execx.Runner, sqlite, dbPath, destination string) error {
	if err := EnsureParent(destination); err != nil {
		return err
	}
	query := fmt.Sprintf(".backup %s", sqliteLiteral(destination))
	if _, err := runner.Run(ctx, sqlite, []string{dbPath, query}, execx.Options{}); err != nil {
		return fmt.Errorf("backup Kavita database: %w", err)
	}
	return nil
}

// EnsureParent creates the directory containing path.
func EnsureParent(path string) error {
	return ensureDir(filepath.Dir(path))
}

// HostPath maps a path written by Kavita (for example /data/Books/X) to the
// host path using the configured runtime mount.
func HostPath(runtimePath, mount, hostRoot string) (string, bool) {
	cleanMount := strings.TrimSuffix(filepath.ToSlash(mount), "/")
	cleanPath := filepath.ToSlash(runtimePath)
	if cleanPath == cleanMount {
		return hostRoot, true
	}
	if !strings.HasPrefix(cleanPath, cleanMount+"/") {
		return "", false
	}
	rest := strings.TrimPrefix(cleanPath, cleanMount+"/")
	return filepath.Join(hostRoot, filepath.FromSlash(rest)), true
}

func sqliteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
