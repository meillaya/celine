# Migration and rollback

Celine imports the legacy Kavita deployment from `config/` into the declarative layout under `state/`. The migration copies state, archives the source, and imports the Kavita signing key so existing sessions survive. This document describes the legacy layout, the exact behavior of `celine migrate`, the signing-key rules, and the rollback and recovery paths.

## Legacy layout

The previous deployment ran Kavita from a top-level Compose file with the `lscr.io/linuxserver/kavita:latest` image and a Watchtower updater. Systemd units in the repository managed the container and took a config backup on shutdown. The unit files, helper scripts, and Compose file were removed from the repository when the declarative layout landed.

Data that remains on disk:

| Path | Contents |
| --- | --- |
| `config/` | Legacy Kavita configuration: `kavita.db`, `appsettings.json`, `bookmarks/`, caches, covers, logs, and served assets. |
| `config.backup/` | An older copy of the legacy configuration. |
| `backups/kavita_config_<date>.tar.gz` | Archives written by the old `backup-config.sh` script. Each archive stores a `config/` tree. |
| `data/Kavita/` | Kavita media (`Books`, `Comics`, `Manga`). The legacy Compose file mounted media from `/media/mei3/Kavita`, which no longer exists; the media is already in the repository data directory. |

The legacy layout does not need to stay installed. Keep `config/`, `config.backup/`, and the old archives until the declarative deployment has run without issues.

Before the first migration, disable any legacy units that are still installed on the host:

```bash
systemctl is-enabled kavita.service kavita-backup.service
sudo systemctl disable --now kavita.service kavita-backup.service
```

Neither unit is installed on this host.

## What `celine migrate` does

Run the command with an explicit confirmation:

```bash
celine migrate --yes
```

Flags:

| Flag | Effect |
| --- | --- |
| `--yes` | Required. The command copies data and may write a secret store. |
| `--force` | Replace an existing `state/kavita/config/kavita.db`. The old directory is renamed to `state/kavita/config.pre-migrate-<stamp>`. |
| `--no-secret-import` | Skip the signing-key import. SecretSpec generates a key on the first start, and existing sessions become invalid. |
| `--state-dir PATH` | Override `paths.state_dir` for this run. |
| `--backup-dir PATH` | Override `paths.backup_dir` for this run. |
| `--secret-file PATH` | Write the SOPS store here instead of `secrets/celine.enc.yaml`. |

Steps, in order:

1. Require `config/kavita.db`. A missing legacy database aborts the migration before any write.
2. Create the state and backup directories.
3. Archive the entire legacy `config/` directory to `backups/legacy-config-<UTC timestamp>.tar.gz` with mode 0600, then read the archive back and verify that it contains `kavita.db`. A failed verification aborts the migration.
4. Check for an existing `state/kavita/config/kavita.db`. Without `--force`, the command stops and tells you to rerun with `--force`. The legacy directory remains untouched.
5. Copy the legacy tree into a temporary staging directory under the state directory.
6. Run `PRAGMA integrity_check` on the copied database and read the user, library, folder, and version rows. A database with zero users aborts the migration, because installing it would replace a working deployment with an empty one.
7. When an existing state database is present and `--force` is set, rename `state/kavita/config` to `state/kavita/config.pre-migrate-<stamp>`.
8. Move the staged tree into `state/kavita/config`.
9. Unless `--no-secret-import` is set, read `TokenKey` from the copied `appsettings.json` and import it into the SOPS store. The signing-key rules are below.
10. On a successful import, write `state/.migration.json` with the migration time, source directory, archive path, Kavita version, user and library counts, and whether the token was imported.
11. Print the backup archive, state directory, Kavita inventory, signing-key result, any discoveries, and the next command, `celine up --build`.

The first migration on this host completed: one user, three libraries, database version 0.9.0.2, signing key imported, and the legacy archive at `backups/legacy-config-20261107T165434Z.tar.gz`.

## What it does not touch

- `config/` is read, never modified. It remains a complete rollback source.
- `config.backup/` and the old `backups/kavita_config_*.tar.gz` archives are left alone.
- Media in `data/` is not copied, moved, or modified.
- Existing SOPS stores that decrypt successfully are not rewritten. See the signing-key rules.
- Containers, images, Tailscale Serve handlers, and installed systemd units are not changed. The migration stops nothing, so stop the legacy deployment first.

## Signing-key import

Kavita signs session tokens with the `TokenKey` in `appsettings.json`. Reusing the legacy value keeps existing sessions valid after the move to Celine.

