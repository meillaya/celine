package render

import (
	"fmt"
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
		fmt.Sprintf("Environment=HOME=%s", opts.Home),
	}
	if opts.DockerHost != "" {
		environment = append(environment, fmt.Sprintf("Environment=DOCKER_HOST=%s", opts.DockerHost))
	}

	service := strings.Join([]string{
		"[Unit]",
		"Description=Celine Calibre and Kavita stack",
		"After=network-online.target tailscaled.service",
		"Wants=network-online.target tailscaled.service",
		"RequiresMountsFor=" + opts.MediaRoot,
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"User=" + opts.User,
		"Group=" + opts.Group,
		"WorkingDirectory=" + opts.Home,
		strings.Join(environment, "\n"),
		fmt.Sprintf("ExecStart=%s up --config %s", opts.CelineBin, opts.ConfigPath),
		fmt.Sprintf("ExecStop=%s down --config %s", opts.CelineBin, opts.ConfigPath),
		"Restart=on-failure",
		"RestartSec=20",
		"TimeoutStartSec=600",
		"TimeoutStopSec=120",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")

	backupService := strings.Join([]string{
		"[Unit]",
		"Description=Celine state backup",
		"RequiresMountsFor=" + opts.MediaRoot,
		"",
		"[Service]",
		"Type=oneshot",
		"User=" + opts.User,
		"Group=" + opts.Group,
		"WorkingDirectory=" + opts.Home,
		strings.Join(environment, "\n"),
		fmt.Sprintf("ExecStart=%s backup --config %s", opts.CelineBin, opts.ConfigPath),
		"Nice=10",
		"TimeoutStartSec=1800",
		"",
	}, "\n")

	timer := strings.Join([]string{
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
	}, "\n")

	return map[string]string{
		"celine.service":        service,
		"celine-backup.service": backupService,
		"celine-backup.timer":   timer,
	}, nil
}
