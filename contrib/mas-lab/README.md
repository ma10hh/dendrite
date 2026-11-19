# Dendrite with Matrix Authentication Service

A compose lab that runs this checkout of Dendrite behind [Matrix Authentication Service](https://github.com/element-hq/matrix-authentication-service)
(pinned to 1.29.0) and Postgres, and `test.sh`, which checks the delegated authentication of the fork against it.

```
./run.sh            # build, start from scratch, seed a user, run the checks, tear down
SLOW=1 ./run.sh     # also waits for an access token to expire (5 minutes) and refreshes it
KEEP=1 ./run.sh     # leave the lab running afterwards (Dendrite on :8008, MAS on :8080)
```

It needs Docker, `curl`, `jq` and `openssl`. URL previews are switched on and rate limiting is switched off in the generated configuration of Dendrite: the checks send bursts of requests and test behaviour, not limits. `setup.sh` builds the image from this checkout and writes the configuration, keys and secrets into
`generated/` (not committed). The names `mas` and `dendrite` are mapped to localhost by `curl --resolve`, so the issuer is the same for the checks and for the containers.

## What the checks do

They act like a client that signs in with the authorization code flow and PKCE, with a device ID of its own choosing, and then use the server with that token.
The checks cover:

- discovery (`auth_metadata`, the account management metadata, well-known) and the legacy endpoints being gone
- the `/_synapse/mas/` endpoints refusing everybody but MAS, and MAS provisioning users and devices through them
- sign in, refresh token rotation, sync, sliding sync, media, and token expiry (slow mode)
- logout through token revocation, deactivation of a user in MAS, compatibility tokens and the admin scope
- replacing cross-signing keys, approved by MAS through its own API
- rendezvous sessions for QR login, at the HTTP level
- appservices creating and acting as devices, and a restart of the homeserver

## What they do not cover

No real client is involved. Element X, the encrypted channel of the QR login (the checks only exercise the rendezvous session it uses),
the browser pages of MAS beyond filling in the login form, and sliding sync as Element X uses it are not tested.
