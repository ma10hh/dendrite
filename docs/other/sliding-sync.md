---
nav_exclude: true
---

# Sliding sync (MSC4186)

Dendrite implements [MSC4186](https://github.com/matrix-org/matrix-spec-proposals/pull/4186), Simplified Sliding Sync,
which Element X uses for fast login and sync. It is built in: there is nothing to enable, and no separate proxy is needed.
The server advertises `org.matrix.simplified_msc3575` in `/_matrix/client/versions`.

It speaks the dialect that Element X and Synapse use today, at

```
POST /_matrix/client/unstable/org.matrix.simplified_msc3575/sync
```

with `pos` and `timeout` as query parameters. The dialect the MSC text now describes for `/v4/sync` is not implemented.

## What is supported

- Lists with ranges and the `is_dm`, `is_encrypted`, `is_invite`, `room_types`, `not_room_types`, `spaces`, `tags` and `not_tags` filters. Rooms are ordered by their latest activity, with invites first.
- Room subscriptions, `timeline_limit` (capped at 100) and `required_state` including `*`, `$ME` and `$LAZY`.
- Invites, knocks, and rooms the user was kicked or banned from.
- Incremental updates, long polling with `timeout` (capped at 3 minutes), and `bump_stamp`, `num_live`, heroes and unread counts.
- `set_presence` (a query parameter, like `pos`) marks the user as online, idle or offline the way `/sync` does. It only has an effect when outbound presence is enabled, see [Enabling presence](../administration/3_presence.md). Other values are refused with `M_INVALID_PARAM`.
- The `to_device`, `e2ee`, `account_data`, `receipts` and `typing` extensions.

Not supported: a presence extension (MSC4186 does not define one, and Element X does not use presence), `is_tombstoned` and `room_name_like`
filters, and `/messages` or `/relations` with a sliding sync `pos`.

## Resource use

Each connection keeps its state in memory. There is no database table for it and nothing is written to disk.

- A client that starts a session (a request without `pos`) replaces any earlier session with the same `conn_id`.
- A device can have up to 10 connections, and a connection is forgotten after 30 minutes without a request.
- After a restart, or when a connection was forgotten, clients receive `M_UNKNOWN_POS` and start again from scratch. This is
  expected: the MSC requires clients to handle it.
- A request that waits for changes holds an HTTP request open for up to three minutes. Reverse proxies need a read timeout
  longer than that.

Requests are limited to 100 lists, 100 room subscriptions and 100 ranges per list, and to a bounded number of `required_state` entries.

## Metrics

With metrics enabled, the following are exported:

| Metric | Meaning |
|---|---|
| `dendrite_syncapi_sliding_sync_active_requests` | Requests being handled, including ones waiting for changes |
| `dendrite_syncapi_sliding_sync_waiting_requests` | Requests waiting for something to return |
| `dendrite_syncapi_sliding_sync_connections` | Connections held in memory |

## Notes for clients

- Receipts and typing notifications are only sent for rooms that are in the same response. A quiet room's typing indicator
  therefore waits until the room has something else to report, as in Synapse.
- The `well_known_sliding_sync_proxy` setting is for pointing clients at an external sliding sync proxy. It is not needed for the built-in support.

## For developers

The design decisions, the sources they were checked against, known limitations and open questions are in the
[decision log](../development/sliding-sync-decisions.md). The code is in `syncapi/slidingsync`.

## Filters on tags and spaces

`tags` keeps the rooms the user tagged with any of the given tags, and `not_tags` drops the rooms that have any of them and wins over `tags`.
`spaces` keeps the rooms that are direct children of any of the given spaces. A child is an `m.space.child` event with a non-empty `via`. Spaces that the user has not joined
or that do not exist are ignored and subspaces are not followed, as the MSC says. Each filter takes up to 100 entries.

Tags are read from the account data of the user with every request that has a tag filter, so a very large amount of account data makes those requests slower.
The `spaces` filter reads the children of each space named in the request.

## `state_after` in `/sync`

`/sync` supports [MSC4222](https://github.com/matrix-org/matrix-spec-proposals/pull/4222). With `use_state_after=true` (or the unstable `org.matrix.msc4222.use_state_after`)
the response has `state_after` (or `org.matrix.msc4222.state_after`) for each joined, peeked and left room instead of `state`. It holds the state changes up to the end of the
timeline, so a state event of the timeline is in it too, unless that event did not take effect. The property is there even when it is empty. The server advertises `org.matrix.msc4222` in `/versions`.

## Measured cost

Measured with `LOAD_TEST=1 LOAD_ROOMS=2000 go test ./syncapi/slidingsync -run LoadLargeAccount -v` on an account with 2000 rooms of 9 events each,
with the database on the same machine. Your hardware will differ, the point is the shape.

| Request | SQLite | Postgres |
|---|---|---|
| Start a session, window of 20 rooms | 55 to 240 ms | 120 to 170 ms |
| Start a session, window of 100 rooms with 10 events each | 145 ms | 620 ms |
| Start a session, all 2001 rooms | 1.0 s | 10 s |
| Same with a `tags`, `not_tags` or `spaces` filter, window of 100 | about 100 ms | 570 ms |
| Follow-up request, nothing changed | 41 ms | 58 ms |
| Follow-up request, one new message | 44 ms | 84 ms |

The cost of starting a session grows with the number of rooms in the window, so clients should page through the list rather than ask for everything at once.
A follow-up request costs time proportional to the length of the account's history, because the order of the rooms is worked out from the latest event of each:
about 0.2 s per request for 200,000 events on SQLite. A very old account with millions of events will notice it.

Until 2026-11-19 SQLite was slower than this by a factor of 10 to 15 in sliding sync and in `/sync`, for rooms without recent events: its planner chose the wrong index
(see `syncapi/storage/sqlite3/output_room_events_table.go`).
