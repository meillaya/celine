# Architecture

Celine deploys the Calibre content server and Kavita from one declaration. The local path uses rootless Podman and the `celine` CLI. The NixOS module provides a native path that consumes the same declaration. This document explains the components, the data flow, and the contracts between them. Key reference is in [configuration.md](configuration.md); task procedures are in [operations.md](operations.md).

## Components

| Component | Runs on | Responsibility |
| --- | --- | --- |
| `celine` CLI | user shell or systemd | Load and validate `celine.toml`, render deployment artifacts, drive Compose, Tailscale, `sqlite3`, and SOPS |
| [celine.toml](../celine.toml) | repository root | Single source of truth for paths, services, ports, libraries, tailnet endpoints, and backup policy |
| [secretspec.toml](../secretspec.toml) and the SOPS store | repository root plus an age identity on the host | Declare and decrypt `KAVITA_TOKEN_KEY` |
| Rootless Podman with the Compose provider | user session | Run the two containers, publish loopback ports, bind-mount media and state |
| `build/compose.yaml` | generated | Rendered Compose document consumed by `docker compose` through the Podman socket |
| `tailscaled` and Tailscale Serve | host service | Terminate HTTPS for the tailnet and proxy to host loopback ports |
| `celine.service`, `celine-backup.timer` | system systemd | Start the stack at boot and run scheduled backups |
| NixOS module ([nix/module.nix](../nix/module.nix)) | NixOS host | Native alternative that runs upstream `services.calibre-server` and `services.kavita` |
| External drive | `/dev/sda1` mounted at the repository path | Media, state, and backups |

The external drive is mounted at the repository root (`/run/media/mei/Kingston/celine`). The repository and its runtime data share that mount.

## Request path

```
tailnet client
      |
      | HTTPS, tailnet only
      v
tailscaled + Tailscale Serve  (massive-1.tailf8e61e.ts.net)
      |
      +-- :443  -> http://127.0.0.1:5000  -> celine-kavita-1  -> /config = state/kavita/config
      |                                                       -> /data   = data/Kavita
      |
      +-- :8443 -> http://127.0.0.1:18080 -> celine-calibre-1 -> /media  = data/
                                                              -> /config = state/calibre
```

Both containers publish on host loopback addresses. Tailscale Serve is the only listener on the tailnet, and it forwards each HTTPS port to one loopback port. A client on the tailnet reaches Kavita and Calibre; a client on the LAN or the internet has no route to either service.

`celine up` polls `http://127.0.0.1:<host port>/` for Calibre and `http://127.0.0.1:<host port>/api/health` for Kavita before it reports the stack healthy. The container healthchecks poll the internal ports instead (Calibre 8080, Kavita 5000).

## Control flow: `celine up`

1. Find `celine.toml` (walk up from the working directory, or use `--config` or `--root`) and load it. `--secretspec` overrides the manifest path. Unknown keys are rejected.
2. Validate the declaration and the media paths: every Calibre library has a readable `metadata.db`, the Kavita media root exists, and ports and tailnet targets obey the rules in [configuration.md](configuration.md).
3. Warn when `DOCKER_HOST` is unset. Rootless Podman needs `DOCKER_HOST=unix:///run/user/<uid>/podman/podman.sock`.
4. Create the state directories and set `state/kavita/config` to mode 0700.
5. Resolve `KAVITA_TOKEN_KEY`. An environment variable with that name short-circuits SecretSpec; otherwise the CLI runs `secretspec export` against the SOPS provider.
6. Render `state/kavita/config/appsettings.json` from the deployment settings and the resolved key, preserving keys it does not manage. Write the file atomically with mode 0600.
7. Render `build/compose.yaml` from `celine.toml` and the current uid/gid.
8. Verify the container runtime, build the Calibre image when it is missing or `--build` is set, pull the Kavita image when it is missing, then run `docker compose up -d --remove-orphans`.
9. Wait up to 180 seconds for both loopback health endpoints. Then apply Tailscale Serve unless `--no-tailnet` is set, and print the URLs. A Tailscale failure produces a warning and the local URLs; the containers stay up.

Other flows:

- `celine render` renders `build/compose.yaml` without the filesystem checks. `--check` compares the rendered bytes with the existing file and exits 1 on drift, which is the drift test used by `devenv` and agents.
- `celine status` loads and validates the declaration, probes both loopback ports, reads `tailscale status --json` and `tailscale serve status --json`, finds the newest backup, and asks the runtime for its version. `--json` prints the same report as machine-readable JSON.
- `celine backup` reads the Kavita inventory, runs an SQLite integrity check, takes an online `.backup` of the database, collects settings and bookmarks and the Calibre config, writes a manifest, and prunes old archives. See [operations.md](operations.md).
- `celine migrate` imports the legacy configuration and signing key. See [migration.md](migration.md).
- `celine doctor` runs the read-only checks documented in [operations.md](operations.md).

