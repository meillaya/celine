package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SystemdOptions describes the host run that boots the container stack.
type SystemdOptions struct {
	CelineBin  string
	ConfigPath string
	User       string
	Group      string
	Home       string
	DockerHost string
	MediaRoot  string
	StateDir   string
	BackupDir  string
	OnCalendar string
	PathEnv    string
	// Rootless selects the rootless Podman boot wiring: the user manager must
	// be running before the socket exists, so the unit waits for user@UID.
	Rootless bool
	UID      int
	// ExtraEnv carries SOPS/SecretSpec settings from the render environment so
	// the boot unit resolves the same provider and identity as the operator.
	ExtraEnv map[string]string
	// BackupEnabled renders and installs the backup units. When false only the
	// stack unit is generated.
	BackupEnabled bool
}

// SystemdUnits renders the host units that start the stack at boot, take a
// nightly backup, and keep working without an interactive login.
func SystemdUnits(opts SystemdOptions) (map[string]string, error) {
	if opts.CelineBin == "" || opts.ConfigPath == "" {
		return nil, fmt.Errorf("celine binary and config path are required")
	}
	onCalendar := opts.OnCalendar
	if onCalendar == "" {
		onCalendar = "daily"
	}

	environment := []string{
		"Environment=" + unitAssignment("HOME", opts.Home),
	}
	if opts.PathEnv != "" {
		// System units do not inherit the operator's shell PATH. Capture it at
		// render time so secretspec, sops, sqlite3, docker, and tailscale resolve
		// the same way they did when the units were generated.
		environment = append(environment, "Environment="+unitAssignment("PATH", opts.PathEnv))
	}
	if opts.DockerHost != "" {
		environment = append(environment, "Environment="+unitAssignment("DOCKER_HOST", opts.DockerHost))
	}
	if opts.Rootless && opts.UID > 0 {
		environment = append(environment, "Environment="+unitAssignment("XDG_RUNTIME_DIR", fmt.Sprintf("/run/user/%d", opts.UID)))
	}
	extraNames := make([]string, 0, len(opts.ExtraEnv))
	for name := range opts.ExtraEnv {
		extraNames = append(extraNames, name)
	}
	sort.Strings(extraNames)
	for _, name := range extraNames {
		environment = append(environment, "Environment="+unitAssignment(name, opts.ExtraEnv[name]))
	}

	after := []string{"network-online.target", "tailscaled.service"}
	if opts.Rootless && opts.UID > 0 {
		after = append(after, fmt.Sprintf("user@%d.service", opts.UID))
	}
	mounts := requiredMounts(opts)
	startPre := ""
	if opts.Rootless && strings.HasPrefix(opts.DockerHost, "unix://") {
		socket := strings.TrimPrefix(opts.DockerHost, "unix://")
		if socket != "" {
			// The socket is provided by the user manager. After a cold boot it
			// can appear slightly after the system unit starts, so wait instead
			// of failing the whole boot.
			startPre = "ExecStartPre=/bin/sh -c 'for i in $(seq 1 30); do [ -S \"$1\" ] && exit 0; sleep 2; done; exit 1' sh " + unitValue(socket)
		}
	}

	unit := []string{
		"[Unit]",
		"Description=Celine Calibre and Kavita stack",
		"After=" + strings.Join(after, " "),
		"Wants=" + strings.Join(after, " "),
		"StartLimitIntervalSec=300",
		"StartLimitBurst=10",
	}
	if mounts != "" {
		unit = append(unit, "RequiresMountsFor="+mounts)
	}
	unit = append(unit,
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"User="+unitValue(opts.User),
		"Group="+unitValue(opts.Group),
		"WorkingDirectory="+unitValue(opts.Home),
		strings.Join(environment, "\n"),
	)
	if startPre != "" {
		unit = append(unit, startPre)
	}
	unit = append(unit,
		fmt.Sprintf("ExecStart=%s up --config %s", unitValue(opts.CelineBin), unitValue(opts.ConfigPath)),
		fmt.Sprintf("ExecStop=%s down --config %s", unitValue(opts.CelineBin), unitValue(opts.ConfigPath)),
		"Restart=on-failure",
		"RestartSec=20",
		"TimeoutStartSec=600",
		"TimeoutStopSec=120",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	)
	units := map[string]string{"celine.service": strings.Join(unit, "\n")}
	if !opts.BackupEnabled {
		return units, nil
	}

	backupUnit := []string{
		"[Unit]",
		"Description=Celine state backup",
	}
	if mounts != "" {
		backupUnit = append(backupUnit, "RequiresMountsFor="+mounts)
	}
	backupUnit = append(backupUnit,
		"",
		"[Service]",
		"Type=oneshot",
		"User="+unitValue(opts.User),
		"Group="+unitValue(opts.Group),
		"WorkingDirectory="+unitValue(opts.Home),
		strings.Join(environment, "\n"),
		fmt.Sprintf("ExecStart=%s backup --config %s", unitValue(opts.CelineBin), unitValue(opts.ConfigPath)),
		"Nice=10",
		"TimeoutStartSec=1800",
		"",
	)
	timer := []string{
		"[Unit]",
		"Description=Celine state backup timer",
		"",
		"[Timer]",
		"OnCalendar=" + onCalendar,
		"Persistent=true",
		"Unit=celine-backup.service",
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}
	units["celine-backup.service"] = strings.Join(backupUnit, "\n")
	units["celine-backup.timer"] = strings.Join(timer, "\n")
	return units, nil
}

func requiredMounts(opts SystemdOptions) string {
	seen := map[string]bool{}
	var mounts []string
	for _, path := range []string{opts.MediaRoot, opts.StateDir, opts.BackupDir} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		mounts = append(mounts, unitValue(path))
	}
	return strings.Join(mounts, " ")
}

// unitValue quotes a systemd unit value when it contains whitespace or
// characters that would otherwise split the value. The project recommends
// simple paths, but media roots and homes are user-controlled.
func unitValue(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'\\") {
		return value
	}
	return strconv.Quote(value)
}

func unitAssignment(name, value string) string {
	return unitValue(name + "=" + value)
}
