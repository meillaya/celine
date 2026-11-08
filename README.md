# Celine

Celine runs the Calibre content server and Kavita for this host from one declaration, [celine.toml](celine.toml). The `celine` CLI renders that file into Docker Compose for rootless Podman, writes Kavita settings, publishes Tailscale Serve HTTPS endpoints, archives state, and checks health. The NixOS module in [nix/module.nix](nix/module.nix) provides the native deployment path.

The repository, its media, and its state live on the external drive mounted at `/run/media/mei/Kingston/celine`.

## What runs

| Service | Image | Host listener | Tailnet URL |
| --- | --- | --- | --- |
| Calibre content server | built from [docker/calibre.Dockerfile](docker/calibre.Dockerfile), Calibre 9.18.0 | `127.0.0.1:18080` | https://massive-1.tailf8e61e.ts.net:8443/ |
| Kavita | `lscr.io/linuxserver/kavita:version-v0.9.1.4` | `127.0.0.1:5000` | https://massive-1.tailf8e61e.ts.net/ |

Calibre serves the nine libraries declared in `celine.toml`, with media from `data/` mounted at `/media` and its own settings in `state/calibre`. Kavita serves `data/Kavita` at `/data` and keeps its database and settings in `state/kavita/config`. Tailscale Serve publishes both services on the tailnet. The host does not publish LAN or internet ports.

## Architecture

```text
tailnet client
      |  HTTPS, tailnet only
      v
tailscale serve on massive-1.tailf8e61e.ts.net
      |
      +-- :443  -> http://127.0.0.1:5000  -> kavita container  -> state/kavita/config
      +-- :8443 -> http://127.0.0.1:18080 -> calibre container -> data/ (read-write)
                                                              -> state/calibre
media and state: data/, state/, backups/ on the external drive
control path:   celine CLI -> celine.toml -> build/compose.yaml -> podman compose
secrets:        secretspec + SOPS -> state/kavita/config/appsettings.json
```

[docs/architecture.md](docs/architecture.md) explains the components, the path contract, and the port map.

## Quickstart (this host)

Run these steps from the repository root with the media drive mounted and `tailscaled` running with HTTPS certificates enabled for the tailnet. The SOPS age identity must be available to `sops` and `secretspec`.

1. Enter the development shell and point the Docker client at the rootless Podman socket.

   ```bash
   cd /run/media/mei/Kingston/celine
   devenv shell                 # or: direnv allow
   export DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
   ```

2. Validate the declaration and inspect the host.

   ```bash
   celine validate
   celine doctor
   ```

   Before the first migration, `doctor` reports the missing Kavita database. That failure is expected.

3. Import the legacy Kavita state. Run this once. The import creates `state/kavita/config`, archives `config/` to `backups/legacy-config-<stamp>.tar.gz`, and imports the legacy signing key into the SOPS store.

   ```bash
   celine migrate --yes
   ```

4. Build the Calibre image and start the stack. `up` renders Compose, writes `appsettings.json`, starts the containers, waits for both health endpoints, and applies Tailscale Serve.

   ```bash
   celine up --build
   ```

5. Reapply Tailscale Serve after tailnet changes, or after a start with `--no-tailnet`.

   ```bash
   celine tailnet apply
   ```

6. Open the services:
   - Kavita: https://massive-1.tailf8e61e.ts.net/
   - Calibre: https://massive-1.tailf8e61e.ts.net:8443/

7. Install the boot units once. The script renders the units with your uid, `DOCKER_HOST`, SOPS/SecretSpec environment, and a stable tool `PATH`, then uses `sudo` for `/etc/systemd/system` and `systemctl`. It refuses to run as root and builds `build/celine` with Go if no binary is available. Rootless Podman also needs user lingering and the user `podman.socket`; `--enable-linger` and `--enable-podman` do both.

   ```bash
   scripts/install-boot.sh --dry-run
   scripts/install-boot.sh --enable-linger --enable-podman
   ```

This host is already deployed: `state/`, `secrets/celine.enc.yaml`, and `backups/` exist. Do not rerun `celine migrate` without `--force`, and run `celine doctor --strict --expect-running` after boot changes to see whether units, lingering, secret modes, and the backup timer are actually healthy. Run `celine up` to converge configuration changes.

## Daily commands

| Task | Command |
| --- | --- |
| Show a machine-readable health report | `celine status --json` |
| Fail automation when a service is down | `celine status --strict` |
| Run preflight and health checks | `celine doctor --strict --expect-running` |
| Follow Kavita logs | `celine logs -f kavita` |
| Start or stop the containers | `celine up`, `celine down` |
| Refresh a re-pushed Kavita tag | `celine up --pull` |
| Install or remove boot units | `scripts/install-boot.sh`, `scripts/install-boot.sh --uninstall` |
| Apply or remove Tailscale Serve | `celine tailnet apply`, `celine tailnet down` |
| Archive state now | `celine backup` |
| Restore an archive | `celine restore backups/<archive> --dry-run`, then `--yes` |
| Print the version | `celine version` |

