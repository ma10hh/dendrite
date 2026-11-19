// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/federationapi/routing"
	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
)

const knockRemote = spec.ServerName("remote.example")

func knockRoom(t *testing.T, owner *test.User, joinRule string) *test.Room {
	room := test.NewRoom(t, owner, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetTrustedPrivateChat))
	room.CreateAndInsert(t, owner, spec.MRoomJoinRules, map[string]any{"join_rule": joinRule}, test.WithStateKey(""))
	room.CreateAndInsert(t, owner, spec.MRoomName, map[string]any{"name": "Knock here"}, test.WithStateKey(""))
	return room
}

func membershipOf(t *testing.T, rsAPI api.RoomserverInternalAPI, roomID, userID string) string {
	t.Helper()
	user, err := spec.NewUserID(userID, true)
	require.NoError(t, err)
	res := api.QueryMembershipForUserResponse{}
	require.NoError(t, rsAPI.QueryMembershipForUser(context.Background(), &api.QueryMembershipForUserRequest{RoomID: roomID, UserID: *user}, &res))
	return res.Membership
}

func TestKnock(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	remoteUser := test.NewUser(t, test.WithSigningServer(knockRemote, "ed25519:r", test.PrivateKeyB))
	knockable := knockRoom(t, alice, "knock")
	closed := knockRoom(t, alice, "invite")
	_, remoteKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := jetstream.NATSInstance{}
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		ctx := context.Background()
		for _, room := range []*test.Room{knockable, closed} {
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
		}

		t.Run("a local user knocks, then withdraws the knock", func(t *testing.T) {
			roomID, err := rsAPI.PerformKnock(ctx, &api.PerformKnockRequest{
				RoomIDOrAlias: knockable.ID, UserID: bob.ID, Content: map[string]any{"reason": "let me in"},
			})
			require.NoError(t, err)
			assert.Equal(t, knockable.ID, roomID)
			assert.Equal(t, spec.Knock, membershipOf(t, rsAPI, knockable.ID, bob.ID))

			bobID, _ := spec.NewUserID(bob.ID, true)
			require.NoError(t, rsAPI.PerformLeave(ctx, &api.PerformLeaveRequest{RoomID: knockable.ID, Leaver: *bobID}, &api.PerformLeaveResponse{}))
			assert.Equal(t, spec.Leave, membershipOf(t, rsAPI, knockable.ID, bob.ID))
		})

		t.Run("a local user cannot knock on a room that does not allow it", func(t *testing.T) {
			carol := test.NewUser(t)
			_, err := rsAPI.PerformKnock(ctx, &api.PerformKnockRequest{RoomIDOrAlias: closed.ID, UserID: carol.ID})
			assert.ErrorAs(t, err, &api.ErrNotAllowed{})
			assert.NotEqual(t, spec.Knock, membershipOf(t, rsAPI, closed.ID, carol.ID))
		})

		t.Run("a user of another server cannot knock through this one", func(t *testing.T) {
			_, err := rsAPI.PerformKnock(ctx, &api.PerformKnockRequest{RoomIDOrAlias: knockable.ID, UserID: remoteUser.ID})
			assert.ErrorAs(t, err, &api.ErrInvalidID{})
		})

		fedCfg := &cfg.FederationAPI
		makeKnock := func(room *test.Room, userID string, origin spec.ServerName, versions ...string) util.JSONResponse {
			path := "/make_knock/" + room.ID + "/" + userID
			for i, v := range versions {
				if i == 0 {
					path += "?ver=" + v
				} else {
					path += "&ver=" + v
				}
			}
			req := fclient.NewFederationRequest("GET", origin, cfg.Global.ServerName, path)
			httpReq := httptest.NewRequest(http.MethodGet, path, nil)
			roomID, _ := spec.NewRoomID(room.ID)
			user, _ := spec.NewUserID(userID, true)
			var remoteVersions []gomatrixserverlib.RoomVersion
			for _, v := range versions {
				remoteVersions = append(remoteVersions, gomatrixserverlib.RoomVersion(v))
			}
			res := routing.MakeKnock(httpReq, &req, fedCfg, rsAPI, *roomID, *user, remoteVersions)
			return res
		}

		t.Run("make_knock refuses what the room does not allow", func(t *testing.T) {
			assert.Equal(t, http.StatusForbidden, makeKnock(closed, remoteUser.ID, knockRemote, "10").Code)
			assert.Equal(t, http.StatusForbidden, makeKnock(knockable, remoteUser.ID, "someone.else", "10").Code, "the origin must be the server of the user")
			assert.Equal(t, http.StatusBadRequest, makeKnock(knockable, remoteUser.ID, knockRemote, "9").Code, "room version not supported by the other server")
			assert.Equal(t, http.StatusNotFound, makeKnock(&test.Room{ID: "!unknown:test"}, remoteUser.ID, knockRemote, "10").Code)
		})

		t.Run("a user of another server knocks with make_knock and send_knock", func(t *testing.T) {
			made := makeKnock(knockable, remoteUser.ID, knockRemote, "9", "10")
			require.Equal(t, http.StatusOK, made.Code)
			body := made.JSON.(map[string]any)
			assert.Equal(t, gomatrixserverlib.RoomVersionV10, body["room_version"])
			proto := body["event"].(gomatrixserverlib.ProtoEvent)
			assert.Equal(t, remoteUser.ID, proto.SenderID)

			builder := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV10).NewEventBuilderFromProtoEvent(&proto)
			event, err := builder.Build(time.Now(), knockRemote, "ed25519:r", remoteKey)
			require.NoError(t, err)

			path := "/send_knock/" + knockable.ID + "/" + event.EventID()
			req := fclient.NewFederationRequest("PUT", knockRemote, cfg.Global.ServerName, path)
			require.NoError(t, req.SetContent(event))
			httpReq := httptest.NewRequest(http.MethodPut, path, nil)
			res := routing.SendKnock(httpReq, &req, fedCfg, rsAPI, &test.NopJSONVerifier{}, knockable.ID, event.EventID())
			require.Equal(t, http.StatusOK, res.Code, "%+v", res.JSON)

			state := res.JSON.(fclient.RespSendKnock).KnockRoomState
			assert.NotEmpty(t, state, "the knocking server gets the stripped state of the room")
			assert.Equal(t, spec.Knock, membershipOf(t, rsAPI, knockable.ID, remoteUser.ID))

			// A knock that names another room, or that is not a knock, is refused.
			other := routing.SendKnock(httpReq, &req, fedCfg, rsAPI, &test.NopJSONVerifier{}, closed.ID, event.EventID())
			assert.GreaterOrEqual(t, other.Code, 400)
		})
	})
}
