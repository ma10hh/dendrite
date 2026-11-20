---
title: Backup and upgrade
parent: Administration
nav_order: 11
permalink: /administration/backup-and-upgrade
---

# Backup and upgrade

## What to back up

| What | Where | Why |
|---|---|---|
| The signing key | `global.private_key`, and the files of `global.old_private_keys` | Without it the server cannot prove who it is to other servers, and a new key on the same server name is not trusted by rooms that know the old one. Back it up first, and keep it apart from the other backups |
| The configuration | `dendrite.yaml`, and the registration files of application services | |
| The databases | `global.database` or the `database` of each component (`room_server`, `sync_api`, `user_api`, `media_api`, `federation_api`, ...) | Everything: rooms, events, accounts, devices, keys, media metadata |
| The media files | `media_api.base_path` | The database only holds the metadata |
| JetStream | `global.jetstream.storage_path` | Events that were accepted but not yet processed by every component. Without it after a restore, such events are lost for the components that did not get them yet |
| The search index | `sync_api.search.index_path` | Not needed: it can be rebuilt with `GET /_dendrite/admin/fulltext/reindex`, which takes time on a large server |

Also keep the configuration of the Matrix Authentication Service and its database if you use it: Dendrite does not hold the passwords or the sessions in that case.

## Taking a backup

The databases and JetStream have to match each other, so the cleanest backup is taken with Dendrite stopped:

1. Stop Dendrite.
2. Dump the databases (`pg_dump` for Postgres) or copy the SQLite files, and copy the media directory and the JetStream directory.
3. Start Dendrite.

If you cannot stop the server, use a single database for all components (`global.database`) so that one `pg_dump` is a consistent snapshot of all of them, and accept that the JetStream copy can be slightly behind or ahead of it: an event that was in flight at that moment can be missing from some component after a restore.

With several databases, one dump per database taken at different times is not consistent. Prefer one database.

## Restoring

1. Install the same version of Dendrite as the one that took the backup, or a newer one.
2. Restore the configuration, the signing key, the databases, the media files and the JetStream directory, all from the same backup.
3. Start Dendrite. Events that arrived or were sent between the backup and the failure are not in it: your users can miss messages of that time, and other servers can have events that this one does not. Whether they come back through federation or backfilling is not guaranteed.

Never run two servers with the same signing key and server name at the same time.

## Upgrading

Dendrite changes its databases itself at startup: the migrations are applied in order, once, and only forward. A downgrade is not supported, as an older version does not know the changes of a newer one. So:

1. **Read `CHANGES.md`**, in particular the *Upgrade notes* of every version between yours and the new one. Dendrite refuses a configuration with keys it does not know, and names them with their line numbers; `generate-config -normalise dendrite.yaml` writes the file out in the current form.
2. **Take a backup** as above. It is the only way back.
3. Stop Dendrite, replace the binary or the container image (use an explicit version tag, never `latest`), and start it. The first start can take a while if a migration has a lot to do, and logs what it does.
4. Check the log for errors and `/_matrix/client/versions`.

`cmd/dendrite-upgrade-tests` runs the upgrade from earlier releases against a database that was filled by them; the CI uses it.

## Partial-state joins

If `federation_api.partial_state_joins` is on (experimental, off by default), rooms that were joined with partial state are listed in the table `roomserver_partial_state_rooms` until their full state has been fetched. A backup taken in that time restores into the same situation, and the fetching carries on after the start.
