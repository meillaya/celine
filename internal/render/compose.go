// Package render turns celine.toml into deterministic deployment artifacts.
package render

import (
	"fmt"
	"path/filepath"

	"github.com/Meillaya/celine/internal/config"
	"gopkg.in/yaml.v3"
)

// Compose renders the Docker Compose document used by the local deployment.
// The output contains no secrets.
func Compose(cfg *config.Config, uid, gid int) ([]byte, error) {
	services := map[string]any{}
	if cfg.Calibre.Enable {
		services["calibre"] = calibreService(cfg, uid, gid)
	}
	if cfg.Kavita.Enable {
		services["kavita"] = kavitaService(cfg, uid, gid)
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("no services enabled")
	}
	doc := map[string]any{
		"name":     "celine",
		"services": services,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("render compose: %w", err)
	}
	return out, nil
}

const (
	internalCalibrePort = 8080
	internalKavitaPort  = 5000
)

func calibreService(cfg *config.Config, uid, gid int) map[string]any {
	command := []string{
		fmt.Sprintf("--port=%d", internalCalibrePort),
		"--listen-on=0.0.0.0",
		"--disable-use-bonjour",
		"--",
	}
	command = append(command, cfg.CalibreLibraryContainerPaths()...)

	service := map[string]any{
		"image":   cfg.CalibreImage(),
		"restart": "unless-stopped",
		"build": map[string]any{
			"context":    filepath.Join(cfg.Root, "docker"),
			"dockerfile": "calibre.Dockerfile",
			"args": map[string]string{
				"CALIBRE_VERSION": cfg.Calibre.Version,
			},
		},
		"command": command,
		"environment": map[string]string{
			"CALIBRE_CONFIG_DIRECTORY": "/config",
			"HOME":                     "/config",
			"TZ":                       cfg.Timezone(),
		},
		"volumes": []string{
			// Calibre probes case sensitivity by creating a temporary file in
			// each library root, so the mount must be writable. The server has no
			// auth and no local-write flag, so the HTTP surface stays read-only.
			cfg.Resolved.MediaRoot + ":/media",
			cfg.Resolved.CalibreConfigDir + ":/config",
		},
		"ports": []string{
			fmt.Sprintf("127.0.0.1:%d:%d", cfg.Calibre.Port, internalCalibrePort),
		},
		"healthcheck": map[string]any{
			"test":         []string{"CMD-SHELL", fmt.Sprintf("curl -fsS http://127.0.0.1:%d/ >/dev/null || exit 1", internalCalibrePort)},
			"interval":     "30s",
			"timeout":      "10s",
			"retries":      3,
			"start_period": "40s",
		},
		"logging": logConfig(),
	}
	if !cfg.Runtime.Rootless {
		service["user"] = fmt.Sprintf("%d:%d", uid, gid)
	}
	return service
}

func kavitaService(cfg *config.Config, uid, gid int) map[string]any {
	puid, pgid := "0", "0"
	if !cfg.Runtime.Rootless {
		puid, pgid = fmt.Sprintf("%d", uid), fmt.Sprintf("%d", gid)
	}
	return map[string]any{
		"image":   cfg.Kavita.Image,
		"restart": "unless-stopped",
		"environment": map[string]string{
			"PUID": puid,
			"PGID": pgid,
			"TZ":   cfg.Timezone(),
		},
		"volumes": []string{
			cfg.Resolved.KavitaConfigDir + ":/config",
			cfg.KavitaMediaHostPath() + ":" + cfg.Kavita.LibraryMount + ":rw",
		},
		"ports": []string{
			fmt.Sprintf("127.0.0.1:%d:%d", cfg.Kavita.Port, internalKavitaPort),
		},
		"healthcheck": map[string]any{
			"test":         []string{"CMD-SHELL", fmt.Sprintf("curl -fsS http://127.0.0.1:%d/api/health >/dev/null || exit 1", internalKavitaPort)},
			"interval":     "30s",
			"timeout":      "10s",
			"retries":      5,
			"start_period": "60s",
		},
		"logging": logConfig(),
	}
}

func logConfig() map[string]any {
	return map[string]any{
		"driver": "json-file",
		"options": map[string]string{
			"max-size": "10m",
			"max-file": "3",
		},
	}
}
