---
title: Sliding sync decision log
parent: Development
nav_order: 5
permalink: /development/sliding-sync-decisions
---

# Simplified Sliding Sync (MSC4186): decision log

Working notes for reviewing the implementation of `POST /_matrix/client/unstable/org.matrix.simplified_msc3575/sync`.
Each entry says what was decided, why, and what a reviewer may want to challenge.
The original implementation plan was approved before the sources below were read; **section "Corrections to the approved plan" lists where reading them changed it.**

**Status: all six phases are implemented and tested, and the feature is advertised in `/versions`. It has not yet been tried with a real Element X client (open item K).**

## Sources used to pin the wire format

Fetched on 2026-11-19 and read in full:

- The MSC text, taken from the pull request diff: `https://github.com/matrix-org/matrix-spec-proposals/pull/4186.diff`.
- Ruma's client API types, which matrix-rust-sdk (and therefore Element X) uses:
  `crates/ruma-client-api/src/sync/sync_events/{msc4186,v4}.rs` in `github.com/ruma/ruma`, branch `main`.

They describe two different dialects:

| | Experimental dialect (Synapse's implementation, **Ruma `msc4186`**, Element X today) | Dialect in the current MSC text (`/v4/sync`) |
|---|---|---|
| Path | `/_matrix/client/unstable/org.matrix.simplified_msc3575/sync` | `/_matrix/client/v4/sync` |
| `pos`, `timeout`, `set_presence` | query parameters | request body |
| List window | `ranges: [[0,19],...]` | `range: [0,19]` |
| `required_state` | `[["type","state_key"],...]` with `*`, `$LAZY`, `$ME` | `{include, exclude, lazy_members}` |
| Invites | `invite_state` | `stripped_state`, plus `membership` and `lists` fields |
| Unread counts | `notification_count`, `highlight_count` | removed |
| Filter | `is_invite` | `is_invited` |
| Sticky lists | no (see decision 3) | no |

**Decision: implement the experimental dialect on the unstable path only.** That is what Element X sends today, which is the goal of this work.
The `/v4/sync` dialect is out of scope; it can be added later as a second route that reuses the same core.

## Developer environment

| Decision | Why |
|---|---|
| `shell.nix` at the repo root; run `nix-shell` to get Go 1.25, gcc (cgo), gopls, golangci-lint, psql and a `dev-postgres` helper. | The machine is NixOS with no global Go. `go.mod` says `go 1.23.0` with `toolchain go1.24.3`, the Dockerfiles use 1.25, so `go_1_25` is used. |
| `dev-postgres start` runs a throwaway server on a unix socket in `./.pgsock` with data in `./.pgdata`. | `test/db.go` creates a per-package database from `POSTGRES_HOST` / `PGHOST`. Both directories are hidden, so the existing `.gitignore` rule `.*` already ignores them. |
| Lint with `golangci-lint run --no-config ...`. | The nixpkgs golangci-lint is v2 and rejects the repo's v1 `.golangci.yml` (`output.formats` is a slice). Upgrading the repo config is out of scope. Without the repo config gocyclo (25) and misspell (UK) are not checked, so keep functions small by hand. |

## Environment findings

- **Pre-existing test failure, unrelated to this work:** `go test -race ./syncapi/storage/tables/` fails on a clean `HEAD`
  (`TestMembershipsTable`, `TestReindex`, `TestTopologyTable`; reproduced 4 of 4 times in a clean worktree).
  It is a data race between the parallel sqlite and postgres subtests, which share one event object whose `EventID()` is lazily cached by `gomatrixserverlib`.
  It is a test-code race, not a product bug. Table tests added by this work should build their events per subtest.
- **Pre-existing lint findings** in `syncapi/syncapi_test.go` (duplicate `roomserver/api` import, unchecked `Body.Close`). Not touched.
- `gomatrixserverlib` at the pinned version has **no sliding-sync types**, so the types are defined in `syncapi/slidingsync/types.go`.

## Corrections to the approved plan

| Plan said | Sources say | Action taken |
|---|---|---|
| Parameters are sticky (omitted fields keep earlier values); store merged lists per connection. | Not sticky. The MSC struck "make lists sticky" from its change list, and Ruma always serialises `ranges` and `timeline_limit` but **omits an empty `required_state`**, so "omitted means keep" would resurrect stale state. | Removed the `Merge` logic and the stored lists from phase 1. Each request fully defines the rules. |
| Response key is `timeline_events`. | Key is `timeline`. (`timeline_events` is only a Python attribute name in Synapse.) | Fixed in `types.go`; covered by `TestEncodeResponseUsesClientFieldNames`. |
| Sort lists by `bump_stamp`. | MSC: lists are ordered by **last activity of any event**; "Rooms are *not* ordered by `bump_stamp`". `bump_stamp` is a separate output field, advanced only by `m.room.create`, `m.room.message`, `m.room.encrypted`, `m.sticker`, `m.call.invite`, `m.poll.start`, `m.beacon_info`. | Phase 2 needs two aggregates per room: max stream id of any non-backfilled event (ordering) and max stream id of bump-type events (`bump_stamp`). |
| Send `knock_state`. | Ruma's room type has only `invite_state`. | Open item E. |
| Rooms the user left are listed. | MSC: left rooms only appear if previously sent on the connection; kicked and banned rooms always appear (banned-but-never-joined excluded). | Deferred to phases 3 and 4, see decision 19. |
| `/versions` flag in phase 1. | Unchanged, but deferred (decision 1). | See decision 1. |
| Filter named `is_invite`; room types `["m.space", null]`. | Confirmed for the experimental dialect; `null` means "rooms without a type". | Modelled with `[]*string`. |

## Phase 1: skeleton, sessions, route (done)

Files: `syncapi/slidingsync/{types,connection,handler}.go` plus tests, `syncapi/slidingsync_test.go`, and wiring in `syncapi/routing/routing.go` and `syncapi/syncapi.go`.

| # | Decision | Why / trade-off |
|---|---|---|
| 1 | **Route is registered unconditionally, but `/versions` does not advertise `org.matrix.simplified_msc3575` yet.** Deviates from the plan. | A non-functional endpoint that is advertised makes Element X select it and then fail. The flag is added in the final phase, once to_device and e2ee work. |
| 2 | Route on the existing `v1unstablemux` subrouter, POST and OPTIONS, via `MakeAuthAPI` with `WithAllowGuests()`. | Verified by `TestSlidingSyncRoute` through the real router and auth on sqlite and postgres: 401 without or with a bad token, and legacy `GET /v3/sync` is unaffected. |
| 3 | **Requests are not sticky.** Lists, subscriptions and extensions are taken from each request as sent. | See corrections table. Per-room "already sent" state, which phase 4 needs, will live in the connection, not the request rules. |
| 4 | `pos` is `<random session id>/<sequence>`. A request without `pos` always starts a **new session** and replaces any connection with the same `(user, device, conn_id)`. | `M_UNKNOWN_POS` comes for free after restart, expiry or replacement. A `pos` is only resolved against the authenticated user and device (MSC requirement). The stream position the connection has served will be stored server-side, not encoded in `pos` (open item B). |
| 5 | Connections are in memory only: TTL 30 minutes, at most 10 per device (least recently used evicted). | Matches the approved choice. Constants are `DefaultConnectionTTL` and `DefaultMaxConnectionsPerDevice`, not configurable yet. |
| 6 | Expiry is swept opportunistically, at most once a minute, inside `Create` and `Get`; no goroutine. | Same pattern as the notifier's `_removeEmptyUserStreams`. Cost: an idle server never frees memory until the next request. Bounded per device, so acceptable. |
| 7 | Requests on one connection are serialised with a channel lock that respects context cancellation. A cancelled waiter gets HTTP 408. | The MSC says clients must cancel an in-flight request before sending another on the same connection, so waiting is rarely needed. A plain mutex would let abandoned long-polls pile up. |
| 8 | **Retry:** the last response is kept. A request repeating the `pos` of the previous request gets that response replayed; any older `pos` gets `M_UNKNOWN_POS`. | MSC: the server cannot assume the client received a response until it sees the next `pos`, so repeating the previous `pos` must work. Limitation: the replay ignores a changed body (the MSC allows a changed body on retry). Missing rooms are simply delivered on the following request, because only rooms actually returned are marked as sent. Phase 4 can improve this by recomputing from the pre-response state. |
| 9 | `pos` with sequence `0` is validated like any other (explicit `hasPos`). | Caught in review of my own first draft: using `seq != 0` as "pos supplied" let `<session>/0` bypass validation. Regression test added. |
| 10 | Limits: at most 100 lists, 100 room subscriptions, `conn_id` at most 64 bytes, else `400 M_INVALID_PARAM`. | The first two are in the MSC; the `conn_id` bound is mine, to keep connection keys small. Ruma documents 16 characters client-side, so 64 leaves headroom. |
| 11 | `txn_id` is echoed in the response, including on replay. | Present in Ruma's request and response; clients use it to learn which parameters reached the server. |
| 12 | Phase 1 ignores `timeout` and `set_presence`. | Nothing to wait for yet. They are added with the long-poll loop in phase 4 (`timeout` defaults to 0, and is ignored on the first request of a session, per the MSC). |
| 13 | `M_UNKNOWN_POS` is a local constant `spec.MatrixErrorCode`, HTTP 400. | `gomatrixserverlib` has no such code; avoids touching a dependency. |
| 14 | Wire types for the full room response are defined now (`RoomResponse`, `Hero`) and tested for JSON key names. | Pins the dialect early. `Avatar` is a `json.RawMessage` so a removed avatar can be an explicit `null`, which differs from "unchanged". |

## Phase 2: room list (done)

Behaviour: a request with `lists` and/or `room_subscriptions` returns `lists.<name>.count` and, for rooms the connection has not received yet, an initial `RoomResponse` with name, avatar, `is_dm`, `bump_stamp`, `joined_count`, `invited_count`, a timeline with `limited` and `prev_batch`, or `invite_state` for invites.
Rooms already sent on the connection are not returned again, **so updates to already-sent rooms are not delivered until phase 4**. This is why the endpoint is still not advertised in `/versions`.

New code: `syncapi/slidingsync/{roomlist,candidates,roomdata}.go` (+ tests), two table queries in both backends, and three `DatabaseTransaction` wrappers.

| # | Decision | Why / trade-off |
|---|---|---|
| 15 | Lists are ordered: invites first (newest first), then joined rooms by latest sync-visible event, ties by room ID. | MSC: order by last activity, not `bump_stamp`. Invites sort first because invite stream ids and event stream ids are **not comparable on SQLite** (separate counters), and an invite hidden below a window would be missed. Synapse sorts invites by recency instead; clients that care use the `is_invite` filter. |
| 16 | New query `Events.SelectRoomActivity`: per room, latest non-backfilled event position and latest position of a bump-type event. | Postgres probes the `(room_id, exclude_from_sync, id, ...)` index once per room (newest first) instead of aggregating every event. SQLite aggregates, in chunks of 500 rooms (bind variable limit). Bump types: `m.room.create`, `m.room.message`, `m.room.encrypted`, `m.sticker`, `m.call.invite`, `m.poll.start`, `m.beacon_info` (from the MSC text). |
| 17 | New query `CurrentRoomState.SelectStateEventsForRooms`: one state event per room for a `(type, state_key)`. | Batch lookups for names, avatars, `m.room.encryption` and `m.room.create` instead of one query per room. Phase 3's `required_state` can reuse it. |
| 18 | Expensive filter attributes (`is_encrypted`, `room_types`) are only loaded when a list filters on them; `m.direct` is always loaded because `is_dm` is returned on rooms. | Keeps the common request (no filters) cheap. |
| 19 | Only **joined and invited** rooms are candidates. Knocked, left, kicked and banned rooms are not listed. **(Kicked, banned and knocked rooms were added in phase 3; self-left rooms remain unlisted.)** | The MSC lists kicked/banned rooms and previously sent left rooms. Telling kick from self-leave needs the membership event's sender, and left rooms need the per-connection "sent" state; both belong with phases 3 and 4. Subscribing to a room that is not listed returns nothing. |
| 20 | Invite attributes (`is_encrypted`, room type) are read from the stripped `invite_room_state`; invites from ignored users, and invites for rooms already joined, are skipped. | Matches the legacy invite stream. |
| 21 | `timeline_limit` is capped at 100 and negative values become 0; 0 means no timeline and no `prev_batch`. | Bounds work per room; the MSC allows the server to cap it. |
| 22 | Combined config for a room matched by several rules: maximum timeline limit, deduplicated union of `required_state`. | MSC "Combining room configs". `required_state` is combined but not yet applied (phase 3). |
| 23 | Timeline uses `RecentEvents` + the legacy `ApplyHistoryVisibilityFilter` + ignored-user filtering, and **fails closed**: if the visibility check errors, the room is returned with an empty, limited timeline. | Privacy over completeness. `streams.applyHistoryVisibilityFilter` was **renamed to the exported `ApplyHistoryVisibilityFilter`** (3 call sites in `stream_pdu.go`, no behaviour change) so the check is shared, not copied. A mutation test confirmed `TestHistoryVisibilityLimitsTheTimeline` fails when the filter is bypassed. |
| 24 | ~~`name` falls back to the canonical alias~~ **Superseded by decision 32.** | Synapse does not do this. |
| 25 | `is_dm` is only sent when true, `initial` is always true in phase 2. | MSC: absent in an initial response means false. |
| 26 | The timeline range is bounded with `math.MaxInt64` rather than the current maximum PDU position. | See "latent SQLite bug" below. The read snapshot already gives a consistent view. |
| 27 | `Handler.fill` is a function field defaulting to `fillResponse`. | Lets session-handling unit tests run without a database; the real path is covered by the integration tests. |
| 28 | `Connection.sent` is a plain set of room IDs, reset with the session. | Placeholder for phase 4, which needs per-room positions to deliver updates. |

### Findings while testing (for the reviewer)

- **Latent bug in existing SQLite code:** `outputRoomEventsStatements.SelectMaxEventID` (`syncapi/storage/sqlite3/output_room_events_table.go`) runs `sqlutil.TxStmt(txn, stmt)` and then `defer`s `stmt.Close()`. SQLite snapshots have no transaction, so `TxStmt` returns the **shared prepared statement** and it is closed for good: the second call fails with `sql: statement is closed`. The legacy `/sync` only calls it at start-up, so it never showed. Not fixed here (out of scope); worth a separate fix. The same shape may exist in other SQLite methods.
- **`IgnoresForUser` returns `sql.ErrNoRows` for users who never ignored anyone.** Callers must treat that as "no ignores" (the legacy code does; see `ignoredUsers` in `candidates.go`). Missing this would have returned HTTP 500 to most users. Caught by the integration tests.
- **Storage fixtures must mirror the consumer.** The `sender` column is filled from `event.UserID` (the parsed sender), and for invites `UserID` is the *invited* user. Tests that write events without setting it silently store an empty sender, which makes sender filters (ignored users) look broken. The test harness in `integration_test.go` sets both.
- **Pre-existing:** the `syncapi/storage/tables` race (see above) is timing dependent: which of its tests fail varies between runs, and none of the new tests are among them.

## Phase 3: required_state, heroes, counts, kicked/banned/knocked rooms (done)

Behaviour: joined rooms now carry `required_state`, `heroes`, `notification_count`/`highlight_count`; kicked and banned rooms and knocks are listed.
Still initial-only: updates to rooms already sent are phase 4.

The semantics below come from reading Synapse's `develop` branch (`handlers/sliding_sync/{__init__,room_lists}.py`, `types/handlers/sliding_sync.py`, `rest/client/sync.py`), fetched 2026-11-19.

New code: `requiredstate.go` (pure matcher), `statefetch.go`, `candidates_left.go`, a rewrite of `roomdata.go`, three table/transaction additions (`SelectStateEventsWithKeys`, `RoomMemberCounts`, `RoomHeroes`), and a notifier fix. Tests: matcher unit tests, three integration test files, a notifier test and a table test.

| # | Decision | Why / trade-off |
|---|---|---|
| 29 | `required_state` pairs follow Synapse: `*` as type or key is a wildcard, `$ME` is the user, `["m.room.member","$LAZY"]` is lazy loading. `$LAZY` for other types is an ordinary key. | Matches what Element X expects. Pure logic in `stateMatcher`, tested without a database. |
| 30 | **`["*","*"]` returns all state including every member, and discards `$LAZY` and everything else** requested alongside it. | That is what Synapse implements. The MSC3575 text quoted in Synapse's comments says extra entries *filter out* from the set; Synapse does not do this, so neither do we. Element X's usual requests have no `["*","*"]`, so this rarely matters. |
| 31 | Lazy members = membership events of every timeline sender and the target of every membership event in the timeline. **The user's own membership is not implied**; clients ask with `$ME`. Timeline events are not deduplicated against `required_state`. | Synapse behaviour. Incremental "all membership changes since last sync when not limited" needs per-connection state and comes with phase 4. |
| 32 | `name` is only `m.room.name`; no canonical alias fallback and no name computed from heroes. | Synapse behaviour; Element X derives the display name from `required_state` and heroes. Replaces decision 24. |
| 33 | Heroes: only for joined rooms with no (or an empty) `m.room.name`; the requester is excluded; joined and invited users first, else left and banned ones; at most 5, oldest first; each with `displayname` and `avatar_url` from the member event. | Synapse behaviour. A canonical alias does **not** suppress heroes (the existing `GetRoomSummary` does suppress them, which is why `RoomHeroes` was added instead of reusing it). |
| 34 | `notification_count` / `highlight_count` are real values from the notification table, and always sent as explicit numbers (0 when there is nothing). | Synapse always sends 0 (its handler hardcodes it with a TODO), so clients that rely on server counts get less there. Dendrite has the data, so sending it is more useful, and it matches the legacy `/sync`. |
| 35 | Exact `(type, state_key)` pairs are looked up for all rooms at once, one query per distinct pair. Only rooms using wildcards read their state per room. Lazy members are one keyed query per room. | Keeps a request for many rooms with the same config (the common Element X list request) at a handful of queries. |
| 36 | **Kicked and banned rooms:** a kick is a `leave` whose sender is not the user; a ban counts only if the user was `join`ed immediately before it. Rooms the user left themselves, and bans of someone who never joined (including after only an invite), are not listed. | Synapse tells kicks apart the same way. For bans the MSC says never-joined bans are excluded; "joined just before" is a conservative approximation of that. Left-then-banned users are not shown the room. |
| 37 | Kicked/banned rooms return **the timeline up to and including the leave event, and nothing else**: no `required_state`, name, avatar, heroes or counts. They sort and get their `bump_stamp` from the leave event's position. | Synapse returns state as of the leave event, which Dendrite's current-state table cannot give. Returning current state would leak changes made after the user lost access, so it is omitted. Verified by a test, and by a mutation that unbounds the timeline. |
| 38 | **Knocked rooms** are returned like invites, as `invite_state`: the stripped knock event preceded by the room's stripped state, with no timeline. The stripped state is `unsigned.knock_room_state` if the knock carries it (remote), otherwise the room's current create, name, avatar, topic, join rules, canonical alias and encryption events. They sort by the knock event's position and do not match `is_invite`. | Synapse serialises knocks under `invite_state` (there is no `knock_state`). Events are reduced to type, state key, sender and content. |
| 39 | The notifier now wakes the target of a `knock` membership event, like it does for an invite. | A knocking user is not a member, so was never woken. The only change to existing sync code besides decision 23. Test fails without it. |

### Known limitations after phase 3

- For pseudo-ID room versions, matching `$ME` and wildcard-type pairs compares against raw state keys (sender IDs), so those pairs can miss there. Exact lookups use the resolved user ID like the rest of the sync API. Needs a pseudo-ID room to verify.
- Heroes and member counts cost two to four queries per room returned. Fine for list windows; revisit if profiling shows it.
- Kicked/banned detection reads the member event of every room the user ever left, in one query, but then one position lookup per kick or ban.
- A room the user left, was kicked from, and that was later purged or forgotten is not handled specially.

## Phase 4: incremental updates and long-polling (done)

Behaviour: a request with `pos` now returns only what changed since the connection last heard about each room, and with `timeout` it waits until there is something to return. Rooms already sent are no longer silent (this replaces the limitation noted under phase 2).

New code: `sentstate.go`, `rooms.go` (view + diff), a rewrite of `handler.go` around `respond`, `candidates` taking the connection's sent state, and `roomdata.go` helpers for timelines since a position. Tests: `integration_update_test.go` (12 scenarios on both databases, including long-poll timing and cancellation).

| # | Decision | Why / trade-off |
|---|---|---|
| 40 | **Every room is built as a view of its current data and diffed against `roomSent`**, what the connection was last told. Only fields that differ are sent; a room that did not change is omitted but its remembered state still advances. | One code path gives initial sends, updates, config expansion and "nothing to say". The MSC says an omitted field means "unchanged" in non-initial responses. |
| 41 | The per-room position is **the room's latest event position in the same snapshot** (`RoomActivity.LastEvent`), not a global stream position. Resolves open item B: `pos` stays an opaque `session/sequence`. | Consistent with the events actually read. Avoids `MaxStreamPositionForPDUs`, which hits the SQLite bug noted under phase 2. |
| 42 | Rooms with no new events only re-check **notification counts and `is_dm`**; everything else (name, avatar, heroes, member counts, state, timeline) is only read for rooms that are fresh, have new events, or have a larger timeline limit. | Those two do not come from room events, so event positions cannot reveal their changes. Keeps an idle list cheap: one counts query and the activity query. |
| 43 | Timeline of an updated room: the newest `timeline_limit` events newer than the last position. More than that sets `limited` and `prev_batch`. `num_live` is the number of returned events that are newer than the last position, and is absent on initial sends. If every new event is hidden (ignored user, history visibility), the room is not returned but its position still advances. | Matches Synapse, where `num_live` is 0 on first send. |
| 44 | **Expanded timeline:** a larger `timeline_limit` returns the room straight away with the newest N events flagged `unstable_expanded_timeline` (the name Synapse's experimental dialect uses). A smaller one just lowers the remembered limit, so growing it again resends. | MSC "changing room configs". Simple and correct; may resend events the client already has, which the MSC permits. |
| 45 | `required_state` updates are the **current matching state events whose event ID differs from the one last sent** for that `(type, state_key)`. New entries in the request therefore send their state, and lazy members already sent are not repeated. State that was deleted is not announced. | The experimental dialect has no deletion stubs. Costs one map of event IDs per room, bounded by what the client requested. |
| 46 | A removed name is sent as `""` and a removed avatar as an explicit `null`. Heroes are re-sent when the hero set changes in an unnamed room, and never for named rooms. | Name is not nullable in this dialect; `avatar` is. |
| 47 | Rooms that are not joined (invite, knock, kick/ban, left) are sent once and again when their membership event changes or the kind of room does. **A joined room that replaces an invite is sent as `initial`.** A room the user left themselves is now listed as left **if the connection had already received it**, with the timeline ending at the leave. | Resolves open item G and the MSC rule that clients learn a room is gone. |
| 48 | **`timeout`** is in milliseconds, defaults to 0, is capped at 3 minutes (under the server's 5 minute write timeout), is rejected if negative or not a number, and is ignored on the request that starts a session. | MSC: omitted or 0 returns immediately. |
| 49 | The long poll takes a notifier listener **and the current position before looking at the database**, so a change that lands in between wakes it immediately. It recomputes on every wake-up and keeps waiting if the result is empty. "Empty" means no rooms (and, from phase 5, no extension data); list counts alone do not end a poll. | Same pattern as legacy `/sync`. Unrelated wake-ups (another room's typing, for instance) just cost one recomputation. |
| 50 | **Nothing is remembered until the response is on its way.** `fill` returns a `commit` function that the loop only calls when it returns a response. A request cancelled while waiting returns 408 and leaves the connection untouched. A timed-out wait returns an empty response that still moves `pos` on and carries the list counts. | A dropped response must not swallow updates: the client retries the same `pos`, and either gets the stored response replayed or recomputes from unchanged state. |
| 51 | The device's last-seen address and user agent are refreshed at most every 5 minutes per connection, in the background. | Legacy `/sync` does this; without it devices would look idle. Uses `sync_api.real_ip_header` like the legacy code. |
| 52 | **`set_presence` is accepted and ignored.** | The legacy presence code (`updatePresence`) is tied to process-wide state plus a producer and consumer, and presence is outside the agreed first release. Element X's reliance on it was not verified (open item I). |

### Findings while testing (for the reviewer)

- Repeating an older `pos` replays the previous response, as designed (decision 8). My first draft of a test reused one by mistake and got an empty response back, which is the replay working.
- Mutation checks: resending unchanged state fails `TestIncrementalRoomAttributes`; ignoring the "since" position fails `TestIncrementalTimelineUpdates`.
- The whole `-race` run passes, including the long-poll goroutines.

### Known limitations after phase 4

- **Every wake-up recomputes the candidate list** (memberships, room activity, invites, filter state): O(rooms the user is in) per wake-up per waiting connection. Fine for typical accounts; for very large accounts cache candidates per connection, keyed by the notifier position.
- Membership events of rooms the user left are read on every request (one query).
- Wake-ups only come from what the notifier already tells devices about. Notification-count changes wake a device through the existing `OnNewNotificationData`; to-device and account data wake-ups matter once phase 5 adds those extensions.
- A room whose events are all hidden from the user is omitted without telling the client; nothing is lost, as its position advances.

## Phase 5: extensions (done)

Behaviour: `to_device`, `e2ee`, `account_data`, `receipts` and `typing` work. Together with phases 1-4 this is what Element X needs to log in, receive keys and keep an encrypted session going.

The semantics come from Synapse's `handlers/sliding_sync/extensions.py` and Ruma's request types (the same sources as phase 3). New code: `extensions.go` (request parsing, response types), `extrun.go` (orchestration and scoping), `ext_todevice.go`, `ext_e2ee.go`, `ext_rooms.go` (account data, receipts, typing), per-connection `extensionState` and per-room positions in `roomSent`. Tests: `integration_ext_test.go` (14 scenarios on both databases).

| # | Decision | Why / trade-off |
|---|---|---|
| 53 | Extension configs are read from every request (not sticky). `enabled` defaults to false. Unknown extensions are ignored. A malformed `since`, a negative `limit` or a wrongly typed field is `400 M_INVALID_PARAM`. | Synapse behaviour. |
| 54 | An enabled extension is **always present in the response**, even empty. A response only ends a long poll for extension data if there is something to act on: to-device messages, device list changes, or any account data, receipts or typing. **One-time key counts alone do not**; they ride along with the next response. | Synapse's `__bool__` rules. Element Android needs the e2ee key present (the comment cites its issue 3725). |
| 55 | The notifier position is read **once before computing each attempt**, and every extension reads up to it. Extension positions (connection-wide and per room) only move in the same `commit` that records the response. | A dropped or cancelled response loses nothing, and a change that lands mid-computation wakes the next attempt. |
| 56 | **Scope:** room-based extensions only cover **joined rooms that are in this response**, as in Synapse. `rooms` and `lists` default to `["*"]`; `*` in `rooms` is every room in the response, a named list is the rooms in its window, a named room counts only if it is in the response. | Element X was written against this. It means a quiet room's typing or receipts wait until the room next appears (open item J). |
| 57 | **to_device:** the client's `since` is the only cursor and also the acknowledgement: messages up to it are deleted, in the same step that records the response. `limit` defaults to and is capped at 100. `next_batch` is the position of the last message returned, or the current position when there is none, so the extension is present even when empty. Messages from ignored users are dropped but still acknowledged. | Synapse behaviour. The deletion reuses `CleanSendToDeviceUpdates`, whose bound is inclusive. |
| 58 | **e2ee:** one-time key counts and unused fallback key types come from `internal.DeviceOTKCounts` and are sent with **every** response. `device_lists` are relative to the connection's previous e2ee response and absent on the first. A failure to read key counts is logged and sent as empty rather than failing the sync. | Synapse behaviour. |
| 59 | **Device list changes are derived from membership deltas and key change offsets, not from the rooms in the response.** Rooms the user joined or left since the last e2ee response come from `GetStateDeltas`; they feed the existing `TrackChangedUsers`; users whose keys changed are kept only if they share a room with the user (the requester always counts). The requester's own join and leave are not reported as someone else's change. | Legacy `/sync` infers joined and left rooms from the rooms present in its response, which sliding sync only partly returns, so that approach would miss changes. |
| 60 | **account_data:** global data since the connection's last global position; a room's data since that room's own position (zero means everything). Room positions live in the room's remembered state, so a room that was not in a response catches up when it next is. Push rules and room tags are ordinary account data in Dendrite and need no special handling. | Matches Synapse's per-room "previously" tracking, simplified to one position per room. |
| 61 | **receipts:** a room sent in full (or with an expanded timeline) gets **all** its receipts; other rooms get those since their own position. Other users' private receipts and ignored users' receipts are never sent, and the shape is the legacy `m.read` one. | Synapse sends receipts only for events in the returned timeline plus the user's own; sending all is a superset and simpler. |
| 62 | **typing:** stateless, as in Synapse. A room sent in full gets who is typing now (nothing if nobody); other rooms get what changed since the last response that carried typing. Ignored users are left out. | A typing update missed because its room was not in a response is not recovered; typing times out anyway. |

### Findings while testing (for the reviewer)

- **A real bug caught by the tests:** for `rooms: ["*"]` the scope was built by assigning the "rooms in the response" set and then adding list windows to it, which also changed the set it was later intersected with, so rooms *not* in the response leaked in. `TestAccountDataOfARoomNotInTheResponseArrivesWhenItAppears` caught it.
- **A test that proved nothing:** the first version of the private receipt test gave the same user a public and a private receipt, so the user showed up either way. A mutation check (removing the filter) exposed it; the test now uses a user who has only a private receipt.
- Other mutation checks: not deleting acknowledged to-device messages fails `TestToDeliveryLimitAndAcknowledgement`; letting key counts end a long poll fails `TestKeyCountsAloneDoNotEndALongPoll`.
- The harness now announces written events to the notifier, as the consumer does, so positions in tests are real.

### Known limitations after phase 5

- Receipts and typing for a room only reach the client when that room is in a response (decision 56); account data and receipts are held per room and not lost, typing is.
- `device_lists.changed` after a room join includes everyone in the new room that was not already shared, as the legacy code does, which can over-report.
- One-time key counts are queried on every response where e2ee is enabled.
- Pseudo-ID room versions are untested for device list tracking.

## Phase 6: hardening, advertising, documentation (done)

New code: request caps and pruning in `handler.go`, `metrics.go`, `unstableFeatures` in `clientapi/routing/routing.go`, `docs/other/sliding-sync.md`, README and CHANGES entries.

| # | Decision | Why / trade-off |
|---|---|---|
| 63 | **`org.matrix.simplified_msc3575` is advertised unconditionally in `/versions`**, from a small `unstableFeatures()` function so it can be unit tested. Resolves decision 1 and open item D. | The feature is complete enough for Element X, and the flag is how clients decide to use it. |
| 64 | Requests are rejected with `400 M_INVALID_PARAM` beyond 100 ranges per list, 500 `required_state` entries per list or subscription, and 500 named lists or rooms per extension (on top of the earlier 100 lists, 100 subscriptions and 64 byte `conn_id`). | Bounds the work one request can ask for. Generous enough that real clients never meet them. |
| 65 | After each response the connection **forgets rooms that are no longer candidates at all** (for example purged rooms). A room the user left themselves stays while the connection has received it. | Keeps per-connection memory proportional to the rooms that exist. A mutation check confirms the test fails without it. |
| 66 | Requests waiting for changes end with **`503` when the server shuts down**, instead of holding shutdown up for as long as three minutes. | The handler takes the process context. |
| 67 | Prometheus gauges `dendrite_syncapi_sliding_sync_{active_requests,waiting_requests,connections}`, registered only when metrics are enabled. | Operators can see long polls and memory held. |
| 68 | The connection TTL (30 minutes) and the cap (10 per device) stay constants, not config options. | No evidence yet that anyone needs to change them; adding config later is cheap. |
| 69 | Documentation: README no longer lists MSC4186 as unsupported, CHANGES has an "Unreleased" entry, and `docs/other/sliding-sync.md` covers behaviour, limits, resource use and metrics. `well_known_sliding_sync_proxy` is left as it is. | That setting points clients at an external proxy and is not needed for built-in support. |

### Real end-to-end check

A real Dendrite (built from this tree, SQLite, local, throwaway) was driven with `curl` as two users, `alice` and `bob`. It confirmed what the tests fake:

- `/versions` lists the flag; the endpoint authenticates with a real access token (401 without one).
- First request returns both rooms with names, `initial`, member counts, a five event timeline, lazily loaded members and `$ME`, and all five extension keys.
- A follow-up with nothing new returns no rooms and still reports list counts.
- A long poll was woken by a message sent by the other user after 1.5 s and returned after 1.7 s with only the new event, `num_live: 1` and no `initial`.
- A long poll was woken by a real `PUT /sendToDevice` and delivered the message.
- With the to-device `since` sent, the request waited its full 2 s, and the acknowledged message was gone for a new session. (My first script did not send `since`, so the message was correctly delivered again; that looked like a bug and was not.)
- `M_UNKNOWN_POS`, `M_MISSING_TOKEN` and `M_INVALID_PARAM` for a bad `timeout`; the legacy `/sync` still works for the same user; the server log has no errors or panics.

**This is not a test with a real Element X client**, only with the protocol (open item K).

### Environment findings

- `clientapi/clientapi_test.go` needs the `libolm` C headers (`mautrix`), which are not in the nix shell, so `go vet ./clientapi/...` fails here. Only `./clientapi/routing/` is vetted. Adding `libolm` to `shell.nix` would fix it if wanted.
- Running a throwaway server needs `--really-enable-open-registration` with the CI-style config from `generate-config --ci`.

## Open items for review

- **A. Dialect longevity.** The unstable endpoint may move to the MSC's `/v4/sync` dialect once clients migrate. Adding it should only need new request/response types and a route.
- **B. `pos` versus stream positions.** Resolved in phase 4 (decision 41): `pos` stays opaque and per-room positions are kept in the connection.
- **C. Replay memory.** One retained response per connection (decision 8). Responses now only carry changes, so this is small; worth another look once extensions land.
- **D. `/versions` flag and `README.md`.** Done in phase 6 (decision 63).
- **E. Knocks.** Resolved in phase 3 (decisions 38 and 39). Remaining question: whether this server's syncapi receives knock events for rooms it is not in at all; only knocks present in its current state or carried with `unsigned.knock_room_state` are listed.
- **F. `pos` in `/messages` and `/relations`.** The MSC allows a sliding-sync `pos` there. Not needed for Element X (it uses `prev_batch`); skipped.
- **G. Left rooms.** Kicked and banned rooms are listed since phase 3 (decision 36). The MSC also lists self-left rooms that were already sent on the connection, so that clients learn the room is gone; that needs the per-connection state of phase 4.
- **H. Extension shapes.** Implemented in phase 5 as specified by Ruma and Synapse (decisions 53-62).
- **I. `set_presence` and the presence extension.** Not implemented (decision 52). Check whether Element X needs `set_presence` to show the user as online, and whether clients rely on the presence extension.
- **J. Extension scope.** Following Synapse, receipts and typing only reach rooms that are in the response (decision 56). If Element X turns out to miss live typing or read receipts in rooms that are otherwise quiet, widen the scope to every room in the lists and subscriptions and let an extension-only response end the long poll.
- **K. Not tried with a real Element X.** Everything was checked against the MSC, Synapse's code, Ruma's types and `curl`. A real client may send something these did not predict (extension options, filters, or a sequence of requests). This is the first thing to do before relying on it: log in with Element X, watch the server log, and compare its requests with `integration_*_test.go`.
- **L. Latent SQLite bug** in `SelectMaxEventID` (phase 2 findings), still unfixed.
- **M. Cost of busy long polls.** Every wake-up recomputes the candidate rooms (memberships, activity, invites, filter state) for each waiting connection. If large accounts show load, cache candidates per connection, keyed by the notifier position.
- **N. Not covered by Sytest or Complement.** Neither suite exercises this endpoint, so CI proves nothing about it beyond the unit and integration tests in this change.

## Remaining work

All six phases are done. What is left is in the open items above, chiefly K, L and M.
