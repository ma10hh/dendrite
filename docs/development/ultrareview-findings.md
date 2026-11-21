# Findings of the ultrareviews, with the check of each

Each phase is reviewed on its own (`/code-review ultra <previous phase>` on the phase branch). Every finding is checked against the code before it is listed here. Status: **fix** (to be fixed on `main`), **fixed**, **wontfix** (with the reason), **nit**.

## Phase 0 (`phase-0-baseline` against `dffc05f7`)

| # | Finding | Checked | Status |
|---|---|---|---|
| 0.1 | `go.mod` needs Go 1.25, but the SQLite upgrade test (`cmd/dendrite-upgrade-tests/main.go:106`) builds in `golang:1.24-bookworm`, where `GOTOOLCHAIN=local` refuses the module: the two `--sqlite` jobs of the CI fail on every run | Yes: line 106 has 1.24, line 62 (Postgres) 1.25 | fix: bump to 1.25 |
| 0.2 | Decision log rows 22 to 28 are after the session report text, so they render as text, not table rows | Yes, known (the log is append-only) | nit, left |

## Phase 1 (`phase-1-mas` against `phase-0-baseline`)

| # | Finding | Checked | Status |
|---|---|---|---|
| 1.1 | `POST /delete_devices` from an appservice skips the user-interactive auth, and an empty or missing `devices` list reaches `PerformDeviceDeletion`, where an empty list means "all devices": an appservice can log a user out everywhere with `{}`. The spec reads an empty list as nothing to delete | Yes: `device.go` does UIA only for non-appservices and passes `payload.Devices` as it is; `user_api.go:330` deletes all for an empty list | fix: an empty list deletes nothing (200), for everybody |
| 1.2 | Strict config loading (`UnmarshalStrict`) refuses the Helm chart's own default values: `dendrite_config.global.profiling` is a key that no config struct has, so every Helm deployment crash-loops at startup after the upgrade | Yes: `helm/dendrite/values.yaml:243` has `profiling`, `setup/config` has no such key | fix: remove it from `dendrite_config` in the chart (it is read by the deployment template, so it needs a place of its own), and add a test that loads the rendered values strictly |
| 1.3 | An appservice request with the as_token and no `user_id` now returns the dummy device `AS_Device` instead of the real device the appservice registers for its sender (ID = sender_localpart): bridge bots with E2EE would change device | Yes: `QueryAccessToken` now always calls `queryAppServiceToken`, whose no-user branch keeps the dummy ID | fix: outside the delegated mode and without an asserted device, keep the old path (the registered device) |
| 1.4 | MAS `delete_user` deactivates the account but keeps its 3PIDs (`PerformAccountDeactivation` never touches them), so an email address of a deleted account can never be given to another user: `provision_user` then answers 500 for ever (`Err3PIDInUse`). Synapse removes them | Yes: no 3PID call in the deactivation; `syncEmails` turns the error into a 500 | fix: `delete_user` forgets the user's email 3PIDs |
| 1.5 | The `device_id` of the introspection response replaces the one from the scope for every provider, not only MAS: a generic provider that returns an unrelated `device_id` makes tokens without exactly one device scope valid, and sessions that share it share a Matrix device. The admin scope next to it is limited to MAS | Yes: `delegatedauth.go:326` has no `MASMode()` check | fix: only in MAS mode |
| 1.6 | `PerformAccountCreation` returns success with `AccountCreated=false` for any `CreateAccount` error, not just "exists", so `provision_user` can answer 200 for an account that was never created (MAS does not retry) | Yes: `user_api.go:218-237`; existing behaviour, new caller | fix: return other errors (callers: register, appservices, server notices, delegated auth, MAS) |
| 1.7 | In `dendrite-sample.yaml` the two `introspection_endpoint` comment lines are under `rendezvous:` instead of `msc3861:`; uncommenting them as laid out fails the strict loader | Yes | fix: move back |
| 1.8 | The three copies of "create a device with a placeholder token under a mutex" (`device.go`, `mas.go`, `userapi/internal/delegated_auth.go`) each have their own lock, so a MAS `upsert_device` and an appservice `PUT /devices` for the same device are not serialised | Yes | nit: one helper in userapi with one lock; do if time allows |
| 1.9 | The doc comment of `UpdateDeviceByID` sits above the new mutex variable | Yes | nit: fix with 1.8 |

## Phase 2 (`phase-2-sliding-sync` against `phase-1-mas`)