## Path contract

Paths in `[paths]` are relative to the directory that holds `celine.toml` unless they start with `/`. The loader resolves them once and every artifact uses the absolute values.

| Declared | Resolved | Container mount | Access |
| --- | --- | --- | --- |
| `paths.media_root = "data"` | `<root>/data` | Calibre `/media`; Kavita host side `data/Kavita` to `/data` | Read-write |
| `paths.state_dir = "state"` | `<root>/state` | Calibre `state/calibre` to `/config`; Kavita `state/kavita/config` to `/config` | Read-write |
| `paths.backup_dir = "backups"` | `<root>/backups` | Not mounted | Archives produced by the CLI and timer |

Derived locations:

| Path | Purpose |
| --- | --- |
| `state/calibre` | Calibre server settings (`global.py.json`, `gui.json`, plugins) |
| `state/kavita/config` | Kavita database, `appsettings.json`, bookmarks, caches, logs |
| `state/kavita/config.pre-migrate-<stamp>` | Previous state kept by `celine migrate --force` |
| `state/.migration.json` | Migration marker written by `celine migrate` when it imports the signing key |
| `secrets/celine.enc.yaml` | SOPS-encrypted secret store referenced by `secretspec.toml` |
| `build/compose.yaml` | Rendered Compose document, regenerated by `render` and `up` |

The Kavita database stores library folders as runtime paths under `kavita.library_mount` (`/data`). The Compose file mounts `data/Kavita` at `/data`, so the rows imported from the legacy deployment resolve without a database rewrite. `celine doctor` maps each stored folder back to the host path and checks that it is a directory. Changing `library_mount` therefore requires updating the stored paths as well as the mount.

`celine backup`, `restore`, and `migrate` accept `--state-dir` and `--backup-dir` overrides (restore accepts `--state-dir` only). The NixOS units use these to target `/var/lib/celine` while reading the same `celine.toml`.

## Port map

| Scope | Address | Port | Owner | Configurable |
| --- | --- | --- | --- | --- |
| Host loopback | `127.0.0.1` | 18080 | Published Calibre port | `calibre.port` |
| Container | Calibre | 8080 | `calibre-server --port=8080` | Fixed in [internal/render/compose.go](../internal/render/compose.go) |
| Host loopback | `127.0.0.1` | 5000 | Published Kavita port | `kavita.port` |
| Container | Kavita | 5000 | Kavita `Port` setting | Fixed for the container layout in [internal/render/appsettings.go](../internal/render/appsettings.go) |
| Tailnet | `massive-1.tailf8e61e.ts.net` | 443 | Kavita HTTPS | `tailnet.serve` |
| Tailnet | `massive-1.tailf8e61e.ts.net` | 8443 | Calibre HTTPS | `tailnet.serve` |
| Tailnet | `massive-1.tailf8e61e.ts.net` | 10000 | Unused | Valid Tailscale HTTPS port |

Validation requires host loopback ports to be 1024 or higher, so rootless Podman can bind them. Tailscale accepts HTTPS ports 443, 8443, and 10000, so `tailnet.serve` entries are restricted to those values. The container internal ports stay fixed so the rendered command and `appsettings.json` do not change when host ports move.

## Secret flow

1. [secretspec.toml](../secretspec.toml) declares `KAVITA_TOKEN_KEY` with `type = "base64"` and `generate = { bytes = 64 }`. It maps the project to the provider alias `celine_secrets = "sops://secrets/celine.enc.yaml"`.
2. [.sops.yaml](../.sops.yaml) lists the age recipients and the creation rule for `secrets/celine.enc.yaml`. SOPS decrypts with an age identity available on the host.
3. `celine up` resolves the value through `secretspec export --format json`. The resolver checks the environment first, which supports tests and CI; a production start should rely on the SOPS store.
4. The value and the Kavita settings are written to `state/kavita/config/appsettings.json` with mode 0600. `celine up` rewrites this file on every start. Keys that `celine` does not manage are preserved, including `OpenIdConnectSettings`.
5. `build/compose.yaml` never contains secret values. It carries the environment flags (`PUID`, `PGID`, `TZ`) and the bind mounts only.
6. The NixOS module runs `celine secrets materialize --name KAVITA_TOKEN_KEY --out <stateDir>/secrets/kavita-token` and wires the file into `services.kavita.credentials.token_key`.
7. Changing the key invalidates Kavita sessions. `celine migrate` imports the legacy key so sessions survive the move from the legacy deployment.