`celine doctor` without `--expect-running` is a preflight check: a free port is reported as a service that has not started yet. Use `--expect-running` (or `celine status --strict`) when the stack is supposed to be up. `celine down` stops the containers and leaves Tailscale Serve handlers in place, so the URLs return 502 until the next `celine up`. Use `celine tailnet down` to remove the handlers.

## Configuration

[celine.toml](celine.toml) is the single source of truth: paths, container images and ports, Calibre libraries, tailnet endpoints, and backup policy. [secretspec.toml](secretspec.toml) declares secret names. [.sops.yaml](.sops.yaml) holds the encryption recipients. [docs/configuration.md](docs/configuration.md) documents every key, default, and validation rule.

Paths resolve relative to the directory that holds `celine.toml`:

| Path | Contents |
| --- | --- |
| `data/` | Calibre libraries and `Kavita/` media |
| `state/` | `calibre/` settings and `kavita/config/` database and settings |
| `backups/` | celine archives and legacy config archives |
| `build/` | rendered `compose.yaml` and systemd units (generated, ignored by Git) |

The host ports in `celine.toml` are loopback ports. The containers always listen on their internal ports: Calibre on 8080 and Kavita on 5000.

## NixOS deployment

The flake exposes `packages.default` and `nixosModules.default`. The module runs the upstream `services.calibre-server` and `services.kavita` options, binds media and state, materializes the Kavita token through SecretSpec into `services.kavita.credentials.token_key`, applies Tailscale Serve, and schedules backups. A `celine.target` unit groups the two services, the SecretSpec materialization, the Tailscale Serve unit, and the backup timer, so `nixos-rebuild switch` installs boot persistence and `systemctl status celine.target` shows the boot set. The install script is for Podman hosts; NixOS does not use it.

```nix
services.celine = {
  enable = true;
  mediaRoot = "/run/media/mei/Kingston/celine/data";
  stateDir = "/var/lib/celine";
  secretspec.sopsAgeKeyFile = "/run/secrets/celine-age.key";
};
```

Keep the default `root`. It points at the Celine flake source, which Nix filters to the tracked files. Setting `root` to a local path literal such as `/run/media/mei/Kingston/celine` copies that directory into the Nix store, including `data/` and `state/` with tens of gigabytes of media. When the flake input path must be explicit, use `root = inputs.celine.outPath;`. `mediaRoot` must be the runtime path on the NixOS host. The age identity must be readable by the service; the module sets `SOPS_AGE_KEY_FILE` from `secretspec.sopsAgeKeyFile` and `SOPS_AGE_SSH_PRIVATE_KEY_FILE` from `secretspec.sopsAgeSshPrivateKeyFile`. When SecretSpec materializes the token, one of those two options is required; a unit that runs outside your login session cannot fall back to an interactive default identity. `services.celine.backup.enable` defaults to `[backup].enable` in `celine.toml`, and the secret/backup units require the state and backup mounts before they run.

The module has been evaluated with `nix flake check`. A NixOS host has not been deployed from this repository. Use the Podman path and the NixOS module on separate hosts, because both own the same host ports and Tailscale HTTPS ports.

## Secrets

`secretspec.toml` declares `KAVITA_TOKEN_KEY` with base64 generation. The SOPS store at `secrets/celine.enc.yaml` is the only secret file tracked by Git; plaintext stores, decrypted files, and `.env` files are ignored. `celine migrate` imports the legacy Kavita signing key, so existing sessions survive the move. On a fresh store, SecretSpec generates a new key and existing sessions become invalid.

```bash
celine secrets materialize --name KAVITA_TOKEN_KEY --out /var/lib/celine/secrets/kavita-token
```

`celine secrets materialize` resolves one value into a file with mode 0600 for consumers outside the Compose path. The NixOS module uses this command. The rendered Compose file contains no secrets. `celine up` writes the resolved key and the Kavita settings to `state/kavita/config/appsettings.json` with mode 0600, and sets the `state/kavita/config` directory mode to 0700.

## Backups and restore

`celine backup` takes an online SQLite backup of the Kavita database, then archives the database, a redacted `appsettings.json`, bookmarks, and the Calibre server configuration into `backups/celine-<UTC timestamp>.tar.gz`. It skips Kavita covers, caches, themes, fonts, templates, favicons, images, logs, internal backups, and all media. The archive contains a manifest with the tool version, timestamp, configuration SHA-256, Kavita inventory, every archived file's SHA-256, and the exclusion list. `celine backup` reads the archive back and verifies the manifest, hashes, database integrity, and inventory before reporting success. Retention defaults to 10 archives; `celine backup --retention N` overrides the configured count for one run.

The Kavita signing key (`TokenKey`) and known nested secret keys (for example `OpenIdConnectSettings.Secret`) are removed before archiving. `celine up` re-renders the signing key from SecretSpec after a restore; OIDC client secrets must be re-entered. Archives are mode 0600 but are **not encrypted**: they contain user data and Calibre account state. Store them on trusted media and copy them offsite separately.

