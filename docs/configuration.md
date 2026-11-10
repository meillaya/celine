# Configuration

Celine reads three files: [celine.toml](../celine.toml) for the deployment, [secretspec.toml](../secretspec.toml) for secret declarations, and [.sops.yaml](../.sops.yaml) for the secret store recipients. This document states what each key does and how rendering turns them into deployment artifacts. Task procedures are in [operations.md](operations.md); the reason behind the path and port contract is in [architecture.md](architecture.md).

## Files and loading

- `celine.toml` is the single source of truth. The CLI finds it by walking up from the working directory, or uses the path given with `--config` or `--root`.
- `--secretspec PATH` overrides the `secretspec.toml` path. Without it, the CLI expects `secretspec.toml` next to `celine.toml`.
- The loader rejects unknown keys. A typo in a table or key name fails `celine validate` and every other command.
- `celine validate` checks the filesystem by default: the media root, every Calibre `metadata.db`, and the Kavita media root. `--no-filesystem` skips those checks.
- Paths in `[paths]` resolve relative to the directory holding `celine.toml`. Absolute paths pass through unchanged. `tailnet.hostname` and `kavita.library_mount` have their own rules.
- Command-line overrides do not rewrite the file. `celine backup --retention N` changes the retention for that run only.

## celine.toml reference

Defaults shown as "built-in" come from `Default()` in [internal/config/config.go](../internal/config/config.go). Values shown as "shipped" are the current contents of the repository's `celine.toml`. The shipped file is the effective configuration on this host.

### `[meta]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `schema` | integer | built-in `1` | Configuration schema. The loader accepts only `1`. |

### `[paths]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `media_root` | string | none; shipped `data` | Root of the Calibre libraries and the Kavita media directory. Mounted into Calibre at `/media`. Must be non-empty. |
| `state_dir` | string | none; shipped `state` | Parent of the container state directories. `calibre/` and `kavita/config/` are derived from it. Must be non-empty. |
| `backup_dir` | string | none; shipped `backups` | Directory for `celine-*.tar.gz` archives and legacy `legacy-config-*.tar.gz` archives. Required when `backup.enable` is true; the CLI validates it before a backup runs. |

### `[runtime]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `rootless` | boolean | built-in `true`; shipped `true` | Selects rootless Podman mappings. `true` leaves Calibre without a `user:` field and sets Kavita `PUID=0`/`PGID=0`. `false` sets host uid/gid for a rootful Docker daemon. |
| `timezone` | string | built-in empty; shipped `America/Toronto` | IANA timezone passed as `TZ` to both containers. Empty falls back to the `TZ` environment variable, the host `/etc/localtime` target, then `UTC`. |

### `[calibre]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enable` | boolean | built-in `true`; shipped `true` | Include the Calibre service. At least one of Calibre and Kavita must be enabled. |
| `version` | string | none; shipped `9.18.0` | Calibre release. Passed as the `CALIBRE_VERSION` build argument and used in the local image tag `celine/calibre:<version>`. Required when Calibre is enabled. |
| `image` | string | built-in empty; shipped absent | Overrides the image tag applied to the locally built Calibre image. The Dockerfile is still built; this changes the tag that receives it. |
| `port` | integer | built-in `8080`; shipped `18080` | Host loopback port. The container listens on the fixed internal port 8080 and Compose publishes `127.0.0.1:<port>:8080`. Valid range is 1024 to 65535 and it must differ from `kavita.port`. |
| `libraries` | array of tables | none | Calibre libraries. Required and non-empty when Calibre is enabled. |

### `[[calibre.libraries]]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | none | Unique display name. Used in `validate` and `doctor` output. |
| `path` | string | none | Library directory relative to `paths.media_root`. Must be relative and stay inside the media root. Must contain `metadata.db`. |

The rendered Calibre command passes each library as `/media/<path>` after `--`. The server serves one library per argument.

### `[kavita]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enable` | boolean | built-in `true`; shipped `true` | Include the Kavita service. |
| `image` | string | none; shipped `lscr.io/linuxserver/kavita:version-v0.9.1.4` | Full image reference. Pin a tag or digest. Required when Kavita is enabled. |
| `port` | integer | built-in `5000`; shipped `5000` | Host loopback port. The container listens on the fixed internal port 5000 and Compose publishes `127.0.0.1:<port>:5000`. Valid range is 1024 to 65535 and it must differ from `calibre.port`. |
| `library_subdir` | string | built-in `Kavita`; shipped `Kavita` | Kavita media root relative to `paths.media_root`. Mounted at `library_mount`. |
| `library_mount` | string | built-in `/data`; shipped `/data` | Clean absolute path Kavita sees for `library_subdir`. Kavita stores library rows under this path. `/` is rejected. |
| `cache` | integer | built-in `75`; shipped `75` | Kavita `Cache` setting in `appsettings.json`. Must be zero or greater. |
| `base_url` | string | built-in `/`; shipped `/` | Kavita `BaseUrl` setting. Must start with `/`. |
| `allow_iframing` | boolean | built-in `false`; shipped `false` | Kavita `AllowIFraming` setting. |

