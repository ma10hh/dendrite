#!/usr/bin/env bash
# End-to-end checks of Dendrite as a homeserver for Matrix Authentication Service.
# Runs on the host. The compose services are reached through published ports, curl maps
# the names mas and dendrite to localhost. mas-cli runs through docker compose.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
MAS=http://mas:8080
HS=http://dendrite:8008
SECRET=$(cat "$HERE/generated/secret.txt")
AS_TOKEN=lab_as_token_0123456789
curl() { command curl --resolve mas:8080:127.0.0.1 --resolve dendrite:8008:127.0.0.1 "$@"; }
. "$HERE/flow.sh"

PASS=0
FAIL=0
FAILED=()
ok() { PASS=$((PASS + 1)); echo "  PASS  $1"; }
bad() { FAIL=$((FAIL + 1)); FAILED+=("$1"); echo "  FAIL  $1 -- $2"; }
section() { echo; echo "== $1"; }

# call METHOD URL [TOKEN] [JSON BODY]: sets CODE and BODY
call() {
  local method=$1 url=$2 token=${3:-} data=${4:-}
  local args=(-s -o /tmp/body -w '%{http_code}' -X "$method" -D /tmp/headers)
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  [ -n "$data" ] && args+=(-H 'Content-Type: application/json' -d "$data")
  CODE=$(curl "${args[@]}" "$url")
  BODY=$(cat /tmp/body)
}
header() { grep -i "^$1:" /tmp/headers | tr -d '\r' | cut -d' ' -f2-; }

expect() { # name want got
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want '$2', got '$3'"; fi
}
expect_code() { # name want-code (uses CODE/BODY)
  if [ "$CODE" = "$2" ]; then ok "$1"; else bad "$1" "want HTTP $2, got $CODE $(echo "$BODY" | head -c 200)"; fi
}
expect_err() { # name want-code want-errcode
  local got; got=$(echo "$BODY" | jq -r .errcode 2>/dev/null)
  if [ "$CODE" = "$2" ] && [ "$got" = "$3" ]; then ok "$1"; else bad "$1" "want $2 $3, got $CODE $(echo "$BODY" | head -c 200)"; fi
}
# wait_for_code TOKEN "CODE ..." SECONDS: whoami until it answers one of the codes. Sets CODE and WAITED.
wait_for_code() {
  local token=$1 want=$2 max=$3 i start=$SECONDS
  for ((i = 0; i < max * 2; i++)); do
    call GET $HS/_matrix/client/v3/account/whoami "$token"
    case " $want " in *" $CODE "*) WAITED=$((SECONDS - start)); return 0 ;; esac
    sleep 0.5
  done
  return 1
}
mas_cli() { (cd "$HERE" && docker compose run --rm --no-deps -T mas manage "$@" --config /config.yaml 2>&1); }
# login USER PASSWORD: sign in through MAS or stop, so that later checks never run with an empty token.
login() {
  mas_login "$1" "$2" 2>/tmp/login.err
  if [ "${ACCESS:-}" = "" ] || [ "$ACCESS" = null ]; then
    bad "sign in as $1 through MAS" "$(tail -c 300 /tmp/login.err)"
    echo; echo "RESULT $PASS passed, $FAIL failed (stopped: no session)"
    exit 1
  fi
}

