---
title: Synapse admin API
parent: Administration
nav_order: 8
permalink: /administration/synapse-adminapi
---

# Synapse admin API

Dendrite serves the part of the [Synapse admin API](https://element-hq.github.io/synapse/latest/usage/administration/admin_api/)
that tools such as synapse-admin and Draupnir use, under `/_synapse/admin`. The caller must be an admin user (see
[Supported admin APIs](adminapi)). The answers follow Synapse's; what differs is listed here.

| Endpoint | Notes |
|---|---|
| `GET /v2/users`, `GET /v2/users/{userId}` | `name`, `user_id`, `guests` (default true), `deactivated` (default false), `order_by` (`name`, `creation_ts`, `displayname`, `admin`, `guest`, `deactivated`), `dir`, `from`, `limit`. The account the server sends server notices from is listed. `locked`, `erased` and `shadow_banned` are always false or 0, `external_ids` is empty |
| `PUT /v2/users/{userId}` | Creates or changes a local user: `password`, `displayname`, `avatar_url`, `admin`, `deactivated`, `logout_devices`. 201 when it creates. Guests and appservice users cannot be changed. 3PIDs in the body are ignored |
| `POST /v1/deactivate/{userId}`, `POST /v1/reset_password/{userId}` | `erase` is ignored |
| `GET /v2/users/{userId}/devices`, `DELETE /v2/users/{userId}/devices/{deviceId}`, `POST /v2/users/{userId}/delete_devices` | |
| `GET /v1/users/{userId}/joined_rooms` | |
| `GET /v1/rooms`, `GET /v1/rooms/{roomId}`, `.../members`, `.../state` | `order_by` is everything of Synapse except `state_events`. The list looks at every room of the server to sort and search, so it is slow on a server with many rooms. `state_events` is only in the single room |
| `DELETE /v1/rooms/{roomId}`, `DELETE /v2/rooms/{roomId}`, `GET /v2/rooms/{roomId}/delete_status` | Local users leave the room, and the room is purged unless `purge` is false. With `block` the room is blocked too (see below), also if it is not known here. **Not supported:** `message`, `new_room_user_id`, `room_name` (answered with 400). The deletion is done when the call returns; `delete_status` keeps the result for a day, in memory |
| `PUT /v1/rooms/{roomId}/block`, `GET /v1/rooms/{roomId}/block` | `{"block": true}` blocks the room, `false` lifts it. A blocked room cannot be joined or knocked on by local users (403 `M_FORBIDDEN`), and invites to local users over federation are refused. The room need not exist here. Users who are in the room stay in it: delete the room to remove them. `GET` does not say which admin blocked the room, as Synapse's does. The block survives a purge |
| `POST /v1/media/quarantine/{server}/{mediaId}`, `.../unquarantine/...`, `POST /v1/user/{userId}/media/quarantine` | Quarantined media is answered as not found. Only the media ID is quarantined: the same file uploaded again is a new media ID. Quarantining the media of a room is not supported |
| `DELETE /v1/media/{server}/{mediaId}`, `POST /v1/media/{server}/delete?before_ts=&size_gt=`, `GET` and `DELETE /v1/users/{userId}/media` | Local media only. The file and its thumbnails go with the last media that uses them. `before_ts` is compared with the time of the last download, or of the upload if there was none (the same as Synapse's `last_access_ts`). Downloads are recorded at most once an hour per media |
| `GET/POST/PUT/DELETE /v1/registration_tokens`, `/new`, `/{token}` | The same as `/_dendrite/admin/registrationTokens` |
| `GET /v1/server_version` | |
| `GET/POST /v1/event_reports`, `POST /v1/send_server_notice`, `/v1/register` | Existing, see their pages |

## With delegated authentication (MSC3861)

When authentication is delegated to Matrix Authentication Service, passwords and deactivation belong to it:
`reset_password` and `deactivate` answer 404 `M_UNRECOGNIZED`, and `PUT /v2/users/{userId}` refuses `password` and
`deactivated`. Creating users and changing the admin flag still work.

## Not provided

Shadow banning, user statistics, room statistics, retention policies, account validity, rate limit overrides and the
federation destination endpoints. Tools that depend on them show an error for that page only.
