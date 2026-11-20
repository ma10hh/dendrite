// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/federationapi/routing"
	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
)

func TestSendJoinOmitMembers(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t, test.WithSigningServer("second.example", "ed25519:b", test.PrivateKeyA))
	carol := test.NewUser(t, test.WithSigningServer(knockRemote, "ed25519:c", test.PrivateKeyB))
	dave := test.NewUser(t, test.WithSigningServer(knockRemote, "ed25519:c", test.PrivateKeyB))
	_, daveKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
	for _, u := range []*test.User{bob, carol} {
		room.CreateAndInsert(t, u, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(u.ID))
	}

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := jetstream.NATSInstance{}
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		ctx := context.Background()
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

		roomID, _ := spec.NewRoomID(room.ID)
		daveID, _ := spec.NewUserID(dave.ID, true)

		join := func(omit bool) fclient.RespSendJoin {
			t.Helper()
			path := "/make_join/" + room.ID + "/" + dave.ID
			makeReq := fclient.NewFederationRequest("GET", knockRemote, cfg.Global.ServerName, path)
			made := routing.MakeJoin(httptest.NewRequest(http.MethodGet, path, nil), &makeReq, &cfg.FederationAPI, rsAPI, *roomID, *daveID, []gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionV10})
			require.Equal(t, http.StatusOK, made.Code, "%+v", made.JSON)
			proto := made.JSON.(map[string]any)["event"].(gomatrixserverlib.ProtoEvent)
			event, err := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV10).NewEventBuilderFromProtoEvent(&proto).Build(time.Now(), knockRemote, "ed25519:c", daveKey)
			require.NoError(t, err)

			sendPath := "/send_join/" + room.ID + "/" + event.EventID()
			sendReq := fclient.NewFederationRequest("PUT", knockRemote, cfg.Global.ServerName, sendPath)
			require.NoError(t, sendReq.SetContent(event))
			res := routing.SendJoin(httptest.NewRequest(http.MethodPut, sendPath, nil), &sendReq, &cfg.FederationAPI, rsAPI, &test.NopJSONVerifier{}, *roomID, event.EventID(), omit)
			require.Equal(t, http.StatusOK, res.Code, "%+v", res.JSON)
			return res.JSON.(fclient.RespSendJoin)
		}
		members := func(events []spec.RawJSON) (out []string) {
			for _, raw := range events {
				if gjson.GetBytes(raw, "type").String() == spec.MRoomMember {
					out = append(out, gjson.GetBytes(raw, "state_key").String())
				}
			}
			return out
		}
		toRaw := func(events gomatrixserverlib.EventJSONs) (out []spec.RawJSON) {
			for _, e := range events {
				out = append(out, spec.RawJSON(e))
			}
			return out
		}

		t.Run("members are left out when asked", func(t *testing.T) {
			res := join(true)
			assert.True(t, res.MembersOmitted)
			assert.Equal(t, []string{string(knockRemote), "second.example", "test"}, res.ServersInRoom, "the servers to ask for the rest")
			got := members(toRaw(res.StateEvents))
			assert.ElementsMatch(t, []string{carol.ID}, got, "only the users of the joining server stay (dave joined after the state was read)")
			assert.NotContains(t, got, alice.ID)
			assert.NotContains(t, got, bob.ID)
			// The creator is not in the state, but the power levels name its membership as an auth event.
			assert.Contains(t, members(toRaw(res.AuthEvents)), alice.ID, "the auth chain of what stays is complete")
			assert.NotContains(t, members(toRaw(res.AuthEvents)), bob.ID, "memberships nothing needs are left out")
			// The state that is not about members is whole.
			var types []string
			for _, raw := range res.StateEvents {
				types = append(types, gjson.GetBytes(raw, "type").String())
			}
			assert.Contains(t, types, spec.MRoomCreate)
			assert.Contains(t, types, spec.MRoomPowerLevels)
			assert.Contains(t, types, spec.MRoomJoinRules)
		})

		t.Run("nothing is left out otherwise", func(t *testing.T) {
			res := join(false)
			assert.False(t, res.MembersOmitted)
			assert.Empty(t, res.ServersInRoom)
			assert.ElementsMatch(t, []string{alice.ID, bob.ID, carol.ID, dave.ID}, members(toRaw(res.StateEvents)))
		})
	})
}

func TestResponseOfOmitMembersIsJSON(t *testing.T) {
	raw, err := json.Marshal(fclient.RespSendJoin{MembersOmitted: true, ServersInRoom: []string{"a"}})
	require.NoError(t, err)
	assert.True(t, gjson.GetBytes(raw, "members_omitted").Bool())
	assert.Equal(t, "a", gjson.GetBytes(raw, "servers_in_room.0").String())
}
