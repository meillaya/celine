package render

import (
	"encoding/json"
	"fmt"

	"github.com/Meillaya/celine/internal/config"
)

// Target selects the process layout that receives the settings.
type Target string

const (
	// TargetDocker is the container layout: the app binds all interfaces inside
	// the container and the host only publishes loopback ports.
	TargetDocker Target = "docker"
	// TargetNative is a host process that should bind loopback directly.
	TargetNative Target = "native"
)

// AppSettings renders Kavita's appsettings.json. Existing unknown keys are
// preserved so upgrades and manually managed settings are not clobbered.
func AppSettings(cfg *config.Config, token string, existing []byte, target Target) ([]byte, error) {
	if token == "" {
		return nil, fmt.Errorf("kavita token key is empty")
	}
	data := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &data); err != nil {
			return nil, fmt.Errorf("parse existing appsettings.json: %w", err)
		}
	}
	if data == nil {
		// A file containing the JSON literal null decodes to a nil map.
		data = map[string]any{}
	}

	ipAddresses := "0.0.0.0,::"
	port := cfg.Kavita.Port
	if target == TargetNative {
		ipAddresses = "127.0.0.1"
	} else {
		// The container always listens on 5000; the host publishes cfg.port.
		port = 5000
	}

	data["TokenKey"] = token
	data["Port"] = port
	data["IpAddresses"] = ipAddresses
	data["BaseUrl"] = cfg.Kavita.BaseURL
	data["Cache"] = cfg.Kavita.Cache
	data["AllowIFraming"] = cfg.Kavita.AllowIFraming

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render appsettings.json: %w", err)
	}
	return append(out, '\n'), nil
}