`celine restore backups/<archive> --dry-run` verifies the archive, hashes, database integrity, and inventory without writing. `celine restore backups/<archive> --yes` verifies again, stops the configured services, takes a pre-restore backup, removes stale SQLite `-wal`/`-shm` sidecars, installs the database through a temporary file, and checks the restored database before telling you to run `celine up`. It refuses to write over live state if the services cannot be stopped or if the pre-restore backup fails. The `--force` flag is accepted for compatibility, but `--yes` is the destructive confirmation. See [docs/migration.md](docs/migration.md) for recovery paths and [docs/operations.md](docs/operations.md) for routine operations.

## Security model

- Both services bind host loopback. Tailscale Serve is the only route to them, and the handlers exist on the tailnet only.
- Calibre runs without authentication and the CLI does not pass `--enable-local-write`, so the HTTP surface is read-only. Access control is the tailnet ACL.
- Kavita enforces its own accounts.
- Calibre mounts media read-write because the server creates a temporary case-sensitivity probe file in each library root on first access; a read-only mount breaks OPDS with an `OSError`. The HTTP surface stays read-only.
- The SOPS store is encrypted at rest and `secrets/` is mode 0700. The age private identity must be mode 0600; `celine doctor` fails when it is group/world readable.
- `celine up` writes `state/kavita/config/appsettings.json` with mode 0600 and keeps the config directory at 0700. It also sets the Calibre state directory to 0700. `celine doctor` reports managed state that is group/world readable.
- New backup archives are mode 0600 and redact the signing key and known nested secret keys, but they are not encrypted. Treat `backups/` as sensitive state.
- Legacy rollback copies are plaintext: `config/`, `config.backup/`, the old `kavita_config_*.tar.gz` archives, and any Celine archive created before this redaction change contain the legacy signing key. Run `chmod -R go-rwx config config.backup` and `chmod 600 backups/*.tar.gz` after you no longer need the rollback path. `celine doctor` warns about group/world-readable legacy copies, and `celine restore --dry-run` warns when an archive still contains plaintext secret values; delete pre-redaction archives after a new verified backup.
- `celine up` warns on stderr when `KAVITA_TOKEN_KEY` is supplied from the environment, because that bypasses the SOPS store. Environment overrides exist for tests and CI; normal operation should use the store.

## Troubleshooting

| Symptom | First check |
| --- | --- |
| `container runtime is not reachable` | Export `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`; check `systemctl --user status podman.socket` |
| Doctor reports a Kavita database failure | Run `celine migrate --yes`, or restore the newest archive |
| Doctor reports `boot units` or `boot linger` | Run `scripts/install-boot.sh --enable-linger --enable-podman` |
| Doctor reports `age identity` mode | `chmod 600 ~/.config/sops/age/keys.txt` (or the path in `SOPS_AGE_KEY_FILE`) |
| Doctor reports legacy permissions | `chmod -R go-rwx config config.backup; chmod 600 backups/*.tar.gz` after the rollback window |
| Doctor reports `backup timer` | Run `scripts/install-boot.sh`, then `systemctl list-timers celine-backup.timer` |
| Doctor reports `backup integrity` | Keep the old archive, run `celine backup`, and verify the new one with `celine restore --dry-run` |
| Tailscale Serve is not applied | Run `celine tailnet apply`; check `tailscale status` and HTTPS certificates for the tailnet |
| Secrets do not resolve | Run `secretspec --file secretspec.toml check`; unlock the age identity |
| A service is not healthy | Run `celine logs <service>`; check the port in `celine.toml` |
| Media root is not a separate mount | Mount the external drive; the generated systemd unit uses `RequiresMountsFor` |

[docs/operations.md](docs/operations.md) has the full troubleshooting matrix and the update procedure.

## Migration and rollback

`celine migrate --yes` copies `config/` into `state/kavita/config` with an integrity check, writes a legacy archive under `backups/`, imports the signing key into the SOPS store, and records `state/.migration.json`. It never edits `config/`, so the legacy configuration remains available for rollback. The migration preflights the target before writing, stages and verifies the database, imports the signing key before replacing live state, compares an existing SOPS store instead of claiming an import it did not perform, and refuses to replace state under a running stack unless you pass `--stop-services`. [docs/migration.md](docs/migration.md) gives the exact steps, the signing-key behavior, and database-upgrade recovery.

## Documentation

- [docs/architecture.md](docs/architecture.md): components, data flow, paths, ports, secret flow, rootless Podman behavior.
- [docs/configuration.md](docs/configuration.md): every `celine.toml` key, `secretspec.toml`, `.sops.yaml`, rendering behavior, common changes.
- [docs/operations.md](docs/operations.md): start and stop, status, doctor, backups, restore, updates, systemd units, troubleshooting.
- [docs/migration.md](docs/migration.md): legacy layout, migration steps, signing-key import, rollback, database-upgrade recovery.
- [AGENTS.md](AGENTS.md): repository map and rules for agents changing this repository.