section "Discovery (MSC2965) and disabled legacy endpoints"
call GET $HS/_matrix/client/v1/auth_metadata
expect_code "auth_metadata is served" 200
expect "auth_metadata issuer is MAS" "$MAS/" "$(echo "$BODY" | jq -r .issuer)"
for f in authorization_endpoint token_endpoint registration_endpoint revocation_endpoint; do
  if [ "$(echo "$BODY" | jq -r ".$f // empty")" != "" ]; then ok "auth_metadata has $f"; else bad "auth_metadata has $f" "missing"; fi
done
expect "auth_metadata supports S256" "true" "$(echo "$BODY" | jq '.code_challenge_methods_supported | index("S256") != null')"
expect "auth_metadata is cacheable" "public, max-age=3600" "$(call GET $HS/_matrix/client/v1/auth_metadata; header cache-control)"
call GET $HS/_matrix/client/unstable/org.matrix.msc2965/auth_issuer
expect "auth_issuer (unstable) names MAS" "$MAS/" "$(echo "$BODY" | jq -r .issuer)"
call GET $HS/_matrix/client/v1/auth_metadata
expect "account management URI is passed through (MSC4191)" "$MAS/account/" "$(echo "$BODY" | jq -r .account_management_uri)"
expect "cross-signing reset is an advertised action" "true" "$(echo "$BODY" | jq '.account_management_actions_supported | index("org.matrix.cross_signing_reset") != null')"
call GET $HS/.well-known/matrix/client
expect "well-known names the issuer" "$MAS/" "$(echo "$BODY" | jq -r '."org.matrix.msc2965.authentication".issuer')"
call POST $HS/_matrix/client/v3/login "" '{"type":"m.login.password","user":"alice","password":"x"}'
expect_err "legacy POST /login is gone" 404 M_UNRECOGNIZED
call POST $HS/_matrix/client/v3/register "" '{"username":"mallory","password":"x","auth":{"type":"m.login.dummy"}}'
expect_err "legacy POST /register is gone" 404 M_UNRECOGNIZED
call POST $HS/_synapse/admin/v1/register "" '{}'
expect_code "shared-secret registration is not served" 404

section "MAS endpoints only answer MAS"
call GET "$HS/_synapse/mas/query_user?localpart=alice"
expect_err "no secret is refused" 403 M_FORBIDDEN
call GET "$HS/_synapse/mas/query_user?localpart=alice" wrong-secret
expect_err "wrong secret is refused" 403 M_FORBIDDEN
call GET "$HS/_synapse/mas/query_user?localpart=alice" "$SECRET"
expect_code "right secret is accepted" 200
call GET "$HS/_synapse/mas/query_user?localpart=nobody" "$SECRET"
expect_code "unknown user is 404" 404
call GET "$HS/_synapse/mas/is_localpart_available?localpart=alice" "$SECRET"
expect_code "taken localpart is refused" 400
call GET "$HS/_synapse/mas/is_localpart_available?localpart=fresh" "$SECRET"
expect_code "free localpart is available" 200

section "Sign in through MAS (authorization code flow, client-chosen device ID)"
login alice correct-horse-battery
ALICE=$ACCESS; ALICE_REFRESH=$REFRESH; ALICE_DEVICE=$DEVICE; ALICE_CLIENT=$CID; ALICE_JAR=$MAS_JAR
ALICE_MAS_ID=$(curl -s -b "$ALICE_JAR" -X POST $MAS/graphql -H 'Content-Type: application/json' -d '{"query":"{ viewer { ... on User { id } } }"}' | jq -r .data.viewer.id)
if [ "$ALICE" = "" ] || [ "$ALICE" = null ]; then bad "login through MAS" "no access token"; echo "RESULT $PASS passed, $FAIL failed"; exit 1; fi
ok "login through MAS gives an access token"
expect "token response has expires_in" "true" "$(echo "$TOKEN_JSON" | jq 'has("expires_in")')"
call GET $HS/_matrix/client/v3/account/whoami "$ALICE"
expect_code "whoami with the MAS token" 200
expect "whoami user" "@alice:lab.test" "$(echo "$BODY" | jq -r .user_id)"
expect "whoami device is the one the client chose" "$ALICE_DEVICE" "$(echo "$BODY" | jq -r .device_id)"
call GET $HS/_matrix/client/v3/devices "$ALICE"
expect "device list has the device MAS created" "true" "$(echo "$BODY" | jq --arg d "$ALICE_DEVICE" '[.devices[].device_id] | index($d) != null')"
call GET "$HS/_matrix/client/v3/profile/@alice:lab.test/displayname"
expect "display name was provisioned from MAS" "alice" "$(echo "$BODY" | jq -r .displayname)"
call GET $HS/_matrix/client/v3/account/whoami "garbage-token"
expect_err "unknown token is M_UNKNOWN_TOKEN" 401 M_UNKNOWN_TOKEN
call GET $HS/_matrix/client/v3/account/whoami
expect_err "missing token is M_MISSING_TOKEN" 401 M_MISSING_TOKEN

section "Using the server with the MAS token"
call POST $HS/_matrix/client/v3/createRoom "$ALICE" '{"name":"Lab room","preset":"private_chat"}'
expect_code "create a room" 200
ROOM=$(echo "$BODY" | jq -r .room_id)
call PUT "$HS/_matrix/client/v3/rooms/$ROOM/send/m.room.message/txn1" "$ALICE" '{"msgtype":"m.text","body":"hello from the lab"}'
expect_code "send a message" 200
EVENT=$(echo "$BODY" | jq -r .event_id)
call GET "$HS/_matrix/client/v3/sync?timeout=0" "$ALICE"
expect_code "sync" 200
expect "sync shows the message" "true" "$(echo "$BODY" | jq --arg e "$EVENT" '[.rooms.join[].timeline.events[].event_id] | index($e) != null')"
call POST "$HS/_matrix/client/unstable/org.matrix.simplified_msc3575/sync?timeout=0" "$ALICE" '{"lists":{"all":{"ranges":[[0,10]],"timeline_limit":5}}}'
expect_code "sliding sync" 200
expect "sliding sync lists the room" "true" "$(echo "$BODY" | jq --arg r "$ROOM" '.rooms | has($r)')"
section "Sync features: state_after, set_presence, tag and space filters"
call GET "$HS/_matrix/client/v3/sync?timeout=0&use_state_after=true" "$ALICE"
expect_code "sync with use_state_after" 200
expect "state_after has an events array" "array" "$(echo "$BODY" | jq -r --arg r "$ROOM" '.rooms.join[$r].state_after.events | type')"
expect "state is left out when state_after is used" "null" "$(echo "$BODY" | jq -r --arg r "$ROOM" '.rooms.join[$r].state | type')"
expect "state_after has the room state at the end of the timeline" "true" "$(echo "$BODY" | jq --arg r "$ROOM" '[.rooms.join[$r].state_after.events[].type] | index("m.room.create") != null')"
call GET "$HS/_matrix/client/v3/sync?timeout=0&org.matrix.msc4222.use_state_after=true" "$ALICE"
expect "unstable state_after name" "array" "$(echo "$BODY" | jq -r --arg r "$ROOM" '.rooms.join[$r]["org.matrix.msc4222.state_after"].events | type')"
call GET $HS/_matrix/client/versions
expect "versions advertises msc4222" "true" "$(echo "$BODY" | jq '.unstable_features["org.matrix.msc4222"]')"
SS="$HS/_matrix/client/unstable/org.matrix.simplified_msc3575/sync"
call POST "$SS?timeout=0&set_presence=offline" "$ALICE" '{"lists":{"all":{"ranges":[[0,10]],"timeline_limit":1,"required_state":[]}}}'
expect_code "sliding sync with set_presence=offline" 200
call POST "$SS?timeout=0&set_presence=busy" "$ALICE" '{"lists":{"all":{"ranges":[[0,10]],"timeline_limit":1,"required_state":[]}}}'
expect_err "sliding sync refuses an unknown set_presence" 400 M_INVALID_PARAM
call PUT "$HS/_matrix/client/v3/user/%40alice%3Alab.test/rooms/$ROOM/tags/m.favourite" "$ALICE" '{"order":0.5}'
expect_code "tag the room as a favourite" 200
filter_rooms() { call POST "$SS?timeout=0" "$ALICE" "{\"lists\":{\"l\":{\"ranges\":[[0,10]],\"timeline_limit\":1,\"required_state\":[],\"filters\":$1}}}"; echo "$BODY" | jq -c '[(.rooms // {}) | keys[]] | sort'; }
expect "tags filter finds the favourite" "[\"$ROOM\"]" "$(filter_rooms '{"tags":["m.favourite"]}')"
expect "not_tags filter hides the favourite" "[]" "$(filter_rooms '{"not_tags":["m.favourite"]}')"
expect "an unused tag matches nothing" "[]" "$(filter_rooms '{"tags":["u.other"]}')"
expect "a space the user is not in matches nothing" "[]" "$(filter_rooms "{\"spaces\":[\"$ROOM\"]}")"

section "Media"
CONTENT=$(curl -s -H "Authorization: Bearer $ALICE" -H 'Content-Type: text/plain' --data-binary 'media body' "$HS/_matrix/media/v3/upload?filename=a.txt")
MXC=$(echo "$CONTENT" | jq -r .content_uri)
if [ "$MXC" != null ] && [ -n "$MXC" ]; then
  ok "upload media"
  call GET "$HS/_matrix/client/v1/media/download/${MXC#mxc://}" "$ALICE"
  expect_code "authenticated media download" 200
  expect "media CSP has sandbox" "true" "$(header content-security-policy | grep -q 'sandbox' && echo true || echo false)"
  expect "media is nosniff" "nosniff" "$(header x-content-type-options)"
else
  bad "upload media" "$CONTENT"
fi

section "Refresh token grant (MSC2964)"
NEW=$(curl -s -X POST $MAS/oauth2/token -d grant_type=refresh_token -d "refresh_token=$ALICE_REFRESH" -d "client_id=$ALICE_CLIENT")
ALICE2=$(echo "$NEW" | jq -r .access_token); ALICE_REFRESH2=$(echo "$NEW" | jq -r .refresh_token)
if [ "$ALICE2" != null ] && [ "$ALICE2" != "$ALICE" ]; then ok "refresh gives a new access token"; else bad "refresh gives a new access token" "$NEW"; fi
expect "refresh rotates the refresh token" "true" "$([ "$ALICE_REFRESH2" != null ] && [ "$ALICE_REFRESH2" != "$ALICE_REFRESH" ] && echo true || echo false)"
call GET $HS/_matrix/client/v3/account/whoami "$ALICE2"
expect "refreshed token keeps the device" "$ALICE_DEVICE" "$(echo "$BODY" | jq -r .device_id)"
ALICE=$ALICE2; ALICE_REFRESH=$ALICE_REFRESH2

section "Cross-signing keys (approval from MAS needed to replace)"
newkey() { printf '%s' "$(openssl rand 32 | openssl base64 -A | tr -d '=')"; }
masterbody() { local k; k=$(newkey); echo "{\"master_key\":{\"user_id\":\"@alice:lab.test\",\"usage\":[\"master\"],\"keys\":{\"ed25519:$k\":\"$k\"}}}"; }
call POST $HS/_matrix/client/v3/keys/device_signing/upload "$ALICE" "$(masterbody)"
expect_code "first upload needs no re-authentication" 200
call POST $HS/_matrix/client/v3/keys/device_signing/upload "$ALICE" "$(masterbody)"
expect_err "replacing the keys is refused without approval" 403 M_FORBIDDEN
MUT=$(curl -s -b "$ALICE_JAR" -c "$ALICE_JAR" -X POST $MAS/graphql -H 'Content-Type: application/json' -d "{\"query\":\"mutation { allowUserCrossSigningReset(input: {userId: \\\"$ALICE_MAS_ID\\\"}) { user { id } } }\"}")
expect "MAS accepted the approval (GraphQL mutation)" "$ALICE_MAS_ID" "$(echo "$MUT" | jq -r .data.allowUserCrossSigningReset.user.id)"
call POST $HS/_matrix/client/v3/keys/device_signing/upload "$ALICE" "$(masterbody)"
expect_code "replacing the keys works after MAS approved it" 200
call POST $HS/_matrix/client/v3/keys/device_signing/upload "$ALICE" "$(masterbody)"
expect_err "the approval covers one replacement" 403 M_FORBIDDEN

section "Second device, then logout through token revocation (MSC4254)"
login alice correct-horse-battery
ALICE_B=$ACCESS; DEVICE_B=$DEVICE; CLIENT_B=$CID
call GET $HS/_matrix/client/v3/account/whoami "$ALICE_B"
expect "second session has its own device" "$DEVICE_B" "$(echo "$BODY" | jq -r .device_id)"
call GET $HS/_matrix/client/v3/devices "$ALICE"
expect "both devices are listed" "2" "$(echo "$BODY" | jq '.devices | length')"
curl -s -o /dev/null -X POST $MAS/oauth2/revoke -d "token=$ALICE_B" -d "client_id=$CLIENT_B"
if wait_for_code "$ALICE_B" "401" 30; then ok "revoked token stops working (after ~${WAITED}s)"; else bad "revoked token stops working" "still $CODE after 30s"; fi
call GET $HS/_matrix/client/v3/devices "$ALICE"
expect "revoking removed that device on the homeserver" "false" "$(echo "$BODY" | jq --arg d "$DEVICE_B" '[.devices[].device_id] | index($d) != null')"
call GET $HS/_matrix/client/v3/account/whoami "$ALICE"
expect_code "the other session is unaffected" 200

section "Compatibility token and admin scope"
CT=$(mas_cli issue-compatibility-token alice COMPAT1 --yes-i-want-to-grant-synapse-admin-privileges | grep -o 'mct_[A-Za-z0-9_]*' | head -1)
if [ -n "$CT" ]; then
  ok "MAS issued a compatibility token"
  call GET $HS/_matrix/client/v3/account/whoami "$CT"
  expect_code "compatibility token is accepted" 200
  expect "compatibility token device (explicit device_id)" "COMPAT1" "$(echo "$BODY" | jq -r .device_id)"
  call GET "$HS/_synapse/admin/v1/event_reports" "$CT"
  expect_code "admin API works with the admin scope" 200
else
  bad "MAS issued a compatibility token" "no token in output"
fi
call GET "$HS/_synapse/admin/v1/event_reports" "$ALICE"
expect_code "admin API refuses a normal session" 403

section "Deactivating a user in MAS"
mas_cli register-user --yes --password 'second-user-pw-1' bob >/dev/null
sleep 3
login bob second-user-pw-1
BOB=$ACCESS
call GET $HS/_matrix/client/v3/account/whoami "$BOB"
expect_code "bob can use the server" 200
mas_cli lock-user --deactivate bob >/dev/null
if wait_for_code "$BOB" "401 403" 30; then ok "deactivated user is locked out (HTTP $CODE after ~${WAITED}s)"; else bad "deactivated user is locked out" "still HTTP $CODE"; fi
call GET "$HS/_synapse/mas/query_user?localpart=bob" "$SECRET"
expect "homeserver knows bob is deactivated" "true" "$(echo "$BODY" | jq -r .is_deactivated)"

section "Rendezvous sessions for QR login (MSC4388)"
call GET $HS/_matrix/client/v1/rendezvous
expect "anonymous device may create a session" "true" "$(echo "$BODY" | jq -r .create_available)"
call POST $HS/_matrix/client/v1/rendezvous "" '{"data":"hello from the new device"}'
expect_code "create session without a token" 200
RID=$(echo "$BODY" | jq -r .id); SEQ=$(echo "$BODY" | jq -r .sequence_token)
call GET "$HS/_matrix/client/v1/rendezvous/$RID"
expect "scanning device reads the payload" "hello from the new device" "$(echo "$BODY" | jq -r .data)"
call PUT "$HS/_matrix/client/v1/rendezvous/$RID/txn-$(openssl rand -hex 8)" "" "{\"sequence_token\":\"$SEQ\",\"data\":\"reply\"}"
expect_code "scanning device answers" 200
call PUT "$HS/_matrix/client/v1/rendezvous/$RID/txn-$(openssl rand -hex 8)" "" "{\"sequence_token\":\"$SEQ\",\"data\":\"stale\"}"
expect_err "a stale write is a conflict" 409 M_CONCURRENT_WRITE
call GET "$HS/_matrix/client/v1/rendezvous/$RID"
expect "payload is the answer" "reply" "$(echo "$BODY" | jq -r .data)"
call DELETE "$HS/_matrix/client/v1/rendezvous/$RID"
expect_code "cancel the session" 200
call GET "$HS/_matrix/client/v1/rendezvous/$RID"
expect_err "cancelled session is gone" 404 M_NOT_FOUND
GRANTS=$(curl -s $MAS/.well-known/openid-configuration | jq -r '.grant_types_supported | join(" ")')
expect "MAS offers the device authorization grant" "true" "$(echo "$GRANTS" | grep -q 'urn:ietf:params:oauth:grant-type:device_code' && echo true || echo false)"

section "Appservice device management (MSC4190, MSC4326)"
ASH="Authorization: Bearer $AS_TOKEN"
call GET $HS/_matrix/client/v3/account/whoami "$AS_TOKEN"
expect "appservice acts as its sender by default" "@bridgebot:lab.test" "$(echo "$BODY" | jq -r .user_id)"
call POST $HS/_matrix/client/v3/login "$AS_TOKEN" '{"type":"m.login.application_service","identifier":{"type":"m.id.user","user":"bridgebot"}}'
expect_err "appservice login is refused" 400 M_APPSERVICE_LOGIN_UNSUPPORTED
call POST $HS/_matrix/client/v3/register "$AS_TOKEN" '{"type":"m.login.application_service","username":"bridge_ghost"}'
expect_err "appservice register without inhibit_login is refused" 400 M_APPSERVICE_LOGIN_UNSUPPORTED
call POST $HS/_matrix/client/v3/register "$AS_TOKEN" '{"type":"m.login.application_service","username":"bridge_ghost","inhibit_login":true}'
expect_code "appservice registers a user without a login" 200
expect "no access token was handed out" "null" "$(echo "$BODY" | jq -r .access_token)"
call PUT "$HS/_matrix/client/v3/devices/GHOSTDEV?user_id=@bridge_ghost:lab.test" "$AS_TOKEN" '{"display_name":"Ghost device"}'
expect_code "appservice creates a device" 201
call GET "$HS/_matrix/client/v3/account/whoami?user_id=@bridge_ghost:lab.test&device_id=GHOSTDEV" "$AS_TOKEN"
expect "appservice acts as that device" "GHOSTDEV" "$(echo "$BODY" | jq -r .device_id)"
call GET "$HS/_matrix/client/v3/account/whoami?user_id=@bridge_ghost:lab.test&device_id=NOPE" "$AS_TOKEN"
expect_err "unknown device is M_UNKNOWN_DEVICE" 400 M_UNKNOWN_DEVICE
call DELETE "$HS/_matrix/client/v3/devices/GHOSTDEV?user_id=@bridge_ghost:lab.test" "$AS_TOKEN" '{}'
expect_code "appservice deletes the device without UIA" 200
call POST $HS/_matrix/client/v3/login "$ALICE" '{}'
expect_err "users still cannot use /login" 404 M_UNRECOGNIZED
section "Server restart"
(cd "$HERE" && docker compose restart dendrite >/dev/null 2>&1)
for i in $(seq 1 60); do curl -sf $HS/_matrix/client/versions >/dev/null 2>&1 && break; sleep 1; done
call GET $HS/_matrix/client/v3/account/whoami "$ALICE"
expect_code "session survives a restart of the homeserver" 200
expect "same device after the restart" "$ALICE_DEVICE" "$(echo "$BODY" | jq -r .device_id)"
call GET "$HS/_matrix/client/v3/rooms/$ROOM/messages?dir=b&limit=5" "$ALICE"
expect "room history survives a restart" "true" "$(echo "$BODY" | jq --arg e "$EVENT" '[.chunk[].event_id] | index($e) != null')"

if [ "${SLOW:-0}" = 1 ]; then
  section "Access token expiry and refresh (waits for the 5 minute MAS token lifetime)"
  login alice correct-horse-battery
  EXP=$ACCESS; EXP_REFRESH=$REFRESH; EXP_CLIENT=$CID
  echo "  access token lifetime: $(echo "$TOKEN_JSON" | jq .expires_in)s"
  if wait_for_code "$EXP" "401" 360; then
    ok "expired access token is refused (after ~${WAITED}s)"
    expect_err "expired token is M_UNKNOWN_TOKEN" 401 M_UNKNOWN_TOKEN
    echo "  body: $BODY"
    NEW=$(curl -s -X POST $MAS/oauth2/token -d grant_type=refresh_token -d "refresh_token=$EXP_REFRESH" -d "client_id=$EXP_CLIENT")
    call GET $HS/_matrix/client/v3/account/whoami "$(echo "$NEW" | jq -r .access_token)"
    expect_code "refreshed token works on the same device" 200
  else
    bad "expired access token is refused" "still HTTP $CODE after 360s"
  fi
fi

echo
echo "RESULT $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then printf 'FAILED: %s\n' "${FAILED[@]}"; fi
[ "$FAIL" -eq 0 ]
