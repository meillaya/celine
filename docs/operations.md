# Operations

This document covers running the deployment: start and stop, status, logs, health checks, Tailscale Serve, backups, restore, updates, systemd units, and troubleshooting. The configuration reference is in [configuration.md](configuration.md). The reason behind ports and paths is in [architecture.md](architecture.md).

## Before you start

1. Mount the external drive. The repository, media, and state live on `/dev/sda1` mounted at `/run/media/mei/Kingston/celine`.

   ```bash
   findmnt -T /run/media/mei/Kingston/celine
   ```

2. Start the rootless Podman socket and point the Docker client at it.

   ```bash
   systemctl --user status podman.socket
   export DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
   ```

3. Confirm the tailnet is up and HTTPS certificates are enabled for the tailnet. Tailscale Serve obtains the certificate automatically.

   ```bash
   tailscale status
   ```

4. Make the SOPS age identity available. The SecretSpec provider decrypts `secrets/celine.enc.yaml` with it. The default SOPS identity location or `SOPS_AGE_KEY_FILE` must point at a usable key.

5. Enter the development shell, or install the package with `nix build .#default`. The shell provides `celine`, `sqlite3`, `sops`, `secretspec`, `tailscale`, `docker`, and the Compose provider.

   ```bash
   cd /run/media/mei/Kingston/celine
   devenv shell
   ```

## Command reference

| Command | Purpose | Command-specific flags |
| --- | --- | --- |
| `celine validate` | Load and validate `celine.toml` and the media paths. | `--no-filesystem` skips the media path checks. |
| `celine render` | Render `build/compose.yaml`. | `--check` fails on drift; `--out DIR` writes to another directory. |
| `celine up` | Render, start the containers, wait for health, apply Tailscale Serve. | `--build` rebuilds the Calibre image; `--no-tailnet` skips Serve. |
| `celine down` | Stop and remove the containers. State and Serve handlers stay. | None. |
| `celine status` | Show service health, tailnet URLs, latest backup, and runtime. | `--json` prints JSON. |
| `celine logs [service]` | Show the last 200 lines for both services or one service. | `-f` follows. |
| `celine tailnet apply\|status\|down` | Install, inspect, or remove the configured Serve handlers. | None. |
| `celine doctor` | Run read-only preflight and health checks. | `--strict` treats warnings as failures; `--json` prints JSON. |
| `celine backup` | Archive state and prune old archives. | `--dry-run`, `--retention N`, `--state-dir PATH`, `--backup-dir PATH`. |
| `celine restore [flags] <archive>` | Restore an archive. | `--yes`, `--dry-run`, `--force`, `--state-dir PATH`. |
| `celine migrate` | Import the legacy `config/` into `state/`. | `--yes`, `--force`, `--no-secret-import`, `--state-dir PATH`, `--backup-dir PATH`, `--secret-file PATH`. |
| `celine secrets materialize` | Resolve one secret into a file with mode 0600. | `--name NAME` (default `KAVITA_TOKEN_KEY`), `--out FILE`, `--profile`, `--provider`. |
| `celine systemd render` | Render boot and backup units into `build/systemd/` by default. | `--out DIR`. |
| `celine systemd install` | Write the rendered units to `/etc/systemd/system` or `--dir DIR`. | `--dir DIR`. |
| `celine version` | Print the version. | None. |

Every command that loads the deployment accepts the common flags:

| Flag | Meaning |
| --- | --- |
| `--config PATH` | Use this `celine.toml` instead of walking up from the working directory. |
| `--root PATH` | Treat `PATH` as the repository root and read `PATH/celine.toml`. |
| `--secretspec PATH` | Use this SecretSpec manifest instead of `secretspec.toml` next to `celine.toml`. |

`logs` and `restore` accept their positional argument before or after the flags, so `celine restore backups/<archive> --yes` and `celine restore --yes backups/<archive>` are equivalent.

## Start and stop

Start the stack:

```bash
celine up
```

`up` renders Compose and `appsettings.json`, starts the containers, waits up to 180 seconds for both loopback health endpoints, applies Tailscale Serve, and prints the URLs. The command exits nonzero when a container or health endpoint fails.

Rebuild the Calibre image before starting when the pinned Calibre version changed or the Dockerfile changed:

```bash
celine up --build
```

Start without touching Tailscale Serve, for example while testing locally:

```bash
celine up --no-tailnet
```

Stop the containers:

```bash
celine down
```

`down` removes the containers and the Compose network. Bind-mounted state and media stay on disk, images stay in the local store, and Tailscale Serve handlers stay installed, so the tailnet URLs return 502 until the next start. Remove the handlers separately:

