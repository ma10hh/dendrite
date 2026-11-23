---
title: Message retention
parent: Administration
nav_order: 13
permalink: /administration/message-retention
---

# Message retention

Dendrite can delete messages that are older than the retention policy of their room, as Synapse does ([MSC1763](https://github.com/matrix-org/matrix-spec-proposals/pull/1763)). It is **off by default**: deleted messages cannot be restored, other than from a backup.

```yaml
sync_api:
  retention:
    enabled: true
    default_policy:            # for rooms without an m.room.retention event; 0 means no limit
      min_lifetime: 0s
      max_lifetime: 4320h      # 180 days
    allowed_lifetime_min: 24h  # shorter lifetimes in room policies are raised to this; 0 allows any
    allowed_lifetime_max: 8760h
    interval: 1h               # time between two runs
```

Durations are in hours (`720h` is 30 days), not days.

## Policies of rooms

A room sets its policy with the state event `m.room.retention` (state key `""`) and the lifetimes in **milliseconds**:

```json
{ "max_lifetime": 604800000 }
```

- `max_lifetime`: messages older than this are deleted. Without it, nothing of the room is deleted, whatever `default_policy` says: an `m.room.retention` event replaces the default as a whole.
- `min_lifetime`: what the room promises to keep at least. The server never deletes before `max_lifetime`, so it only matters to clients and other servers. It must not be above `max_lifetime`.
- Both must be non-negative integers. Dendrite refuses an event that sends something else (400, `M_BAD_JSON`), and refuses `min_lifetime` above `max_lifetime`. The usual power level for state events applies to who may send it.
- `allowed_lifetime_min` and `allowed_lifetime_max` move the lifetimes of room policies into that range for this server; the event itself is not changed.

## What is deleted

- **Messages and other events that are not state events**, when they were sent (their `origin_server_ts`) before now minus `max_lifetime`. They disappear from `/sync`, sliding sync, `/messages`, `/context`, `/relations` and search.
- **State events are never deleted.** They define the room; so do the membership events. Old topics, names and joins stay.
- In the roomserver, the event stays but **loses its content**, as after a redaction: the event ID, type, sender, time and the links to other events remain, so that the room's history stays consistent and other servers can still follow it. The data in the roomserver is what other servers get over federation.
- The run asks the roomserver first and deletes from the sync API afterwards, so a crash in between leaves the messages to be found again by the next run.
- Deleted messages are removed from the search index.

## What is not deleted

- **Media.** Files attached to deleted messages stay; use the [media retention](9_media.md) for them.
- **Copies on other servers and clients.** Retention is a promise of this server only. Other servers may keep the events, and may send old events again when somebody backfills (Dendrite deletes them again in the next run).
- **Events from before the policy**, if the server does not know their time: events stored before this feature are given their time by a migration at the first start, and that migration reads every stored event once. On a database with millions of events the first start takes a little longer.
- **Read receipts, notification counts and the event IDs** that point at the deleted events.

## Differences from Synapse

- No separate `purge_jobs` per room size and no `/_synapse/admin/v1/purge_history`.
- Messages that are over their `max_lifetime` stay visible until the next run (`interval`); Synapse hides them at once.
- Rooms of other servers are treated alike: this server deletes its copy by its own limits.

## Metrics and logs

`dendrite_syncapi_retention_deleted_total` counts the deleted messages. Each run that deleted something logs "Deleted messages that were older than the retention policy of their room" at info level, and a room that failed is logged at warning level and tried again in the next run.