- `celine migrate` reads `TokenKey` from the copied legacy `appsettings.json`.
- When no SOPS store exists, the CLI writes a temporary plaintext YAML file with `KAVITA_TOKEN_KEY`, encrypts it with `sops encrypt --filename-override secrets/celine.enc.yaml --output secrets/celine.enc.yaml`, sets mode 0600, and decrypts it once to verify the key name is present.
- When the store exists and decrypts, the CLI leaves it unchanged and reports the signing key as imported. It does not compare the stored value with the legacy value.
- When the store exists and cannot be decrypted, the CLI records a discovery message, finishes the migration, and leaves the store in place. The first start then fails at secret resolution until the store is fixed.
- When the legacy `appsettings.json` has no readable `TokenKey`, the CLI records a discovery message and skips the import. SecretSpec generates a key on the first start.
- With `--no-secret-import`, the CLI skips the import entirely and does not write the migration marker.
- On a fresh store, generated keys invalidate old sessions. Users sign in again; reading progress and libraries are unaffected.

To force the legacy key into a store that already exists, move the store aside and migrate again. This replaces the key in use, so it also invalidates sessions that were issued with the previous key.

```bash
celine down
mv secrets/celine.enc.yaml secrets/celine.enc.yaml.old
celine migrate --yes --force
celine up
```

## Rollback

The migration is reversible from three independent sources.

### Re-run the migration from the untouched legacy configuration

Move the declarative state aside and import the legacy configuration again:

```bash
celine down
mv state/kavita/config state/kavita/config.current
celine migrate --yes
celine up
```

The migration archives `config/` again and imports the legacy database and signing key. The moved state directory remains on disk for inspection.

### Restore a celine archive

```bash
celine restore backups/celine-<stamp>.tar.gz --yes
celine up
```

Restore stops the services, takes a pre-restore backup, and copies `kavita/config/` and `calibre/` back over the state directory. Pair the restored database with a Kavita image version that can read it. See the upgrade recovery section.

### Return to the legacy deployment

`config/` is untouched, so the legacy configuration remains complete. The old Compose file, `backup-config.sh`, and systemd units were removed from the repository; reconstruct them or extract a `backups/kavita_config_*.tar.gz` archive, which contains a `config/` tree. Stop Celine first with `celine down` and remove the Tailscale handlers with `celine tailnet down` so the old deployment can bind the ports.

### Undo a forced migration

`celine migrate --force` renames the previous state to `state/kavita/config.pre-migrate-<stamp>`. Move it back after stopping the services:

```bash
celine down
mv state/kavita/config state/kavita/config.migrated
mv state/kavita/config.pre-migrate-<stamp> state/kavita/config
celine up
```

## If the Kavita database upgrade fails

Kavita upgrades its SQLite schema when it starts with a newer image. The live deployment upgraded the legacy 0.9.0.2 database to 0.9.1.4 successfully. These are the recovery paths for a failed upgrade.

Symptoms: the Kavita container restarts repeatedly, `celine logs kavita` shows a database migration or Entity Framework error, and `celine doctor` fails the Kavita database check or the port check.

1. Stop the containers.
2. Restore the newest celine archive, or re-import the legacy configuration.
3. Start with the earlier Kavita image tag, so the database schema matches the application version.
4. Verify with `celine doctor --strict`, then plan the upgrade again.

```bash
celine down
celine restore backups/celine-<stamp>.tar.gz --yes
# set kavita.image back to the tag that matches the restored database
celine up
```

When no celine archive is usable, re-import the untouched legacy state:

```bash
celine down
mv state/kavita/config state/kavita/config.failed-<stamp>
celine migrate --yes
# pin the earlier kavita.image tag before starting
celine up
```

Keep the failed state directory until the deployment is healthy. It contains the database that the newer image attempted to upgrade.

## After migration

1. Run `celine validate` and `celine doctor`.
2. Run `celine up --build`.
3. Run `celine doctor --strict`. The expected result is every check `ok`.
4. Confirm the tailnet URLs and sign in to Kavita. Existing sessions that used the imported signing key stay valid.
5. Keep `config/` and the legacy archives on disk until the declarative deployment has run successfully for a while.

The migration, the first start, the database upgrade, the first backup, and a restore into a separate `--state-dir` copy were exercised live on this host on 2026-11-07. The `--force` migration path, a restore over live state, and the failed-upgrade recovery above were not exercised against live state.
