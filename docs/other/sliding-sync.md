---
nav_exclude: true
---

# Sliding sync (MSC4186)

Dendrite implements [MSC4186](https://github.com/matrix-org/matrix-spec-proposals/pull/4186), Simplified Sliding Sync,
which Element X uses for fast login and sync. It is built in: there is nothing to enable, and no separate proxy is needed.
The server advertises `org.matrix.simplified_msc3575` in `/_matrix/client/versions`.

The implementation is experimental. It speaks the dialect that Element X and Synapse use today, at

```
POST /_matrix/client/unstable/org.matrix.simplified_msc3575/sync
```

with `pos` and `timeout` as query parameters. The dialect the MSC text now describes for `/v4/sync` is not implemented.

## What is supported

- Lists with ranges and the `is_dm`, `is_encrypted`, `is_invite`, `room_types` and `not_room_types` filters. Rooms are ordered by their latest activity, with invites first.
- Room subscriptions, `timeline_limit` (capped at 100) and `required_state` including `*`, `$ME` and `$LAZY`.
- Invites, knocks, and rooms the user was kicked or banned from.
- Incremental updates, long polling with `timeout` (capped at 3 minutes), and `bump_stamp`, `num_live`, heroes and unread counts.
- The `to_device`, `e2ee`, `account_data`, `receipts` and `typing` extensions.

Not supported: presence (`set_presence` is ignored and there is no presence extension), the `spaces`, `tags` and `not_tags`
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