### `[tailnet]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enable` | boolean | built-in `true`; shipped `true` | Apply Tailscale Serve endpoints from `celine up` and allow `celine tailnet` commands. When `false`, the CLI prints local URLs and `doctor` skips tailnet checks. |
| `hostname` | string | built-in empty; shipped empty | Public hostname used to print URLs. Empty reads the MagicDNS name from `tailscale status --json` (`Self.DNSName`). A set value must be a bare DNS name with no `/` or `:`. |
| `serve` | array of tables | none | Tailscale Serve targets. Required and non-empty when the tailnet is enabled. |

### `[[tailnet.serve]]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `service` | string | none | `calibre` or `kavita`. The service must be enabled, and each service can appear once. |
| `https_port` | integer | none | Tailscale HTTPS port for the service. Allowed values are 443, 8443, and 10000, and each port can appear once. |

Each entry becomes `tailscale serve --bg --yes --https=<port> http://127.0.0.1:<upstream host port>`, where the upstream port is the service's `calibre.port` or `kavita.port`. Port 443 produces a URL without a port (`https://hostname`); other ports append the port (`https://hostname:8443`).

### `[backup]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enable` | boolean | built-in `true`; shipped `true` | Gates the scheduled backup timer in both the local systemd units and the NixOS module (the NixOS option defaults to this value). Manual `celine backup` still runs when it is false. |
| `retention` | integer | built-in `10`; shipped `10` | Number of newest archives kept after each backup. Must be at least 1, including when the timer is disabled, because manual backups use it. `celine backup --retention N` overrides it for one run. |
| `on_calendar` | string | built-in `daily`; shipped `daily` | systemd `OnCalendar` expression for the generated timer and the NixOS module timer. Required and non-empty. |

## Validation rules

`celine validate` and the load step used by every command enforce these rules:

- `meta.schema` equals 1.
- At least one of `calibre.enable` and `kavita.enable` is true.
- `paths.media_root` and `paths.state_dir` are non-empty.
- `paths.backup_dir` is non-empty when `backup.enable` is true.
- Calibre, when enabled: `version` non-empty, `port` in 1024 to 65535, at least one library, unique library names, unique library paths, relative paths that stay inside the media root.
- Kavita, when enabled: `image` non-empty, `port` in 1024 to 65535, `library_subdir` relative and inside the media root, `library_mount` a clean absolute path other than `/`, `cache` zero or greater, `base_url` starting with `/`.
- The Calibre and Kavita host ports must differ when both services are enabled.
- Tailnet, when enabled: at least one serve target, known service names, enabled services only, one entry per service, HTTPS ports restricted to 443, 8443, and 10000, unique ports, bare hostname.
- Backup: `retention` at least 1; when enabled, `on_calendar` non-empty.
- `secretspec.toml` exists at the resolved path when Kavita is enabled. A Calibre-only deployment does not need a secret manifest.
- With filesystem checks on: the media root is a readable directory, every declared Calibre library has a readable `metadata.db`, and the Kavita media root is a directory.

## secretspec.toml

| Element | Value | Meaning |
| --- | --- | --- |
| `project.name` | `celine` | Project identity recorded by SecretSpec. |
| `project.revision` | `1.0` | Manifest revision. |
| `providers.celine_secrets` | `sops://secrets/celine.enc.yaml` | Provider alias for the SOPS store, relative to the repository root. |
| `profiles.default.KAVITA_TOKEN_KEY` | `type = "base64"`, `generate = { bytes = 64 }` | The Kavita JWT signing key. SecretSpec can generate a value when the provider has none. |
| `profiles.default.defaults.providers` | `["celine_secrets"]` | Provider used when no override is given. |

The CLI resolves values with `secretspec --file <manifest> export --format json --profile <profile> --provider <provider> --reason <reason> --caller celine --caller-version <version>`. `celine doctor` uses the value-free `secretspec check --no-prompt --json`; the generated units run `celine up` and `celine backup`, which resolve values through the same exporter. They do not call `secretspec check` directly.

Resolution order for `KAVITA_TOKEN_KEY`:

1. The environment variable `KAVITA_TOKEN_KEY`, when set and non-empty. This path exists for tests and CI; use the SOPS store for normal operation. `celine up` and `celine secrets materialize` warn on stderr when it is used.
2. The SecretSpec provider.