| # | Finding | Checked | Status |
|---|---|---|---|
| 2.1 | With `use_state_after` and `lazy_load_members` together, the lazy-loading pruning still runs on the state section (`stream_pdu.go:360-368`; only `removeDuplicates` was switched off for the new mode). Membership changes of users who are not senders of the timeline (a kick, a ban, an invite by somebody else) or of senders the client already has (a display name change) are dropped from `state_after`. A client that follows MSC4222 takes state from `state_after` only, so it keeps the old name and shows the kicked user as joined. In the default mode the timeline carries those events, so the client got them there. The complete sync path has the same problem | Yes: `lazyLoadMembers` keeps a member event only for senders that are new to the client's cache, for the user and for gapped syncs | fix: in state_after mode do not drop the membership events that are in the timeline of the response |
| 2.2 | `maxFilterEntries` (100 spaces) is per list, and up to 100 lists are allowed: 10,000 spaces in one request, deduplicated with `slices.Contains` in `needsFor` (O(n squared), tens of millions of comparisons, repeated at every wake of the long poll); string lengths are not bounded | Yes by reading `tooLarge` and `needsFor`; the review calls the cost bounded | fix: deduplicate with a map, and bound the total number of distinct spaces and tags per request |
| 2.3 | `JoinResponse.MarshalJSON` removes `unread_notifications` when only `state_after` has data (its emptiness check looks at `State`, not at `StateAfter`) | Yes by reading | fix |
| 2.4 | `CHANGES.md` entry for MSC4186 still says "Presence is not supported" next to the new `set_presence` support | Yes (`CHANGES.md:15`) | fix: "there is no presence extension" |
| 2.5 | `roomTags` and `spaceChildren` run in `loadCandidates` at every wake of the long poll, not once per request as `docs/other/sliding-sync.md` and decision 51 say: all account data of the user and one state query per joined space each time | Yes: `fill` runs at each wake | fix: cache them on the connection and reload when the account data or the PDU stream position moved; if that is too much, correct the docs |
| 2.6 | The SQLite plan test checks a hand-copied query, not the one `SelectRoomActivity` builds | Yes | nit: build the query in one helper |
| 2.7 | `SetPresence` was inserted between the comment of `updatePresence` and that function | Yes | nit: move the comment |
| 2.8 | The load test writes the last `rooms/5` space events with `space.Events()[len-rooms/5:]`, but children are made for `i%5==0`, which is `ceil(rooms/5)`: with a room count that 5 does not divide, one child is not written (found by the local review) | Plausible by reading, timing test only | nit: fix the slice |
| 2.9 | The stable and unstable `state_after` are two more pointer fields on `JoinResponse` and `LeaveResponse`, so every reader of the state must check three fields (`keychange.go` and `stream_presence.go` had to be patched) and `StateEvents()` copies on every call (local review) | Yes | nit: one field, key name chosen at marshal time; do if time allows |
| 2.10 | `spaceChildren` runs one `CurrentState` query per named space, one after the other, in the request's transaction, at every wake of the long poll (local review; the same as 2.5) | Yes | fix with 2.5 (one query for all spaces, cached) |

Note on the tools: the local `/code-review` (no billing, no size limit) found 2.3 and 2.7 again and three more nits, but **missed 2.1**, the one real bug of this phase, and said the `state_after` code was fine. The ultrareview is the stronger one; the local one is a second pair of eyes for the small things.

## Phase 3 (`phase-3-api` against `phase-2-sliding-sync`, local `/code-review high`)