```bash
celine tailnet down
```

## Status

```bash
celine status
```

The report covers:

- Configuration path.
- Each enabled service: up or down, host loopback port, and HTTP status.
- Tailscale backend state and node name.
- Tailnet URLs for the configured endpoints.
- Latest backup archive.
- Container runtime version and Compose version.

Use `celine status --json` for automation. The JSON contains `config`, `services` (each with `service`, `port`, `reachable`, `status`), `urls`, `tailnet` (backend state, DNS name, certificate domains, raw serve status), `latest_backup`, `runtime`, and `compose_version`. `status` exits 0 even when a service is down; read the report. It validates `celine.toml` but skips the filesystem checks.

## Logs

```bash
celine logs                 # both services, last 200 lines
celine logs kavita          # one service
celine logs -f calibre      # follow
```

The CLI passes `logs --tail=200` to Compose. Container logs use the `json-file` driver with a 10 MB file limit and three files per container, so disk use is bounded.

## Doctor

`celine doctor` runs read-only checks and never changes the system.

```bash
celine doctor            # warnings allowed, failures exit 1
celine doctor --strict   # warnings also exit 1
celine doctor --json     # machine-readable report
```

Levels:

| Level | Meaning |
| --- | --- |
| `ok` | The check passed. |
| `warn` | The deployment can work but needs attention, for example a stale backup or a port owned by another process. |
| `fail` | A required condition is missing, for example a missing Kavita database or an unreachable container runtime. |

Exit codes: 0 when every check is `ok` (or `warn` without `--strict`), 1 otherwise. A configuration load failure exits 1.

Checks and what they report:

| Check | Reports |
| --- | --- |
| `configuration` | Loaded `celine.toml` path. |
| `media root` | Mount source and filesystem, or a warning when the path is not a separate mount. |
| `calibre libraries` | Number of declared libraries. |
| `calibre <name>` | Book count from each library's `metadata.db`. |
| `kavita database` | Integrity check result, user and library counts, folder count, and stored database version. |
| `kavita folders` | Whether every Kavita folder path maps back to an existing host directory. |
| `secrets` | Value-free SecretSpec check of the declared keys. |
| `container runtime` | Runtime version and Compose version. |
| `calibre image`, `kavita image` | Whether the expected image exists locally. |
| `calibre port`, `kavita port` | Whether the host port is free or serving the expected service. |
| `tailscale` | Backend state and node name. |
| `tailscale https` | Whether the node has a certificate domain for the hostname. |
| `tailscale serve` | Whether every configured upstream port appears in `tailscale serve status`. |
| `backups` | Age of the newest celine archive. |
| `disk space` | Free space on the media root and backup directory. Below 512 MiB is a failure, below 5 GiB is a warning. |
| `rendered compose` | Whether `build/compose.yaml` exists. |

Use `celine doctor --strict` as the gate before declaring a deployment healthy and after every update. The current host reports 25 ok, 0 warn, 0 fail.

## Tailscale Serve

```bash
celine tailnet apply    # install handlers for the configured endpoints
celine tailnet status   # backend, node, certificate domains, serve JSON, URLs
celine tailnet down     # remove handlers for the configured ports
```

`apply` runs one `tailscale serve --bg --yes --https=<port> http://127.0.0.1:<upstream>` call per endpoint. The operation is idempotent per port. `down` turns off only the configured ports and leaves other handlers on the node alone.

Current endpoints:

| Service | Tailnet URL | Upstream |
| --- | --- | --- |
| Kavita | https://massive-1.tailf8e61e.ts.net/ | `http://127.0.0.1:5000` |
| Calibre | https://massive-1.tailf8e61e.ts.net:8443/ | `http://127.0.0.1:18080` |

The hostname comes from `tailnet.hostname` when set, otherwise from the node's MagicDNS name. When `tailnet.enable` is false, `celine tailnet` prints that the feature is disabled and exits 0.

If you move a service to a different HTTPS port, apply the new configuration and turn off the old handler with `tailscale serve --https=<old port> off`. `celine tailnet down` reads the current configuration, so it cannot remove a port that is no longer declared.

## Backup

```bash
celine backup                # write an archive and prune old ones
celine backup --dry-run      # print the plan, write nothing
celine backup --retention 20 # override retention for this run
```

The backup:

1. Reads the Kavita inventory and runs a `PRAGMA integrity_check`.
2. Takes an online copy of `state/kavita/config/kavita.db` through the `sqlite3` `.backup` command.
3. Copies `appsettings.json`, the `bookmarks/` directory, and the Calibre server configuration in `state/calibre`. Calibre caches, caches under other names, logs, and `*.log` files are skipped.
4. Writes `manifest.json` with the format version, tool version, timestamp, `celine.toml` SHA-256, Kavita version, user and library counts, archived files, and exclusions.
5. Writes `backups/celine-<UTC timestamp>.tar.gz` with mode 0600, then prunes archives beyond the retention count, keeping the newest.