`SECRETSPEC_PROFILE` (default `default`) and `SECRETSPEC_PROVIDER` (default empty) select the profile and provider. [celine.toml](../celine.toml) keeps the default profile.

## .sops.yaml

The file defines four age recipients and one creation rule:

| Recipient | Purpose |
| --- | --- |
| `admin` | General administration identity. |
| `recovery` | Recovery identity kept for the secret store. |
| `workstation` | Decrypts the store on this machine. |
| `github` | Derived identity used by NixOS hosts that install the fleet SSH key. |

The creation rule matches `^secrets/celine\.enc\.yaml$` and encrypts the file data key to all four recipients. SOPS selects the rule from the file path, so `celine migrate` passes `--filename-override secrets/celine.enc.yaml` when it creates the store.

Add a recipient by adding its age public key to the recipient list and running `sops updatekeys secrets/celine.enc.yaml`. `updatekeys` re-wraps the existing data key; removing an old identity from the file requires a data-key rotation (`sops rotate -i secrets/celine.enc.yaml`). Private age keys stay on hosts and never enter the repository. The identity file must be mode 0600; `celine doctor` fails when the default or configured identity is group/world readable. SOPS reads an identity from its default location, `SOPS_AGE_KEY_FILE`, or `SOPS_AGE_SSH_PRIVATE_KEY_FILE`.

## Rendering behavior

The CLI generates three kinds of artifacts. None contains secret values.

| Artifact | Produced by | Location | Notes |
| --- | --- | --- | --- |
| Compose document | `celine render`, `celine up` | `build/compose.yaml` | Deterministic bytes for a given config, uid, and gid. Mode 0644. |
| Kavita settings | `celine up` | `state/kavita/config/appsettings.json` | Mode 0600. Managed keys: `TokenKey`, `Port`, `IpAddresses`, `BaseUrl`, `Cache`, `AllowIFraming`. Other keys are preserved. |
| systemd units | `celine systemd render` | `build/systemd/` by default | `celine.service`; plus `celine-backup.service` and `celine-backup.timer` when `backup.enable` is true. Written with mode 0644. |

The Compose document contains:

- Project name `celine` and a service for each enabled app.
- Calibre: image tag, local `build` context `docker/`, command `--port=8080 --listen-on=0.0.0.0 --disable-use-bonjour -- <libraries>`, `CALIBRE_CONFIG_DIRECTORY=/config`, `HOME=/config`, `TZ`, the media mount, the state mount, the loopback port mapping, a healthcheck, and log rotation. The media mount is read-write because Calibre writes a temporary case-sensitivity probe file in each library root; the HTTP surface stays read-only.
- Kavita: pinned image, `PUID`, `PGID`, `TZ`, the state mount at `/config`, the media mount at `kavita.library_mount`, the loopback port mapping, a healthcheck, and log rotation.
- `restart: unless-stopped`, a 15-second Calibre stop grace period, a 30-second Kavita stop grace period, and `json-file` logging with `max-size=10m` and `max-file=3` on both services.

`celine render --check` compares the rendered bytes against `build/compose.yaml` and exits 1 when the file is missing or differs. `celine up` always re-renders the Compose document and re-renders `appsettings.json`, writing it when the bytes changed, so a separate `render` step is not required before starting. If `appsettings.json` is newer than the running Kavita container, `celine up` restarts Kavita before waiting for health, so token rotations and other settings changes actually apply. The systemd units are never rendered implicitly; run `celine systemd render` after changing paths, units-related settings, or `DOCKER_HOST`. When `backup.enable` is false, `celine systemd render` emits only `celine.service`.

The generated units include `RequiresMountsFor` for the media, state, and backup directories, pass through `SOPS_AGE_KEY_FILE`, `SOPS_AGE_SSH_PRIVATE_KEY_FILE`, `SECRETSPEC_PROFILE`, and `SECRETSPEC_PROVIDER`, and (for rootless Podman) wait for `user@<uid>.service`, set `XDG_RUNTIME_DIR`, wait briefly for the Podman socket, and wait up to 20 minutes for the binary and media root to appear. The path wait is for the external drive, which may be auto-mounted after login rather than at boot.

Kavita settings are rendered for the container layout: `Port` is always 5000 and `IpAddresses` is `0.0.0.0,::`. The native target used by the NixOS path binds `127.0.0.1` and uses the configured port instead.

## Common changes

### Add a Calibre library

1. Create the library directory under `data/Calibre/` with a `metadata.db`. Copying an existing Calibre library directory preserves `metadata.db`.
2. Add a block to `celine.toml`:

   ```toml
   [[calibre.libraries]]
   name = "History"
   path = "Calibre/History"
   ```

