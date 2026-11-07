# AGENTS.md

Rules and orientation for agents changing the Celine repository. The user-facing documentation set starts at [README.md](README.md).

## Repository map

| Path | Contents |
| --- | --- |
| `cmd/celine/main.go` | Entry point. `main.version` is set by the Nix build. |
| `internal/cli/` | Command dispatch and flags. [cli.go](internal/cli/cli.go) holds common flags and dispatch; [commands.go](internal/cli/commands.go) holds validate, render, up, down, status, logs; [commands_ops.go](internal/cli/commands_ops.go) holds tailnet, doctor, backup, restore, migrate, secrets, systemd. |
| `internal/config/` | Loads and validates [celine.toml](celine.toml), resolves paths, rejects unknown keys. |
| `internal/render/` | Renders Compose, Kavita `appsettings.json`, systemd units, and tailnet endpoints. |
| `internal/compose/` | Docker Compose client driven through the Podman socket. |
| `internal/tailnet/` | Reads `tailscale status --json` and applies or clears Serve handlers. |
| `internal/secrets/` | SecretSpec resolver and `materialize`. |
| `internal/backup/` | Archive creation, manifest, retention, restore. |
| `internal/migrate/` | Legacy import and signing-key import. |
| `internal/doctor/` | Read-only checks. |
| `internal/kavita/` | SQLite inventory, integrity check, online backup, path mapping. |
| `internal/archiveutil/`, `internal/execx/` | Tar/copy helpers and the command runner abstraction used by tests. |
| `docker/calibre.Dockerfile`, `docker/calibre-entrypoint.sh` | Calibre image with a pinned release. |
| `nix/package.nix`, `nix/module.nix`, `flake.nix` | Package, NixOS module, flake outputs and evaluation fixture. |
| `devenv.nix`, `devenv.yaml`, `.envrc` | Development shell and scripts. |
| `tests/nixos-test.nix` | Evaluation-only NixOS fixture. |
| `.github/workflows/ci.yml` | CI: `go vet`, `go test`, `go build`. |
| `celine.toml`, `secretspec.toml`, `.sops.yaml` | Deployment declaration, secret declarations, SOPS recipients. |
| `docs/` | Architecture, configuration, operations, migration reference. |
| `data/`, `state/`, `backups/`, `config/`, `config.backup/`, `secrets/` | Runtime and legacy data. Ignored by Git. |

## Before changing code

Use the development shell for the toolchain: `devenv shell`. It provides Go, `sqlite3`, `sops`, `secretspec`, `tailscale`, `docker` with the Compose provider, `jq`, and the lint tools.

When package and build caches are read-only, point them into the repository:

```bash
export GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gopath/pkg/mod GOCACHE=$PWD/.cache/go-build
```

Run these checks before you hand off a change:

```bash
go vet ./...
go test ./...                 # backup tests use sqlite3; they skip when it is missing
go build ./cmd/celine
```

For render changes or `celine.toml` edits, run the drift check:

```bash
go run ./cmd/celine render --check
```

When the change intentionally alters `build/compose.yaml`, run `go run ./cmd/celine render` once so the check passes. Do not hand-edit generated files.

For Nix changes:

```bash
export XDG_CACHE_HOME=$PWD/.cache
nix flake check --no-build
nix build .#default
nix eval .#nixosConfigurations.celine-test.config.system.build.toplevel.drvPath
```

For systemd unit changes:

```bash
go run ./cmd/celine systemd render
systemd-analyze verify build/systemd/*.service build/systemd/*.timer
```

Installing units requires root and is the user's task. The agent environment has no root and no user DBus.

Keep the documentation set in step with code changes. A new command, flag, configuration key, or behavior change updates [README.md](README.md) plus the relevant file under `docs/`. CI runs the Go checks only; run the Nix checks locally when the flake, module, or package changes.

## Data directories

Never edit, move, or delete these; they hold user data and rollback sources:

| Path | Rule |
| --- | --- |
| `data/` | Media. Read for inspection only. Never write, move, or delete. |
| `state/` | Live Kavita database and settings, Calibre settings. Touched only by `celine` commands. |
| `backups/` | Archives, including the legacy config archive. Never delete. |
| `config/`, `config.backup/` | Legacy Kavita state kept for rollback. Read-only. |
| `secrets/` | SOPS store. Access only through `sops` or `secretspec`. |
| `build/` | Generated artifacts. Regenerate with `celine render` or `celine systemd render`; never hand-edit. |
| `.cache/`, `.gopath/`, `.devenv/` | Tool caches. Ignored by Git. |

## Secret rules

- Keep secrets out of [celine.toml](celine.toml), rendered Compose, documentation, tests, commit messages, logs, and terminal output.
- The only secret artifact tracked by Git is `secrets/celine.enc.yaml`, encrypted with SOPS. Plaintext `.env` files, `*.plain.*`, `*.dec.*`, and `*.tmp` files under `secrets/` are ignored; never add them.
- Resolve values through SecretSpec and SOPS. Use `celine secrets materialize --name <name> --out <path>` for consumers, and keep materialized files at mode 0600.
- Do not print `TokenKey` or `KAVITA_TOKEN_KEY`, do not `cat` decrypted store content, and do not echo the `KAVITA_TOKEN_KEY` environment override.
- Age private keys stay on hosts. Do not copy them into the repository, and do not add or remove recipients without the owner's approval.
- If a key leaks, rotate it. A new Kavita signing key invalidates sessions; SOPS recipients are updated with `sops updatekeys secrets/celine.enc.yaml`.

## Runtime safety

These commands change the live deployment or its data, so confirm with the Lead before running them:

```text
celine up [--build] [--no-tailnet]
celine down
celine tailnet apply
celine tailnet down
celine backup
celine restore <archive> --yes
celine migrate --yes
celine systemd install
celine secrets materialize
```

Read-only commands are safe for diagnosis: `celine validate`, `celine render --check`, `celine status`, `celine logs`, `celine doctor`, `celine tailnet status`, `celine backup --dry-run`, and `celine restore <archive> --dry-run`.

Run `celine up`, `celine down`, and tailnet commands only with `DOCKER_HOST` pointing at the rootless Podman socket. Do not change pinned image versions, host ports, or storage paths as part of unrelated work.