| # | Finding | Checked | Status |
|---|---|---|---|
| 3.1 | `withdrawRemoteKnock` emits `OutputRetireKnockEvent` without `Membership: leave`, so the sync API's consumer (`knocks.go:60`) takes the branch for "an invite answered it" (purges the room's state) and writes no leave event: v3 `/sync` clients are never told that the knock ended | Yes: the struct is built without `Membership`, the consumer tests `!= spec.Leave` | fix: set `Membership: spec.Leave`, with a test of the consumer path |
| 3.2 | `withdrawRemoteKnock` (a) rolls the transaction back before the federation call and then deletes the membership without checking `IsKnock()` again: an invite that arrived in between is deleted with it; (b) an error of `MembershipUpdater` or `Rollback` returns `handled=false`, which the caller ignores, so the normal leave path runs on top | Yes by reading (`perform_leave.go:284-320`, the caller at line 132 returns only if `handled`) | fix: re-check `IsKnock()` on the second updater; return the error whatever `handled` is |
| 3.3 | `/publicRooms` over federation with a search term or room types slices a list whose order is random (`PopulatePublicRooms` ranges over a map): the pages of `next_batch` come from different orders, so a client sees duplicates and misses rooms | Yes: `for roomID, data := range stateRes.Rooms` | fix: sort by room ID before filtering and slicing (and make `PopulatePublicRooms` keep the order of its input) |
| 3.4 | `remoteRoomSummary` returns the answer of the federation hierarchy without `canPreview`, which local rooms get: the hierarchy check is per server, not per user, so a user that is not in an allowed room can read the summary of a restricted or not world-readable remote room that the server may see | Yes: the only `canPreview` call is at line 259, for local rooms | fix: apply it with the join rule, `world_readable` and `allowed_room_ids` of the remote answer |
| 3.5 | `/threads` reports `count` unchanged, but `BundleRelations` leaves out replies of ignored users: `count` differs from `/event`, `/messages` and `/context` for the same thread | Yes: `withThreadSummary(..., e.Count, ...)` | fix: count without ignored users |
| 3.6 | Room summary resolves an alias with `IncludeAppservices: true` also for anonymous callers, so an unauthenticated client makes the server ask the application services over HTTP for unknown aliases | Yes: line 134, the federation lookup is gated on `device != nil`, this is not | fix: only with a device |
| 3.7 | Every federation `/publicRooms` request with a filter reads the state of all published rooms, with no cache; the client API caches | Yes by reading `publicrooms.go:80-90` | fix: the same cache, or bound the work; otherwise document |
| 3.8 | SQLite `LIKE` is case-insensitive for ASCII: the wildcard types (`m.room.*`) of filters also match `M.ROOM.x`, which Postgres does not | Yes: `typeClause` builds a `LIKE` | fix: `GLOB` (case-sensitive), escaping `?` and `[` |
| 3.9 | `/context` loads the whole state at the event and drops the members afterwards when `lazy_load_members` is set (`QueryStateAfterEvents` with no tuples); `BundleRelations` runs three times | Yes by reading | fix if time allows: ask for the tuples |
| 3.10 | Commit `9424f2e3` bundles two unrelated changes (the `/context` state and the SQLite wildcard filters), against the rule of the owner to split commits | Yes by the message | wontfix: the history is merged; rewriting it is not worth it |

## Phase 4 (`phase-4-partial-state` against `phase-3-api`, local `/code-review high`)

The feature is experimental and off by default (`federation_api.partial_state_joins`); only 4.9 touches a server that has it off.

