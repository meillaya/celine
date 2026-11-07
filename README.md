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

This host is already deployed: `state/`, `secrets/celine.enc.yaml`, and `backups/` exist. Do not rerun `celine migrate` without `--force`. Run `celine up` to converge configuration changes.

## Daily commands

| Task | Command |
| --- | --- |
| Show health, URLs, and runtime | `celine status` |
| Print the same report as JSON | `celine status --json` |
| Run preflight and health checks | `celine doctor --strict` |
| Follow Kavita logs | `celine logs -f kavita` |
| Start or stop the containers | `celine up`, `celine down` |
| Apply or remove Tailscale Serve | `celine tailnet apply`, `celine tailnet down` |
| Archive state now | `celine backup` |
| Restore an archive | `celine restore backups/<archive> --yes` |
| Print the version | `celine version` |

`celine down` stops the containers and leaves Tailscale Serve handlers in place, so the URLs return 502 until the next `celine up`. Use `celine tailnet down` to remove the handlers.

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

The flake exposes `packages.default` and `nixosModules.default`. The module runs the upstream `services.calibre-server` and `services.kavita` options, binds media and state, materializes the Kavita token through SecretSpec into `services.kavita.credentials.token_key`, applies Tailscale Serve, and schedules backups.

```nix
services.celine = {
  enable = true;
  mediaRoot = "/run/media/mei/Kingston/celine/data";
  stateDir = "/var/lib/celine";
  secretspec.sopsAgeKeyFile = "/run/secrets/celine-age.key";
};
```

Keep the default `root`. It points at the Celine flake source, which Nix filters to the tracked files. Setting `root` to a local path literal such as `/run/media/mei/Kingston/celine` copies that directory into the Nix store, including `data/` and `state/` with tens of gigabytes of media. When the flake input path must be explicit, use `root = inputs.celine.outPath;`. `mediaRoot` must be the runtime path on the NixOS host. The age identity must be readable by the service; the module sets `SOPS_AGE_KEY_FILE` from `secretspec.sopsAgeKeyFile` and `SOPS_AGE_SSH_PRIVATE_KEY_FILE` from `secretspec.sopsAgeSshPrivateKeyFile`.

The module has been evaluated with `nix flake check`. A NixOS host has not been deployed from this repository. Use the Podman path and the NixOS module on separate hosts, because both own the same host ports and Tailscale HTTPS ports.

## Secrets

`secretspec.toml` declares `KAVITA_TOKEN_KEY` with base64 generation. The SOPS store at `secrets/celine.enc.yaml` is the only secret file tracked by Git; plaintext stores, decrypted files, and `.env` files are ignored. `celine migrate` imports the legacy Kavita signing key, so existing sessions survive the move. On a fresh store, SecretSpec generates a new key and existing sessions become invalid.

```bash
celine secrets materialize --name KAVITA_TOKEN_KEY --out /var/lib/celine/secrets/kavita-token
```

`celine secrets materialize` resolves one value into a file with mode 0600 for consumers outside the Compose path. The NixOS module uses this command. The rendered Compose file contains no secrets. `celine up` writes the resolved key and the Kavita settings to `state/kavita/config/appsettings.json` with mode 0600, and sets the `state/kavita/config` directory mode to 0700.

## Backups and restore

`celine backup` takes an online SQLite backup of the Kavita database, then archives the database, `appsettings.json`, bookmarks, and the Calibre server configuration into `backups/celine-<UTC timestamp>.tar.gz`. It skips Kavita covers and caches, logs, and all media. Retention defaults to 10 archives; `celine backup --retention N` overrides the configured count for one run.

`celine restore backups/<archive> --yes` verifies the archive, stops the services, takes a pre-restore backup, and writes the archived state back. `--yes` confirms the destructive action. `--force` is accepted for compatibility and is only relevant when restoring into a state directory chosen with `--state-dir`. Run `celine up` afterward. [docs/migration.md](docs/migration.md) covers restore and database-upgrade recovery; [docs/operations.md](docs/operations.md) covers routine backup and restore.

## Security model

- Both services bind host loopback. Tailscale Serve is the only route to them, and the handlers exist on the tailnet only.
- Calibre runs without authentication and the CLI does not pass `--enable-local-write`, so the HTTP surface is read-only. Access control is the tailnet ACL.
- Kavita enforces its own accounts.
- Calibre mounts media read-write because the server creates a temporary case-sensitivity probe file in each library root on first access; a read-only mount breaks OPDS with an `OSError`. The HTTP surface stays read-only.
- Secrets stay encrypted at rest. The `appsettings.json` file has mode 0600, the `state/kavita/config` directory has mode 0700, and rendered Compose contains no secret values.

## Troubleshooting

| Symptom | First check |
| --- | --- |
| `container runtime is not reachable` | Export `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`; check `systemctl --user status podman.socket` |
| Doctor reports a Kavita database failure | Run `celine migrate --yes`, or start Kavita once |
| Tailscale Serve is not applied | Run `celine tailnet apply`; check `tailscale status` and HTTPS certificates for the tailnet |
| Secrets do not resolve | Run `secretspec --file secretspec.toml check`; unlock the age identity |
| A service is not healthy | Run `celine logs <service>`; check the port in `celine.toml` |
| Media root is not a separate mount | Mount the external drive; the generated systemd unit uses `RequiresMountsFor` |

[docs/operations.md](docs/operations.md) has the full troubleshooting matrix and the update procedure.

## Migration and rollback

`celine migrate --yes` copies `config/` into `state/kavita/config` with an integrity check, writes a legacy archive under `backups/`, imports the signing key into the SOPS store, and records `state/.migration.json`. It never edits `config/`, so the legacy configuration remains available for rollback. [docs/migration.md](docs/migration.md) gives the exact steps, the signing-key behavior, and database-upgrade recovery.

## Documentation

- [docs/architecture.md](docs/architecture.md): components, data flow, paths, ports, secret flow, rootless Podman behavior.
- [docs/configuration.md](docs/configuration.md): every `celine.toml` key, `secretspec.toml`, `.sops.yaml`, rendering behavior, common changes.
- [docs/operations.md](docs/operations.md): start and stop, status, doctor, backups, restore, updates, systemd units, troubleshooting.
- [docs/migration.md](docs/migration.md): legacy layout, migration steps, signing-key import, rollback, database-upgrade recovery.
- [AGENTS.md](AGENTS.md): repository map and rules for agents changing this repository.
