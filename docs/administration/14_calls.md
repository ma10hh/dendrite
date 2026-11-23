---
title: Calls (delayed events and MatrixRTC)
parent: Administration
nav_order: 14
permalink: /administration/calls
---

# Calls: delayed events and MatrixRTC

Element Call needs three things from a homeserver: **delayed events** ([MSC4140](https://github.com/matrix-org/matrix-spec-proposals/pull/4140)), so that the membership of a call ends by itself when a client disconnects; the address of a **media server** ([MSC4143](https://github.com/matrix-org/matrix-spec-proposals/pull/4143)); and the media server itself. Dendrite provides the first two. It does **not run a media server**: install [LiveKit](https://livekit.io/) and `lk-jwt-service` yourself and point Dendrite at the latter.

Both are off by default.

```yaml
client_api:
  delayed_events:
    enabled: true
    max_delay: 24h        # the longest delay a user may ask for
    max_per_user: 100     # how many delayed events one user may have waiting
  matrix_rtc:
    transports:
      - type: livekit
        livekit_service_url: https://livekit-jwt.example.com
```

## Delayed events

A client adds `org.matrix.msc4140.delay=<milliseconds>` to a send (`PUT /rooms/{roomId}/send/{type}/{txnId}`) or a state call (`PUT /rooms/{roomId}/state/{type}/{stateKey}`). Instead of sending, the server keeps the event and answers `{"delay_id": "..."}`. Then:

- the event is **sent as the user when the delay is over**, with the checks of any other send at that time (membership, power levels);
- `POST /_matrix/client/unstable/org.matrix.msc4140/delayed_events/{delay_id}` with `{"action": "restart"}` starts the delay again, `"cancel"` drops the event, `"send"` sends it now. The form with the action in the path (`.../{delay_id}/restart`) works too;
- `GET .../delayed_events` lists the delayed events of the user, the next to be sent first;
- `/versions` advertises `org.matrix.msc4140`. The paths under `/_matrix/client/v1/` work as well.

Details and limits:

- **A client that sends a delay while the feature is off is refused** (400, `M_UNRECOGNIZED`): sending the event at once would announce a call member that should have timed out.
- A delay above `max_delay` is refused with `M_MAX_DELAY_EXCEEDED` (`org.matrix.msc4140.errcode`), and the error says the limit in `max_delay`. More than `max_per_user` waiting events gives `M_LIMIT_EXCEEDED`.
- Only members of the room can schedule an event. Other users' delay IDs are answered as unknown (404).
- The events are stored in the database (`userapi_delayed_events`) and survive a restart. **Events that were due while the server was down are sent when it starts**, spread over a few seconds.
- When a state event is sent, **delayed state events for the same room, type and state key are cancelled**, whoever scheduled them, since they would overwrite the newer state. This only sees state events sent through the client API (including delayed ones that fire); membership changes and state from other servers do not cancel anything.
- A shadow banned user is told that the event was scheduled, and nothing is kept.

### When an event is dropped or sent again

- **Dropped at once** when the user can no longer have meant it: the user leaves, is kicked or banned from the room, the device that scheduled it logs out (or is deleted), the account is deactivated, or the room is purged. A newer state event for the same key also drops it (above). What is left in memory is only a timer that finds nothing when it runs out.
- **A failure of the server is retried.** If sending fails because of the server (a 5xx answer, for example the roomserver is busy), the event is kept and tried again after 1, 2, 4 and 8 seconds, five attempts in all; then it is dropped and counted as failed. A refusal (the user may not send it any more, the content is invalid) drops it at once. A call member that is never removed is worse than one that is removed a few seconds late, which is why the event is not dropped on the first failure.
- **A crash between the send and the removal of the row sends the event again** at the next start. For state events (the membership of a call) this changes nothing; a delayed *message* could appear twice. Before this change the event was removed first, which lost it instead.
- A `restart` or `send` of an event that has just been sent answers 404.

### Metrics and the administrator's view

- `dendrite_clientapi_delayed_events_total{result}` counts `scheduled`, `sent`, `cancelled`, `retried` and `failed`; `dendrite_clientapi_delayed_events_waiting` is how many have a timer.
- `GET /_synapse/admin/v1/delayed_events[?user_id=]` lists the waiting events of all users or one, and `DELETE /_synapse/admin/v1/delayed_events/{delay_id}` drops one.

### Limits that matter for calls

- The calls of the client API that keep a call alive (`delayed_events`, `rtc/transports`) have **their own rate limit bucket** per device, so a restart every few seconds cannot use up the slots of normal requests and the other way round. The size of the bucket is the one of `client_api.rate_limiting`.
- `client_api.max_to_device_messages` (default 10000) bounds the number of (user, device) targets of one `sendToDevice` request, which a call uses to hand out its keys; more answers 413 `M_TOO_LARGE`. A `*` device counts once. 0 is no limit.
- The event content is checked when it is sent, not when it is scheduled: a delayed event that is invalid by then is dropped and logged at warning level.

## MatrixRTC

`client_api.matrix_rtc.transports` is announced in two places:

- `/.well-known/matrix/client`, as `org.matrix.msc4143.rtc_foci` (needs `global.well_known_client_name`);
- `GET /_matrix/client/unstable/org.matrix.msc4143/rtc/transports` (and under `/v1/`), for signed-in users, as `{"rtc_transports": [...]}`.

Only `type: livekit` is known; each needs the URL of `lk-jwt-service`. The configuration is checked at start.

## What is not done

- No media server, and no `lk-jwt-service`.
- The `org.matrix.msc3401.call.member` events themselves are ordinary state events: nothing on the server looks at them.
- Sticky events (MSC4354) and the other call proposals are not supported.
- **No push rules for call notifications** (MSC4075: `m.rtc.notification`, `m.call.notify`): they need the push rule condition `event_property_is`, which Dendrite does not evaluate, and I could not check the rule definitions that clients expect. Ringing for Element Call therefore relies on the client (and on `.m.rule.call` for the older `m.call.invite`).
