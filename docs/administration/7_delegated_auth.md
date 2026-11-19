---
title: Delegated authentication
parent: Administration
nav_order: 7
permalink: /administration/delegated-auth
---

# Delegated authentication (MSC3861)

Dendrite can hand authentication over to an external OAuth 2.0 / OpenID Connect provider, as proposed by
[MSC3861](https://github.com/matrix-org/matrix-spec-proposals/pull/3861). The provider owns logins, registration,
sessions and account management. Dendrite becomes a resource server: it only checks the access tokens the provider issued.
This is experimental.

```yaml
client_api:
  msc3861:
    enabled: true
    issuer: https://auth.example.com/
    client_id: dendrite
    client_secret: "..."
    # client_auth_method: client_secret_basic   # or client_secret_post
    # introspection_endpoint: https://auth.example.com/oauth2/introspect
```

`client_id` and `client_secret` are the credentials Dendrite uses to call the provider's token introspection
endpoint ([RFC 7662](https://datatracker.ietf.org/doc/html/rfc7662)), so the provider must have a client registered for it.
The endpoint is read from the provider's metadata (`/.well-known/openid-configuration` or
`/.well-known/oauth-authorization-server`) unless `introspection_endpoint` is set.

## Matrix Authentication Service

For [Matrix Authentication Service](https://github.com/element-hq/matrix-authentication-service) (MAS) set a `shared_secret` instead of a client ID and secret.
The same value goes into the `matrix.secret` option of MAS, and Dendrite is then reached through the `synapse` homeserver kind.

The configuration that `generate-config` writes already has an `msc3861:` block with `enabled: false`: edit that block.
A second `msc3861:` key is refused when Dendrite starts.

```yaml
client_api:
  msc3861:
    enabled: true
    issuer: https://auth.example.com/
    shared_secret: "..."
```

The matching part of the MAS configuration (the `secret` is the same value, `homeserver` is the server name of Dendrite and `endpoint` is where MAS reaches it):

```yaml
matrix:
  kind: synapse
  homeserver: example.com
  secret: "..."
  endpoint: http://dendrite:8008/
```

This was tested with MAS 1.29.0. `contrib/mas-lab` runs the setup in Docker and checks it, see its README.

In this mode MAS manages the accounts and devices of the server:

- Dendrite introspects tokens with the shared secret as bearer token and reads the explicit `device_id` that MAS sends for compatibility sessions.
- Nothing is created on first use. A token whose user or device MAS has not provisioned is refused, and deleting a device (logout) ends the session at once.
- A session with the `urn:synapse:admin:*` scope is an admin, so the admin API works with MAS-issued tokens. The scope is ignored in client ID and secret mode.
- The endpoints MAS uses to manage the server are served under `/_synapse/mas/`, and only for requests that present the shared secret:
  `query_user`, `provision_user`, `is_localpart_available`, `delete_user`, `reactivate_user`, `set_displayname`, `unset_displayname`,
  `upsert_device`, `delete_device`, `update_device_display_name`, `sync_devices` and `allow_cross_signing_reset`.
- `delete_user` deactivates the account but cannot erase its data, and `locked` is ignored because Dendrite has no locked accounts.
- `allow_cross_signing_reset` lets the user replace their cross-signing keys without a password, once, within 10 minutes. The approval is kept in memory and is lost on restart.

## What Dendrite does

- Every access token is introspected. Active tokens are cached for up to a minute (less if the token expires sooner),
  so a revoked token can keep working for that long. Inactive tokens are cached for 10 seconds, and concurrent lookups of
  the same token share one request to the provider. Application service tokens are unaffected.
- A token must carry the scope `urn:matrix:client:api:*` and exactly one `urn:matrix:client:device:<device ID>`
  scope ([MSC2967](https://github.com/matrix-org/matrix-spec-proposals/pull/2967)). The `urn:matrix:org.matrix.msc2967.client:` prefix is accepted too.
  Tokens without them get `403 M_FORBIDDEN`. Inactive tokens get `401 M_UNKNOWN_TOKEN`.
  If the provider cannot be reached the request fails with a `5xx`, so clients do not log out.
- The Matrix user is taken from the `username` field of the introspection response, which must be a localpart or a
  user ID on this server. The account and the device are created on first use.
  Existing accounts are matched by localpart, so an existing admin account stays an admin.
- `GET /_matrix/client/v1/auth_metadata` ([MSC2965](https://github.com/matrix-org/matrix-spec-proposals/pull/2965))
  returns the provider's metadata. `/_matrix/client/unstable/org.matrix.msc2965/auth_metadata` and `.../auth_issuer`
  are served as well, and `/.well-known/matrix/client` gains `org.matrix.msc2965.authentication`.
  Without this feature the endpoints answer `404 M_UNRECOGNIZED`.
- Appservices keep working ([MSC4190](https://github.com/matrix-org/matrix-spec-proposals/pull/4190), [MSC4326](https://github.com/matrix-org/matrix-spec-proposals/pull/4326)):
  they register users with `POST /register` (type `m.login.application_service` and `"inhibit_login": true`), create devices with `PUT /devices/{deviceId}`
  and act as one of them with the `device_id` query parameter next to `user_id`. `POST /login` and a register that would log in answer `400 M_APPSERVICE_LOGIN_UNSUPPORTED`.
- These endpoints answer `404 M_UNRECOGNIZED`: `/login`, `/register`, `/logout`, `/logout/all`, `/account/password`,
  `/account/deactivate`, the 3pid write endpoints, `/delete_devices`, `DELETE /devices/{deviceId}` and the
  user-interactive auth fallback pages. `/capabilities` reports `m.change_password` and `m.3pid_changes` as disabled.
- `registration_shared_secret` is ignored while this is enabled, so `/_synapse/admin/v1/register` is not served.
- Replacing existing cross-signing keys needs approval by the provider and is refused with `403 M_FORBIDDEN` otherwise (see above for MAS).
  The first upload still works without it.

## Limitations

- Dendrite does not implement the provider side (authorization, registration, token and revocation endpoints).
  Run a provider such as [Matrix Authentication Service](https://github.com/element-hq/matrix-authentication-service) or any other OIDC provider.
- With another provider that provisions nothing, use the client ID and secret mode: accounts and devices are then created on first use.
- Switching an existing server over does not migrate the old access tokens: they stop working, and users log in again through the provider.
- [MSC4190](https://github.com/matrix-org/matrix-spec-proposals/pull/4190) (appservice device management) is not implemented.

## Signing in with a QR code

A signed-in device can sign in a new one by scanning a QR code. The two devices exchange short messages through a rendezvous session on a homeserver
([MSC4388](https://github.com/matrix-org/matrix-spec-proposals/pull/4388)). The feature is off by default:

```yaml
client_api:
  rendezvous:
    enabled: true
    # Let anyone create a session, also devices without an account on this server.
    # allow_unauthenticated: false
```

Sessions are kept in memory for five minutes, hold at most 4096 bytes and are limited to 1000 at a time, of which one user can hold 5 and anonymous creators share 200. Only the endpoint to create one needs an access token,
and only if `allow_unauthenticated` is not set. The endpoints are `/_matrix/client/v1/rendezvous` and the unstable `/_matrix/client/unstable/io.element.msc4388/rendezvous`.
The provider (MAS) has to support signing in a device with a QR code as well, through the device authorization grant.
