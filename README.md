# Dendrite

[![Build status](https://github.com/ma10hh/dendrite/actions/workflows/dendrite.yml/badge.svg?event=push)](https://github.com/ma10hh/dendrite/actions/workflows/dendrite.yml) [![Dendrite](https://img.shields.io/matrix/dendrite:matrix.org.svg?label=%23dendrite%3Amatrix.org&logo=matrix&server_fqdn=matrix.org)](https://matrix.to/#/#dendrite:matrix.org) [![Dendrite Dev](https://img.shields.io/matrix/dendrite-dev:matrix.org.svg?label=%23dendrite-dev%3Amatrix.org&logo=matrix&server_fqdn=matrix.org)](https://matrix.to/#/#dendrite-dev:matrix.org)

Dendrite is a second-generation Matrix homeserver written in Go. It is currently in maintenance mode,
meaning only security fixes are being applied, for example [room version 12](https://matrix.org/blog/2025/08/security-release/).

> **This is a fork of [element-hq/dendrite](https://github.com/element-hq/dendrite)** that adds support for
> Simplified Sliding Sync (MSC4186), which Element X needs. See [Trying out this fork](#trying-out-this-fork).

It intends to provide an **efficient** and **reliable** alternative to [Synapse](https://github.com/matrix-org/synapse):

- Efficient: A small memory footprint with better baseline performance than an out-of-the-box Synapse.
- Reliable: Implements the Matrix specification as written, using the
  [same](https://github.com/matrix-org/sytest) test [suites](https://github.com/matrix-org/complement) as Synapse.

Dendrite is **beta** software, which means:

- Dendrite is ready for early adopters. We recommend running Dendrite with a PostgreSQL database.
- Dendrite has periodic releases. We intend to release new versions as we fix bugs and land significant features.
- Dendrite supports database schema upgrades between releases. This means you should never lose your messages when upgrading Dendrite.

This does not mean:

- Dendrite is bug-free. It has not yet been battle-tested in the real world and so will be error prone initially.
- Dendrite is feature-complete. There may be client or federation APIs that are not implemented.
- Dendrite is ready for massive homeserver deployments. There is no high-availability/clustering support.

Currently, we expect Dendrite to function well for small (10s/100s of users) homeserver deployments as well as P2P Matrix nodes in-browser or on mobile devices.

If you have further questions, please take a look at [our FAQ](docs/FAQ.md) or join us in:

- **[#dendrite:matrix.org](https://matrix.to/#/#dendrite:matrix.org)** - General chat about the Dendrite project, for users and server admins alike
- **[#dendrite-dev:matrix.org](https://matrix.to/#/#dendrite-dev:matrix.org)** - The place for developers, where all Dendrite development discussion happens
- **[#dendrite-alerts:matrix.org](https://matrix.to/#/#dendrite-alerts:matrix.org)** - Release notifications and important info, highly recommended for all Dendrite server admins

## This fork compared with upstream

The base is upstream `element-hq/dendrite` **0.15.2**. The fork keeps its storage, its configuration and its federation, and adds what
Element X and an administrator of a small server need. Everything new that changes how the server behaves is **off by default**.

| | Upstream 0.15.2 | This fork (`v0.15.2-f2`) | Default |
|---|---|---|---|
| Sync for **Element X** (Simplified Sliding Sync, MSC4186) | No | Yes | On |
| Sign-in through the **Matrix Authentication Service** (MSC3861), provisioning endpoints for MAS, QR code sign-in (MSC4388), appservice device management (MSC4190) | No | Yes, tested with MAS 1.29.0 in `contrib/mas-lab` | Off (`client_api.msc3861`) |
| Client API: knocking, thread list, room summary, bundled edits, threads and references, `state_after` in `/sync`, `timestamp_to_event`, room types in `/publicRooms` | Missing or partial | Yes | On |
| URL previews, with protection against requests to private networks | No | Yes | Off (`media_api.url_previews`) |
| Admin API | `/_dendrite/admin` | Also the Synapse admin API at `/_synapse/admin` (users, rooms, block list, media quarantine and deletion, registration tokens, shadow bans, rate limit overrides, account expiry, media statistics) | On, admins only |
| Media: quota per user, deletion of media nobody downloaded | No | Yes | Off (`media_api.max_user_storage_bytes`, `media_api.retention`) |
| Message retention policies (`m.room.retention`, MSC1763): old messages are deleted | No | Yes | Off (`sync_api.retention.enabled`) |
| Metrics | A few | Also the duration of every request, the database pools and the new features | With `global.metrics.enabled` |
| Joining big rooms without their member list (MSC3706) | No | Experimental, **do not enable** | Off (`federation_api.partial_state_joins`) |
| Configuration check | Unknown and repeated keys are ignored | **Refused at startup**, with their line numbers | On |
| Remote crash through an event whose room ID is `!:` (found by fuzzing) | Same code paths, not tested | Fixed | Fixed |
| Docker image | `ghcr.io/element-hq/dendrite-monolith` | `ghcr.io/ma10hh/dendrite-monolith:<tag>`, `linux/amd64`, no `latest` | |

Not available in either: workers, high availability, and Element Call (delayed events, MatrixRTC).

The whole list with the reasons is in [`CHANGES.md`](CHANGES.md), the decisions behind it in
[`docs/development/decisions.md`](docs/development/decisions.md), and what the tests say in [the supported features](#supported-features).

## Migrating from upstream Dendrite

For servers on upstream `element-hq/dendrite` (or `matrix-org/dendrite`) **0.15.x**, with PostgreSQL or SQLite. Your users, rooms, media,
signing key and configuration stay as they are: the fork reads the same databases and the same `dendrite.yaml`.

**What changes in the databases.** On the first start, the migrations add: four tables (`roomserver_partial_state_rooms`,
`roomserver_partial_state_events`, `roomserver_blocked_rooms`, `userapi_account_restrictions`), two columns of the media table (`quarantined`, `last_access_ts`, both with a default)
and two indexes on it (`user_id`, `base64hash`: a moment on a table with millions of rows). The sync API events table gets two columns (`origin_server_ts`, `is_state`) and an index,
and the migration reads every stored event once to fill them, which takes a while on a database with millions of events. Nothing is renamed or removed. Sliding sync sessions are
kept in memory, not in the database.

1. **Start from 0.15.2.** If you are on an older upstream version, update to 0.15.2 first, as its upgrade notes describe, and check that it runs.
2. **Read the upgrade notes** at the top of [`CHANGES.md`](CHANGES.md).
3. **Make a backup.** It is the only way back: the fork's migrations only go forward and going back to upstream is neither supported nor tested.
   Take the signing key (`global.private_key`), `dendrite.yaml`, the databases, the media directory and the JetStream directory,
   see [backup and upgrade](docs/administration/11_backup_and_upgrade.md).
4. **Check your configuration.** The fork refuses a configuration with a key it does not know or a key that appears twice, instead of ignoring it
   (typical: options that upstream removed, such as `kafka`, `server_key_api` or `pusher_database`). Write the file out again and compare:

   ```bash
   $ ./bin/generate-config -normalise dendrite.yaml > dendrite.normalised.yaml
   $ diff dendrite.yaml dendrite.normalised.yaml     # the keys that only the left file has are the ones the server would refuse (comments and order differ too)
   ```

   Remove those keys (or use the normalised file), and check that the server starts. The new options are all in `dendrite-sample.yaml` and all off.
5. **Switch the program.**
   - *From source:* build this repository with Go 1.25 or later: `go build -o bin/ ./cmd/...`.
   - *Docker:* replace the image (`ghcr.io/element-hq/dendrite-monolith` or `matrixdotorg/dendrite-monolith`) by `ghcr.io/ma10hh/dendrite-monolith:v0.15.2-f2` (an explicit tag: there is no `latest`). Volumes and the configuration stay as they are.
   - *Helm:* use the chart of this repository. Two values moved: `dendrite_config.global.profiling` is now `profiling`, and
     `dendrite_config.global.metrics.basic_auth.user` is now `username`. Move your overrides.
6. **Start it and look at the log** for the migrations and for configuration errors. Then check that the server answers and advertises the new sync:

   ```bash
   $ curl -s http://localhost:8008/_matrix/client/versions | grep simplified_msc3575
   ```

7. **Reverse proxy:** sliding sync holds requests open for up to three minutes. Set the read timeout to more than that (nginx's default of 60 seconds is
   too short; [the sample](docs/nginx/dendrite-sample.conf) uses 600).
8. **Opt in to what you want**, one thing at a time, in `dendrite.yaml`: media quota and retention (see [media storage](docs/administration/9_media.md)),
   URL previews, metrics ([what to watch](docs/administration/10_metrics.md)). Your existing clients keep working as before.
9. **Element X** needs sign-in through the Matrix Authentication Service. This is a change for all your users and the one step that is not reversible
   without a restore, so plan it on its own:
   - Run MAS and set `client_api.msc3861` with its shared secret, see [delegated authentication](docs/administration/7_delegated_auth.md).
   - Passwords, registration and logout of Dendrite stop working: the endpoints answer 404, and every client signs in through MAS. Old access tokens stop working,
     so everybody signs in again, as a new device. They need their recovery key or another signed-in session to get their message keys back.
   - Dendrite does not import passwords into MAS, and `syn2mas` cannot read a Dendrite database. Create the users in MAS with the **same localparts**. Dendrite matches
     accounts by localpart, so rooms and history stay with the user. The lab only tests new accounts, not the move of existing ones: try it on a copy first.
10. **Going back:** stop the server, restore the backup of step 3 (databases, media, JetStream, configuration), and start the upstream version you came from.

Questions and problems go to [this fork's issue tracker](https://github.com/ma10hh/dendrite/issues), not to upstream.

## Trying out this fork

This fork adds **support for Simplified Sliding Sync ([MSC4186](https://github.com/matrix-org/matrix-spec-proposals/pull/4186))**,
the API [Element X](https://element.io/) uses for fast login and sync. Upstream Dendrite is in maintenance mode and does not
have it, so Element X cannot sync with it. There is nothing to enable: build this fork, run it like any Dendrite, and it
advertises `org.matrix.simplified_msc3575` in `/_matrix/client/versions`.

**Status: in use.** It is used with Element X on a phone (since release `v0.15.2-f1` of this fork), is covered by unit and integration tests on both SQLite and PostgreSQL,
  and a load test with a 2000 room account is included. Please report what you find.
- Supported: room lists with ranges and the `is_dm`, `is_encrypted`, `is_invite`, `room_types`, `not_room_types`, `spaces`, `tags` and `not_tags` filters, room subscriptions, `required_state`, incremental updates with long polling,
  invites, knocks, and the `to_device`, `e2ee`, `account_data`, `receipts` and `typing` extensions.
- `set_presence` is applied like in `/sync`. There is no presence extension, and the `/v4/sync` dialect of the final MSC text is not implemented. See `docs/other/sliding-sync.md`.
- Sign-in through the Matrix Authentication Service ([MSC3861](https://github.com/matrix-org/matrix-spec-proposals/pull/3861)) is supported, see `docs/administration/7_delegated_auth.md`. The lab in `contrib/mas-lab` runs a scripted client against MAS and this server (161 checks); the Element X app itself has not been run against it by the tests.

To try it:

```bash
$ git clone https://github.com/ma10hh/dendrite
$ cd dendrite
# Go 1.25 is what the Dockerfiles use. With Nix, `nix-shell` provides Go and the other tools.
$ go build -o bin/ ./cmd/dendrite ./cmd/generate-keys ./cmd/create-account
```

Then follow [Get started](#get-started) below and check that the server advertises the feature:

```bash
$ curl http://localhost:8008/_matrix/client/versions | grep simplified_msc3575
```

In Element X, enter your server's address as the homeserver.

Things to know:

- **Reverse proxies** need a read timeout of more than three minutes, because requests that wait for changes are held open that long.
  nginx's default of 60 seconds is too short. The [sample nginx configuration](docs/nginx/dendrite-sample.conf) already sets
  `proxy_read_timeout` to 600 seconds.
- **Container images.** Release builds are published to the GitHub Container Registry as
  `ghcr.io/ma10hh/dendrite-monolith:<tag>`, where the tag is the upstream version followed by `-f` and a build number, for example
  `v0.15.2-f2`. Only `linux/amd64` is built, and there is no `latest` tag. Configuration and volumes work as described in
  [build/docker](build/docker).
- **Database changes.** Sliding sync sessions are kept in memory: after a restart, clients are told to start their sync again from scratch (`M_UNKNOWN_POS`), which the MSC requires clients to handle. Other features of this fork add tables and columns (partial-state rooms, blocked rooms, quarantined media, the time of the last download of a media), always by additive migrations that run at startup. Going back to upstream Dendrite or to an older version of this fork is not supported and not tested: take a backup first, see `docs/administration/11_backup_and_upgrade.md`.
- **Report problems on [this fork's issue tracker](https://github.com/ma10hh/dendrite/issues), not upstream's**, since upstream does not maintain this code. Include the Dendrite
  log around the time and what Element X showed.

More detail: [what is supported and how it behaves](docs/other/sliding-sync.md), and the
[decision log](docs/development/sliding-sync-decisions.md) with design choices, sources and open questions.
The code is in [`syncapi/slidingsync`](syncapi/slidingsync).

## Supported features

What this fork does, by area. "Tested" means automated tests on SQLite and PostgreSQL; the lab is `contrib/mas-lab`. Decisions behind each line are in the [decision log](docs/development/decisions.md).

| Area | Status | Notes |
|---|---|---|
| Client-server API for Element X: sync, sliding sync (MSC4186), rooms, messages, E2EE keys, cross-signing, push, media | Supported | Sliding sync: no presence extension, not the `/v4/sync` dialect. `/sync` supports `state_after` (MSC4222) |
| Sign-in through Matrix Authentication Service (MSC3861), provisioning endpoints for MAS, appservice devices (MSC4190), QR login rendezvous (MSC4388) | Supported, tested, lab | Dendrite has no login of its own in this mode. No SSO, SAML, CAS or JWT login: use MAS |
| Knocking, room summary (MSC3266), thread list (MSC3856), `timestamp_to_event` (MSC3030, from this server's copy of the room), bundled aggregations, URL previews (off by default, with SSRF protection), `context` state, public room types | Supported, tested | Not done: `mutual_rooms`, dehydrated devices (MSC3814), peeking, `/events` and `/initialSync` stubs |
| Federation | Supported | The upstream federation code, with knocking added. Complement runs against this fork, see `docs/development/decisions.md` for the results |
| Faster joins / partial state (MSC3706) | **Experimental, off, do not enable** | `federation_api.partial_state_joins`. Never tried against a real large room; see decision entries 73 to 79 for what is open |
| Synapse admin API (users, rooms, block list, media quarantine and deletion, registration tokens, shadow bans, rate limit overrides, account expiry, media statistics) | Supported, tested | What differs from Synapse is in `docs/administration/8_synapse_adminapi.md`. synapse-admin and Draupnir were not run against it |
| Media: quota per user, deletion of unused media | Supported, tested | Off by default, `docs/administration/9_media.md` |
| Metrics (Prometheus), backup and upgrade guides, moving from Synapse | Documented | `docs/administration/10_metrics.md` to `12_from_synapse.md`. There is no Synapse importer |
| Message retention policies (`m.room.retention`) | Supported, tested | Off by default, `docs/administration/13_message_retention.md`. A migration adds two columns to the sync API events table |
| Worker mode, high availability | Not supported | One process; scale it up |
| Element Call, delayed events (MSC4140), MatrixRTC (MSC4143) | Not supported | Out of scope for this fork |

## Requirements

See the [Planning your Installation](https://element-hq.github.io/dendrite/installation/planning) page for
more information on requirements.

To build Dendrite, you will need Go 1.25 or later.

For a usable federating Dendrite deployment, you will also need:

- A domain name (or subdomain)
- A valid TLS certificate issued by a trusted authority for that domain
- SRV records or a well-known file pointing to your deployment

Also recommended are:

- A PostgreSQL database engine, which will perform better than SQLite with many users and/or larger rooms
- A reverse proxy server, such as nginx, configured [like this sample](https://github.com/element-hq/dendrite/blob/main/docs/nginx/dendrite-sample.conf)

The [Federation Tester](https://federationtester.matrix.org) can be used to verify your deployment.

## Get started

If you wish to build a fully-federating Dendrite instance, see [the Installation documentation](https://element-hq.github.io/dendrite/installation). For running in Docker, see [build/docker](build/docker).

The following instructions are enough to get Dendrite started as a non-federating test deployment using self-signed certificates and SQLite databases:

```bash
$ git clone https://github.com/ma10hh/dendrite
$ cd dendrite
$ go build -o bin/ ./cmd/...

# Generate a Matrix signing key for federation (required)
$ ./bin/generate-keys --private-key matrix_key.pem

# Generate a self-signed certificate (optional, but a valid TLS certificate is normally
# needed for Matrix federation/clients to work properly!)
$ ./bin/generate-keys --tls-cert server.crt --tls-key server.key

# Copy and modify the config file - you'll need to set a server name and paths to the keys
# at the very least, along with setting up the database connection strings.
$ cp dendrite-sample.yaml dendrite.yaml

# Build and run the server:
$ ./bin/dendrite --tls-cert server.crt --tls-key server.key --config dendrite.yaml

# Create an user account (add -admin for an admin user).
# Specify the localpart only, e.g. 'alice' for '@alice:domain.com'
$ ./bin/create-account --config dendrite.yaml --username alice
```

Then point your favourite Matrix client at `http://localhost:8008` or `https://localhost:8448`.

## Contributing

We would be grateful for any help on issues marked as
[Are We Synapse Yet](https://github.com/element-hq/dendrite/labels/are-we-synapse-yet). These issues
all have related Sytests which need to pass in order for the issue to be closed. Once you've written your
code, you can quickly run Sytest to ensure that the test names are now passing.

If you're new to the project, see our
[Contributing page](https://element-hq.github.io/dendrite/development/contributing) to get up to speed, then
look for [Good First Issues](https://github.com/element-hq/dendrite/labels/good%20first%20issue). If you're
familiar with the project, look for [Help Wanted](https://github.com/element-hq/dendrite/labels/help-wanted)
issues.

## Copyright & License

Copyright 2017 OpenMarket Ltd
Copyright 2017 Vector Creations Ltd
Copyright 2017-2025 New Vector Ltd

This software is dual-licensed by New Vector Ltd (Element). It can be used either:

(1) for free under the terms of the GNU Affero General Public License (as published by the Free Software Foundation, either version 3 of the License, or (at your option) any later version); OR

(2) under the terms of a paid-for Element Commercial License agreement between you and Element (the terms of which may vary depending on what you and Element have agreed to).
Unless required by applicable law or agreed to in writing, software distributed under the Licenses is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the Licenses for the specific language governing permissions and limitations under the Licenses.
