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

## What Dendrite does

- Every access token is introspected. Results are cached for up to a minute (less if the token expires sooner),
  so a revoked token can keep working for that long. Application service tokens are unaffected.
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
- These endpoints answer `404 M_UNRECOGNIZED`: `/login`, `/register`, `/logout`, `/logout/all`, `/account/password`,
  `/account/deactivate`, the 3pid write endpoints, `/delete_devices`, `DELETE /devices/{deviceId}` and the
  user-interactive auth fallback pages. `/capabilities` reports `m.change_password` and `m.3pid_changes` as disabled.
- Replacing existing cross-signing keys needs re-authentication and is refused with `403 M_FORBIDDEN`.
  The first upload still works without it.

## Limitations

- Dendrite does not implement the provider side (authorization, registration, token and revocation endpoints).
  Run a provider such as [Matrix Authentication Service](https://github.com/element-hq/matrix-authentication-service) or any other OIDC provider.
- Matrix Authentication Service also provisions users and devices through Synapse specific endpoints
  (`/_synapse/mas/...`), which Dendrite does not offer. A provider that only needs introspection works.
- Switching an existing server over does not migrate the old access tokens: they stop working, and users log in again through the provider.
- [MSC4190](https://github.com/matrix-org/matrix-spec-proposals/pull/4190) (appservice device management) is not implemented.