| # | Finding | Checked | Status |
|---|---|---|---|
| 4.1 | The stand-in joined hosts have the event ID `partial-state:<server>` in every room, and the table has a unique index on `event_id` alone (and `DeleteJoinedHosts` deletes by it): partially joining a second room through the same server inserts nothing (`ON CONFLICT DO NOTHING`), and completing one room deletes the stand-in of the others | Yes: `federatonsender_joined_hosts_event_id_idx`, the ID built in `WithPartialStateHosts` and `processMembersAdded` | fix: put the room ID in the ID |
| 4.2 | `fetchMembers` takes whatever `/state` answers as the whole membership: members whose signature fails are skipped, an empty answer from a server that is itself partial ends in `ClearRoomPartialState`, so the room is "whole" with most members missing, for ever | Yes: no check of the count or of the servers | fix: fail (and try the next server) when there is no joined member, or none of the answering server |
| 4.3 | `/state` and `/state_ids` over federation do not refuse a room with partial state (only make_join, send_join, make_knock, send_knock do): another server, also a partial joiner, takes our incomplete state as the full one (this feeds 4.2) | Yes: no partial check in `federationapi/routing/state.go` | fix: refuse them like make_join |
| 4.4 | `userLeft` (a) reads the membership before the join's membership row exists (`MarkRoomPartialState` commits first), so one resync tick in that window drops the record and the room stays partial for ever; (b) only looks at the user who joined: with a second local user in the room the record is dropped when the first one leaves | Yes by reading `userLeft` and the order of the commits | fix: drop the record only when no user of this server is joined (`QueryServerJoinedToRoom`), and treat a missing row as not left |
| 4.5 | While a room is partial, every soft-fail (also a real one: an event of a banned user) is accepted provisionally, and the recheck after the members came tests it against its state before the event, not the current state; the decision log already says that the soft-fail check is not repeated | Yes (decision 77 area) | known; fix if time: repeat the soft-fail check against the whole current state in the recheck |
| 4.6 | A provisional event that fails the recheck is only flagged rejected: it stays in the current state, and `snapshotWithMembers` then does not add the real membership of that user (a banned user who rejoined with an old leave as auth stays joined in our state) | Yes; this is the remaining blocker of entry 77 | known, not fixed: needs a rewrite of the current state; the flag stays off |
| 4.7 | All the members go into one `OutputMembersAdded` message, and the consumers handle them in one transaction: for more than about 20,000 members the message is over the 16 MB `MaxPayload` of NATS (`setup/jetstream/nats.go:58`), `ProduceRoomEvents` fails and the resync retries for ever: the biggest rooms never become whole | Yes: the payload limit is 16 MB | known blocker of turning the flag on: chunk the message (with a "last chunk" mark); add to the open list |
| 4.8 | The partial record is cleared by a separate call after the `KindAddMembers` commit, not in the room's input worker: events in between are accepted provisionally and never rechecked (the notes are already used); a crash in between repeats the whole fetch | Yes | known; low |
| 4.9 | **With the flag off** the resync goroutine runs every 2 seconds for the life of the process (`roomserver/internal/api.go:225`, not tied to the process lifetime), and `PartialStateOfRoom` is read at every `processRoomEvent`, `QueryMembershipForUser`, `QueryServerJoinedToRoom` and state rewrite: one more query each, although no room can be partial | Yes: the goroutine starts unconditionally; `PartialStateJoins` is only read in `federationapi/internal/perform.go:196` | fix: start the goroutine only when the flag is on or a record exists at startup; keep a flag in memory "partial rooms exist" so that the reads are skipped without them |
| 4.10 | `recheckPartialStateEvents` adds the join-time memberships to the state before every recorded event, also backfilled events that are older than the join: a user who joined later then counts as present at that event, so history visibility checks (`joined`) can show it to that user's server | Yes by reading; plausible | fix: leave events older than the join event alone |
| 4.11 | (not in the list) the stand-in hosts come from the untrusted `servers_in_room` of the resident server: any names, any number: this server would federate the room's events to them until the resync completes | Yes by reading `WithPartialStateHosts` | fix: validate the names and cap the number (for example 100) |

## Phase 5 (`phase-5-admin` against `phase-4-partial-state`, local `/code-review high`)

The review confirmed that `MakeAdminAPI` is on every new route, that the SQL built from request parameters uses fixed lists or bound arguments, and that the SQLite parameter order is right in the new statements.

| # | Finding | Checked | Status |
|---|---|---|---|
| 5.1 | The room list loads the state of every room with every membership event on every request (known, decision 80); **on SQLite more than about 32,766 member events in the server make `BulkSelectEventJSON` fail (one variable per event), so the list answers 500**; on Postgres it reads all member JSON into memory | Yes by reading `adminRooms` (one `QueryBulkStateContent` for all room IDs) | fix: ask in chunks of 200 rooms |
| 5.2 | A shared file is removed after the "in use" check has ended: an upload of the same bytes in between finds the file, only inserts a row, and the row then points at a deleted directory (404 on every download); Postgres has no writer lock | Yes by reading `removeMedia` and `doUpload` | fix: one mutex around the check and the delete, and around lookup and insert of an upload |
| 5.3 | `user_id` and `base64hash` have no index: the quota sum (twice per upload), the count of `DeleteMedia` (per deleted media, 500 per retention batch), the list and the quarantine of a user are all table scans | Yes: only `(media_id, media_origin)` is indexed | fix: add `IF NOT EXISTS` indexes on `user_id` and `(base64hash, media_origin)` to the schema |
| 5.4 | An upload of content that is already stored keeps the announced `Content-Length` as `file_size_bytes` (-1 when chunked, or a lie), so the quota is undercounted | Yes: the branch uses `r.MediaMetadata.FileSizeBytes` | fix: use the bytes received |
| 5.5 | `PUT /v2/users/{id}` creates accounts with `ValidateUsername` only: not the numeric names that are reserved for guests, not the exclusive namespaces of application services | Yes: only `ValidateUsername` is called | fix: the same checks as `/register` |
| 5.6 | An admin can demote or deactivate their own account (`admin=false`, `deactivated=true`); Synapse refuses self-demotion | Yes by reading | fix: 400 for both |
| 5.7 | `from+limit` overflows for a huge `limit` in the room list and the slice panics (500) | Yes: `min(from+limit, total)` | fix: clamp |
| 5.8 | `GET /rooms/{id}/members` answers 200 with an empty list for a room that does not exist | Yes | fix: 404 |
| 5.9 | `listUser` of the media admin answers every error as 400 with the raw error text | Yes | fix: validate `order_by` first, 500 for the rest |
| 5.10 | Commit `b930c55e` bundles two unrelated fixes | Yes | wontfix (history) |