Kavita covers and caches, logs, and media are never archived. The backup directory must have room for the staged copy; a Kavita database of a few megabytes produces an archive of similar size.

The generated systemd timer and the NixOS module run `celine backup` on `backup.on_calendar` with `Persistent=true`, so a missed run starts after the next boot. `celine doctor` warns when the newest archive is older than 48 hours.

## Restore

Restore is destructive. Use `--dry-run` first.

```bash
celine restore backups/celine-<stamp>.tar.gz --dry-run
celine restore backups/celine-<stamp>.tar.gz --yes
celine restore backups/celine-<stamp>.tar.gz --yes --state-dir /tmp/celine-copy
```

Behavior:

1. Reads the archive and lists its entries. `--dry-run` prints the archive, entry count, and target state directory, then stops.
2. Requires `--yes`. Without it, the command fails.
3. When restoring the configured state directory, stops the services and takes a pre-restore backup when a Kavita database exists. A failure there is a warning. With `--state-dir`, the command targets that directory and leaves the live stack running.
4. Extracts into a staging directory and reads `manifest.json`. Archives without a manifest or with an unsupported format are rejected.
5. Copies `kavita/config/` and `calibre/` from the archive over the state directory. Files that exist in the state directory but not in the archive remain in place.
6. Prints the file count and archive creation time, then asks you to run `celine up`.

`--yes` is the destructive confirmation, and it permits overwriting the existing database. `--force` is accepted for compatibility and is only relevant when restoring into a directory chosen with `--state-dir`.

A restore into a separate `--state-dir` copy was run on 2026-11-07 and its database passed `PRAGMA integrity_check`. A restore over live state has not been run in this environment. The archive creation and extraction code is covered by unit tests, and `--dry-run` verifies an archive without writing.

## Update procedure

Take a backup before an update that changes data formats:

```bash
celine backup
```

### Update Calibre

1. Set `calibre.version` in `celine.toml` to the target release.
2. Run `celine up --build`.
3. Run `celine doctor --strict` and check the Calibre URL.

If the new release fails, restore the previous `calibre.version` and run `celine up --build` again.

### Update Kavita

1. Set `kavita.image` to the target tag or digest. Keep the `version-v` tag style used in `celine.toml`.
2. Run `celine up`. The pull happens when the image reference is not present locally.
3. Kavita upgrades its database on first start. Watch the logs during the upgrade.

   ```bash
   celine logs -f kavita
   ```

4. Run `celine doctor --strict`. The database check reports the stored version.

If the upgrade fails, recover with the latest backup or by re-importing the untouched legacy configuration. The recovery paths are in [migration.md](migration.md).

### Update the celine deployment

1. Pull or apply the repository changes.
2. Run `celine validate`.
3. Run `celine render --check`; when it fails, run `celine render`.
4. Run `celine up`.
5. Run `celine doctor --strict`.

When the systemd units change, re-render and reinstall them. See the next section.

## systemd units

`celine systemd render` writes three units to `build/systemd/` by default:

| Unit | Purpose |
| --- | --- |
| `celine.service` | System oneshot service with `User=<invoking user>` that runs `celine up --config <path>` at boot and `celine down --config <path>` on stop. It waits for the network and `tailscaled`, requires the media mount, and restarts on failure after 20 seconds. |
| `celine-backup.service` | Oneshot service that runs `celine backup --config <path>`. |
| `celine-backup.timer` | Timer with `OnCalendar` from `backup.on_calendar` and `Persistent=true`. |

Render the units with `DOCKER_HOST` set when the rootless Podman socket is not the default:

```bash
export DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
celine systemd render
```

Install and enable them with the boot script. Run it as the operator account that owns the Podman socket, not with `sudo`; it calls `sudo` for the system directories and `systemctl`. It refuses to run as root because the units must carry your uid and `DOCKER_HOST`.

```bash
scripts/install-boot.sh --dry-run   # render and print the actions
scripts/install-boot.sh             # install, enable, and start
scripts/install-boot.sh --uninstall # disable and remove
```

The script renders `build/systemd/` through `celine systemd render`, installs the three units into `/etc/systemd/system`, runs `systemctl daemon-reload`, and enables `celine.service` and `celine-backup.timer`. It accepts `--celine PATH` and `--config PATH`; without `--celine` it uses `build/celine`, then `result/bin/celine`, then `celine` from `PATH`, and builds `build/celine` with Go when no binary is found. If you use a Nix-built binary, run `nix build .#default` and pass `--celine result/bin/celine`.

