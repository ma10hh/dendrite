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
- The events are stored in the database (`userapi_delayed_events`) and survive a restart. **Events that were due while the server was down are sent when it starts.** An event is taken out of the table before it is sent, so a crash at that moment loses it instead of sending it twice.
- When a state event is sent, **delayed state events for the same room, type and state key are cancelled**, whoever scheduled them, since they would overwrite the newer state. This only sees state events sent through the client API (including delayed ones that fire); membership changes and state from other servers do not cancel anything.
- A shadow banned user is told that the event was scheduled, and nothing is kept.
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