## Phase 6 (`phase-6-hardening` against `phase-5-admin`, local `/code-review high`)

The review found no problem in the SQLite busy timeout change, and no correctness bug in the heroes change.

| # | Finding | Checked | Status |
|---|---|---|---|
| 6.1 | **The fix for the room ID crash is incomplete.** `fetchMembers` (`perform_partial_state.go:165`) parses events from the `/state` of another server and calls `ev.RoomID()` and `VerifyEventSignatures` without `CheckUntrustedEvent`, in the resync goroutine, which has no recover: a server in a partial-state room could crash the process, and again after every restart (the record persists) | Yes: grep shows no check there | fix: check, and a recover in `Run`/`ResyncAll` |
| 6.2 | The workers that fetch missing events (`go worker(pending)`, `input_missing.go:848`) run in goroutines of their own: the recover of `processRoomEventRecovering` does not cover them | Yes | fix: a deferred recover in the worker |
| 6.3 | Events of `/get_missing_events` (`input_missing.go:607`) and of the state answers go to `VerifyEventSignatures` without `CheckUntrustedEvent`; only `/event` and the transaction path were guarded; the wrapper turns the panic into an error for the legitimate event being processed, which is dropped | Yes: only line 955 has the check | fix: filter those lists too, and the places in `federationapi/internal/perform.go` (invite answers, `missingAuth`) |
| 6.4 | `FuzzUntrustedEvent` never calls `RoomID()` or `VerifyEventSignatures`, so it would not catch a gap in the check | Yes | fix: add both with a stub verifier |
| 6.5 | `FuzzParsePos` compares `seq != seq` (never true, staticcheck SA4000) | Yes | fix: `seq >= 0` is not an invariant of a `uint64`; assert the round trip instead |
| 6.6 | `run.sh` does `git checkout --force` and `reset --hard` in `COMPLEMENT_DIR`, which a user can point at their own clone; the container writes files as root | Yes | fix: refuse a directory that is not the script's own clone or is dirty |
| 6.7 | The doc comment of `omitMembers` now sits above `maxHeroes` | Yes | fix: move |

## What was done (2026-11-22, on `main`)

Every finding was checked against the code first (the column "Checked" above); all of them were real. Fixed, each with a test unless said, in these commits (see `git log` from "Check events from other servers wherever they are read"):

- **Crash safety:** 6.1 to 6.3 (a check of every place that reads events from other servers, a recover in the resync and in the missing-event workers; the test of the resync gets a malformed event, and fails without the check), 6.4 to 6.7.
- **Config, CI, auth:** 0.1, 1.1 to 1.7, 1.9; 1.2 also found that the Helm `metrics.basic_auth.user` was a misnamed key (never worked): both are in the upgrade notes.
- **Sync and API:** 2.1 (the test fails without the fix), 2.2, 2.3, 2.4, 2.5 (docs corrected, the cost is bounded by 2.2; no cache), 2.6 to 2.8, 3.1 to 3.9.
- **Partial state:** 4.1 to 4.4, 4.9 to 4.11. 4.10 has no test of its own.
- **Media and admin:** 5.1 to 5.9.

Not fixed, with the reason:

- **1.8** is done for the two copies in `clientapi` (one lock); the third (`createDelegatedDevice` in `userapi`) is on the other side of the API and keeps its own lock.
- **2.9** (the three `state_after` fields): a refactor of types that are encoded in the responses and in tests; the current code works, so it stays. 2.10 is covered by 2.2 and 2.5 (bounded, not cached).
- **4.5 to 4.8** are the known blockers of turning `federation_api.partial_state_joins` on, now said in `dendrite-sample.yaml`: genuine soft-fails are accepted provisionally while a room is partial, a provisional event that fails the recheck stays in the current state, more than about 20,000 members do not fit one NATS message, and the record is cleared in a separate step.
- **0.2, 3.10, 5.10** are about the decision log table and the history of commits; not touched.