## Why Tailscale uses two HTTPS ports

Tailscale Serve associates one handler tree with one HTTPS port on one hostname. Serving both applications behind the same port requires path prefixes such as `/kavita` and `/calibre`. Tailscale Serve strips the mount point before proxying, so a request for `/calibre/...` reaches the backend as `/...`. Calibre's `--url-prefix` and Kavita's `BaseUrl` then no longer match the request, and absolute links generated by the applications point outside the mount. Celine gives each service its own HTTPS port instead: 443 for Kavita and 8443 for Calibre. Port 10000 remains available as the third valid Tailscale HTTPS port.

Each `tailnet.serve` entry becomes one handler:

```text
tailscale serve --bg --yes --https=443  http://127.0.0.1:5000
tailscale serve --bg --yes --https=8443 http://127.0.0.1:18080
```

`celine tailnet down` turns off only the configured ports (`tailscale serve --https=<port> off`), so unrelated handlers on the node stay installed. `tailscale serve reset` would remove every handler on the node; the CLI does not use it.

## Rootless Podman specifics

- `runtime.rootless = true` means containers keep their own root inside a user namespace. The Calibre service has no `user:` field, and Kavita runs with `PUID=0` and `PGID=0`. Container root maps to the calling host user, so files created in bind mounts stay owned by that user and no recursive chown is needed.
- Host ports are published on loopback only. Rootless Podman cannot bind ports below 1024, and validation rejects them.
- The Docker CLI and Compose provider talk to Podman through `DOCKER_HOST`. The compose project name is `celine`, the file is `build/compose.yaml`, and `up` passes `--remove-orphans`.
- Container healthchecks use the internal ports. `celine up` additionally probes the host loopback ports, so a published port or proxy problem fails the start.
- Container logs use the `json-file` driver with `max-size=10m` and `max-file=3`.
- `runtime.rootless = false` targets a rootful Docker daemon. It sets `user: <uid>:<gid>` on Calibre and passes the host uid/gid as `PUID`/`PGID` to Kavita. File ownership on the bind mounts then follows those ids.
- `systemctl --user status podman.socket` shows whether the socket is available. The rendered `celine.service` is installed as a system unit with `User=<caller>` so it starts at boot without a login session; it imports `DOCKER_HOST` from the render environment, falling back to the rootless Podman socket when the runtime is rootless.

## Reproducibility and state

Rendering is deterministic: the same `celine.toml`, uid, and gid produce the same Compose bytes, and `celine render --check` uses byte comparison. The Calibre image is rebuilt from [docker/calibre.Dockerfile](../docker/calibre.Dockerfile) with the pinned Calibre version. Kavita uses the pinned image tag in `celine.toml`.

State creates the parts that are not reproducible:

| Data | Reproducible | Backed up |
| --- | --- | --- |
| Rendered Compose and systemd units | Yes | No |
| Container images | Yes, subject to upstream availability | No |
| Kavita database (users, libraries, progress) | No | Yes, online SQLite backup |
| Kavita `appsettings.json` and `bookmarks/` | No | Yes |
| Kavita caches, covers, logs | Yes or cosmetic | No |
| Calibre server settings | Mostly | Yes, minus caches and logs |
| Media in `data/` | No | No, excluded by policy |

## Deployment paths

The local path runs on this host: `celine up` renders Compose, Podman runs the containers, and the generated system units start the stack at boot as the invoking user. The systemd path and the CLI share the same `celine.toml` and the same `DOCKER_HOST`.

The NixOS path uses the same declaration inside a NixOS configuration. The module enables the upstream `services.calibre-server` and `services.kavita`, binds `media_root/Kavita` at `kavita.library_mount` and `stateDir/kavita/config` at `/app/kavita/config`, materializes the token, applies Tailscale Serve from a oneshot unit, and schedules the backup timer. The flake exposes `packages.default`, `nixosModules.default`, and an evaluation-only `nixosConfigurations.celine-test` fixture. The two paths own the same ports and Tailscale HTTPS ports, so use them on separate hosts.

## Validation status

Verified live on this host on 2026-11-07: `celine migrate --yes`, `celine up --build`, `celine tailnet apply`, `celine status`, `celine status --json`, `celine doctor --strict` (25 ok, 0 warn, 0 fail), `celine backup`, `celine restore` into a separate `--state-dir` copy, and `celine render --check`. Both tailnet URLs answer 200 with a Let's Encrypt certificate.

Not exercised in this environment: `celine restore` against live state, `celine systemd install` (requires root; the units render and pass `systemd-analyze verify`), rootful container runtime behavior, and a NixOS host deployment (the module is evaluated by `nix flake check`).