System units do not inherit your shell `PATH`. The rendered units capture a compact `PATH` with the user's Nix profile, stable system directories, and the directories that currently provide `docker`, `tailscale`, `secretspec`, `sops`, and `sqlite3`. Rerun the script if a tool moves.

`celine systemd install` is the low-level alternative. It writes the files directly to `/etc/systemd/system` and fails without write access, so `scripts/install-boot.sh` is the supported path.

After changing paths, units, or `DOCKER_HOST`, rerun the script and restart the service:

```bash
scripts/install-boot.sh
sudo systemctl restart celine.service
```

The units render and pass `systemd-analyze verify`, and the script's `--dry-run` path runs in CI. The agent environment has no root and no user DBus, so unit installation was left to you.

On NixOS, do not use the script. The module defines `celine.target`, which wants `calibre-server.service`, `kavita.service`, `celine-secrets.service`, `celine-tailnet.service`, and `celine-backup.timer`. `nixos-rebuild switch` installs and starts the target; `systemctl status celine.target` and `systemctl status celine-backup.timer` show the boot state.

## Disk and mount

- The external drive is `/dev/sda1`, ext4, mounted at the repository path. `celine doctor` reports mount source and filesystem for the media root.
- The generated `celine.service` and `celine-backup.service` carry `RequiresMountsFor=<media_root>`. A boot without the drive fails the unit instead of starting containers against an empty directory.
- `celine doctor` fails below 512 MiB free on the media root or backup directory and warns below 5 GiB. Run `df -h` on the repository path when the check warns.
- Images live in the rootless Podman store under the user's home, not on the media drive. Media and archives grow on the drive; archives stay small relative to media.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `container runtime is not reachable` | `DOCKER_HOST` unset or the Podman socket stopped | Export `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`; `systemctl --user start podman.socket` |
| `compose file failed validation` | Rendered Compose rejected by the runtime | Run `celine render --check`; compare `build/compose.yaml` with the earlier version |
| `calibre did not become healthy` | Port conflict, image build failure | `celine logs calibre`; check `calibre.port` is free; rebuild with `celine up --build` |
| `kavita did not become healthy` | Database migration stalled or settings rejected | `celine logs kavita`; check `state/kavita/config/appsettings.json` and free space |
| `calibre port` is in use and does not answer | Another process owns the host port | Choose another `calibre.port`, then `celine up` |
| Tailscale Serve is not applied | Tailscale down, missing certificate domain, or ACL issue | `tailscale status`; `celine tailnet status`; enable HTTPS certificates; `celine tailnet apply` |
| Tailnet URL returns 502 | Containers are stopped; Serve is still installed | `celine up` |
| Secrets fail to resolve | Age identity unavailable or store undecryptable | `sops -d secrets/celine.enc.yaml`; set `SOPS_AGE_KEY_FILE`; run `secretspec --file secretspec.toml check` |
| Media root warning in doctor | External drive not mounted | Mount the drive; rerun `celine doctor` |
| Calibre library reports `metadata.db` failure | Library path wrong or incomplete copy | Fix `path` in `celine.toml`; restore the library |
| Kavita database failure | State not migrated | `celine migrate --yes` |
| Backup is stale | Timer not installed or the backup failed | Run `celine backup`; check `systemctl status celine-backup.timer`; read the journal |
| Restore does not proceed | Missing `--yes` | Rerun with `--yes`, or use `--dry-run` to verify the archive first |
| Kavita fails after an image upgrade | Database migration error | See [migration.md](migration.md); restore the latest archive or re-import `config/` |
| Calibre pages fail with `OSError` after a mount change | Media mounted read-only, which breaks the case-sensitivity probe | Restore the read-write mount from the rendered Compose |
| `celine.service` fails at boot | Media mount missing or `DOCKER_HOST` absent from the unit | Mount the drive; re-render the units with `DOCKER_HOST` exported; reinstall |
| Ports owned by another user | Rootless podman user or rootful daemon | Check `docker ps` against the running user; stop the conflicting service |

## Validation status

Verified live on this host on 2026-11-07: `celine up --build`, `celine status` and `celine status --json`, `celine doctor --strict`, `celine tailnet apply`, `celine backup`, `celine render --check`, and both tailnet URLs answering 200 with a Let's Encrypt certificate.

Not run in this environment: `celine restore` against live state, `celine systemd install`, a NixOS host deployment, and the rootful runtime path. The relevant core code and units are tested or verified as described above.
