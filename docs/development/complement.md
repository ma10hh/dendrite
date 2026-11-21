---
title: Complement
parent: Development
permalink: /development/complement
---

# Complement

[Complement](https://github.com/matrix-org/complement) is the black box test suite for homeservers. This page says how to run it against this fork and what to expect. Results of 2026-11-21.

## Running it with federation (recommended)

`contrib/complement-docker/run.sh` runs Complement in a container on the network of the homeservers, so that the federation tests work on a host where containers cannot connect to the host (the case on the machine of the first runs). See `contrib/complement-docker/README.md`. With it, on 2026-11-21:

| | pass | fail | skip |
|---|---|---|---|
| `main` before the phases 1 to 6 | 459 | 4 | 60 |
| checkout, before the heroes fix | 461 | 3 | 59 |

`./tests ./tests/csapi` with the tag `dendrite_blacklist`, no timeouts to the host in any log. Failures on the checkout: `TestEventAuth/returns_the_auth_chain_for_the_requested_event`, which `main` fails in the same way (the check wants the leave event of a user in the auth chain of that user's next join; the cause was not found), and `TestSendJoinPartialStateResponse`, which `main` skips because it answers send_join with the full state. The checkout failed it because the partial answer lacked the memberships of the heroes; that is fixed (`federationapi/routing/omit_members.go`) and the test passes. `main` also failed `TestRedact` in that run, the checkout did not.

## Running it directly on the host

```bash
# an image of the checkout (this is what build/scripts/complement.sh does)
docker build -t complement-dendrite:local -f build/scripts/Complement.Dockerfile --build-arg CGO=0 .

git clone --depth 1 https://github.com/matrix-org/complement
cd complement
COMPLEMENT_BASE_IMAGE=complement-dendrite:local \
  go test -tags dendrite_blacklist -json -count=1 -timeout 3h ./tests ./tests/csapi > run.json
```

- **Use `-tags dendrite_blacklist`.** Without it Complement does not know which server it tests, prints a warning for every test that Dendrite is known not to support, and runs it anyway. A run without the tag had 147 failures, of which most are such tests.
- Current Complement needs Go 1.26 (its `go.mod`). With `GOTOOLCHAIN=auto` Go downloads it.
- Run it on a machine that nothing else uses: the tests run in parallel and are timing sensitive. A run takes 30 to 40 minutes.
- Federation tests need the containers to reach the test process on the host as `host.docker.internal`. On a machine with a firewall that drops traffic from the Docker bridge to the host they time out (the machine of the first runs); use `contrib/complement-docker/run.sh` there.

## Result

`./tests ./tests/csapi`, image of the checkout (`phase-6-hardening`) and of `main` before the phases 1 to 6, both with the tag:

| | pass | fail | skip |
|---|---|---|---|
| `main` | 402 | 36 | 59 |
| checkout | 402 | 36 | 59 |

The two runs are the same test by test. The 36 failures are 23 tests, and all 23 have connection timeouts to `host.docker.internal` in their logs, so they could not pass on that machine. They have to be run again where the containers reach the host, and until then it is not known whether they pass:

> TestBannedUserCannotSendJoin TestCannotSendNonJoinViaSendJoinV1 TestCannotSendNonJoinViaSendJoinV2 
> TestCannotSendNonLeaveViaSendLeaveV1 TestCannotSendNonLeaveViaSendLeaveV2 
> TestComplementCanCreateValidV12Rooms TestEventAuth TestFederationRejectInvite TestGetMissingEventsGapFilling 
> TestInboundCanReturnMissingEvents TestInboundFederationProfile TestIsDirectFlagFederation 
> TestJoinFederatedRoomWithUnverifiableEvents TestJoinViaRoomIDAndServerName TestMediaWithoutFileName 
> TestMSC4289PrivilegedRoomCreators TestMSC4291RoomIDAsHashOfCreateEvent_AuthEventsOmitsCreateEvent 
> TestOutboundFederationEventSizeGetMissingEvents 
> TestOutboundFederationIgnoresMissingEventWithBadJSONForRoomVersion6 TestOutboundFederationProfile 
> TestOutboundFederationSend TestSendJoinPartialStateResponse TestUnrejectRejectedEvents

**The tag also hides what this fork added.** The list of skipped tests is Dendrite's, and it skips tests for features that this fork has built, such as the thread list (`TestThreadsEndpoint`) and room summary (`TestRoomSummaryAllowedRoomIDs`). To test those, run them without the tag: `go test -json -count=1 -run 'TestThreadsEndpoint|TestRoomSummaryAllowedRoomIDs' ./tests/csapi ./tests`. In the run without the tag (147 failures on the checkout, 152 on `main`, mostly federation timeouts and tests that Dendrite skips) the checkout passed five tests that `main` failed (`TestRoomSummaryAllowedRoomIDs` with two subtests, `TestThreadsEndpoint`, and two federation tests) and failed one that `main` passed (`TestPushRuleRoomUpgrade`, subtest `joining_a_remote_upgraded_room_carries_over_existing_push_rules`; it needs federation with the host, so probably noise, and it is skipped with the tag).

## Not run

- `./tests/...` beyond `./tests` and `./tests/csapi`: the other directories (restricted joins, MSC tests and so on) were not run.
- The faster joins tests (`TestPartialStateJoin*`): partial state joins are off in this fork.
