// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/federationapi"
	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/storage"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
)

// A room that is joined with the memberships of other users left out is marked, with the servers to ask for them, and
// has the state it was given.
func TestJoinWithPartialState(t *testing.T) {
	admin := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:r", test.PrivateKeyB))
	bob := test.NewUser(t, test.WithSigningServer("second.example", "ed25519:s", test.PrivateKeyA))
	local := test.NewUser(t)

	room := test.NewRoom(t, admin, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
	bobJoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(bob.ID))
	join := room.CreateAndInsert(t, local, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(local.ID))

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := jetstream.NATSInstance{}
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		fsAPI := federationapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, nil, rsAPI, caches, nil, true)
		rsAPI.SetFederationAPI(fsAPI, nil)
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
		require.NoError(t, err)
		ctx := context.Background()

		// The state the server answered with: all of it but the membership of bob.
		var state []gomatrixserverlib.PDU
		for _, ev := range room.CurrentState() {
			if ev.EventID() != bobJoin.EventID() && ev.EventID() != join.EventID() {
				state = append(state, ev.PDU)
			}
		}
		respState := &fclient.RespState{StateEvents: gomatrixserverlib.NewEventJSONsFromEvents(state)}

		require.NoError(t, api.SendEventWithPartialState(
			ctx, rsAPI, cfg.Global.ServerName, api.KindNew, respState, join, "remote.example", nil, false,
			[]string{"remote.example", "second.example"},
		))

		info, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		require.NotNil(t, info)
		partial, err := db.PartialStateOfRoom(ctx, info.RoomNID)
		require.NoError(t, err)
		require.NotNil(t, partial, "the room is marked")
		assert.Equal(t, join.EventID(), partial.JoinEventID)
		assert.Equal(t, []string{"remote.example", "second.example"}, partial.Servers)

		// The state is the one that was given, plus the join: bob's membership is not in it.
		res := api.QueryCurrentStateResponse{}
		require.NoError(t, rsAPI.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
			RoomID: room.ID,
			StateTuples: []gomatrixserverlib.StateKeyTuple{
				{EventType: spec.MRoomMember, StateKey: local.ID},
				{EventType: spec.MRoomMember, StateKey: bob.ID},
				{EventType: spec.MRoomCreate, StateKey: ""},
			},
		}, &res))
		assert.Len(t, res.StateEvents, 2)
		for tuple := range res.StateEvents {
			assert.NotEqual(t, bob.ID, tuple.StateKey)
		}

		rooms, err := db.PartialStateRooms(ctx)
		require.NoError(t, err)
		assert.Len(t, rooms, 1)
		require.NoError(t, db.ClearRoomPartialState(ctx, info.RoomNID))
		partial, err = db.PartialStateOfRoom(ctx, info.RoomNID)
		require.NoError(t, err)
		assert.Nil(t, partial)
	})
}