3. Run `celine validate`. It checks that `metadata.db` exists and that the name and path are unique.
4. Run `celine up`. The Calibre container restarts with the new library argument. The image does not need a rebuild.

### Remove a Calibre library

1. Delete its `[[calibre.libraries]]` block.
2. Run `celine validate` and `celine up`.
3. Keep the files under `data/`; removing a declaration does not delete media.

### Change the Calibre version

1. Set `calibre.version` to the target release. The version is passed to the upstream Calibre installer inside the image build.
2. Run `celine up --build`. `up` rebuilds the image and recreates the container.
3. Roll back by restoring the previous version and running `celine up --build` again.

### Change the Kavita image

1. Set `kavita.image` to the target tag or digest.
2. Run `celine up`. When the image reference is not present locally, `up` pulls it before starting the container.
3. To refresh the same tag after an upstream re-push, run `celine up --pull`; the flag pulls even when the image already exists locally.

### Change host ports

1. Set `calibre.port` or `kavita.port` to a free loopback port. The container internal ports stay fixed.
2. Run `celine up`. Compose publishes the new host ports and recreates the containers.
3. Tailscale Serve follows automatically because each endpoint proxies to the service's host port. Run `celine tailnet apply` after changing the direction or state of the tailnet.

### Change Tailscale HTTPS ports

1. Edit the `https_port` values in `[[tailnet.serve]]`. Allowed values are 443, 8443, and 10000.
2. Run `celine tailnet apply` to install the handlers for the new ports.
3. Remove a handler on a port you no longer configure with `tailscale serve --https=<port> off`. `celine tailnet down` clears only the ports present in the current configuration.

### Change paths, timezone, or retention

- `paths` and `library_mount` changes affect bind mounts and the Kavita database path contract. Read the path contract in [architecture.md](architecture.md) before moving state. Run `celine migrate` again only for a deliberate state relocation.
- `runtime.timezone` takes effect on the next `celine up`.
- `backup.retention` takes effect on the next backup. `celine backup --retention N` overrides it once; the systemd timer and the NixOS module use the configured value.

## Command-line overrides

| Flag | Commands | Effect |
| --- | --- | --- |
| `--config PATH` | all | Use this `celine.toml`. |
| `--root PATH` | all | Treat `PATH` as the repository root and read `PATH/celine.toml`. |
| `--secretspec PATH` | all | Use this SecretSpec manifest instead of the one next to `celine.toml`. |
| `--state-dir PATH` | `backup`, `restore`, `migrate` | Override `paths.state_dir` for the command. |
| `--backup-dir PATH` | `backup`, `migrate` | Override `paths.backup_dir` for the command. |
| `--retention N` | `backup` | Override `backup.retention` for this run when `N` is greater than zero. |
| `--dry-run` | `backup`, `restore` | Print the plan; for `restore`, fully verify the archive, hashes, database, and inventory without writing. |
| `--yes` | `restore`, `migrate` | Confirm the destructive action. For `restore`, `--yes` also permits overwriting the existing database. |
| `--strict` | `status`, `doctor` | Treat a degraded service (status) or warnings (doctor) as a failure. |
| `--expect-running` | `doctor` | Treat free service ports as failures; use this when the stack is supposed to be up. |
| `--pull` | `up` | Pull the Kavita image even when it is present locally. |
| `--stop-services` | `migrate` | Stop the running Celine stack before replacing live state. |

## Environment variables

| Variable | Read by | Meaning |
| --- | --- | --- |
| `DOCKER_HOST` | CLI, generated units | Address of the container runtime socket. Rootless Podman uses `unix:///run/user/<uid>/podman/podman.sock`. The CLI warns when it is unset; `celine up` fails when the default socket is unreachable. |
| `SECRETSPEC_PROFILE` | CLI | SecretSpec profile. Default `default`. |
| `SECRETSPEC_PROVIDER` | CLI | SecretSpec provider override. Empty uses the manifest default. |
| `KAVITA_TOKEN_KEY` | CLI resolver | Short-circuits SecretSpec resolution and overrides the SOPS store. Intended for tests and CI; `celine up` and `celine secrets materialize` warn when it is set. |
| `TZ` | CLI, containers | Fallback timezone when `runtime.timezone` is empty. Also part of the container environment defaults. |
| `SOPS_AGE_KEY_FILE`, `SOPS_AGE_SSH_PRIVATE_KEY_FILE` | SOPS and SecretSpec provider | Age identity used to decrypt `secrets/celine.enc.yaml`. The NixOS module exposes the same values as options. |
