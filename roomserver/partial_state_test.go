// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fsapi "github.com/element-hq/dendrite/federationapi/api"
	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/internal"
	"github.com/element-hq/dendrite/roomserver/storage"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
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
		rsAPI.SetFederationAPI(&fakeStateFederation{}, nil)
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

		localID, _ := spec.NewUserID(local.ID, true)
		membership := api.QueryMembershipForUserResponse{}
		require.NoError(t, rsAPI.QueryMembershipForUser(ctx, &api.QueryMembershipForUserRequest{RoomID: room.ID, UserID: *localID}, &membership))
		assert.True(t, membership.PartialState, "the members of the room are not all known")
		server := api.QueryServerJoinedToRoomResponse{}
		require.NoError(t, rsAPI.QueryServerJoinedToRoom(ctx, &api.QueryServerJoinedToRoomRequest{RoomID: room.ID}, &server))
		assert.True(t, server.PartialState)

		rooms, err := db.PartialStateRooms(ctx)
		require.NoError(t, err)
		assert.Len(t, rooms, 1)
		require.NoError(t, db.ClearRoomPartialState(ctx, info.RoomNID))
		partial, err = db.PartialStateOfRoom(ctx, info.RoomNID)
		require.NoError(t, err)
		assert.Nil(t, partial)
		require.NoError(t, rsAPI.QueryMembershipForUser(ctx, &api.QueryMembershipForUserRequest{RoomID: room.ID, UserID: *localID}, &membership))
		assert.False(t, membership.PartialState)
	})
}

// fakeStateFederation is the other server: it has the whole state of the room.
type fakeStateFederation struct {
	fsapi.RoomserverFederationAPI
	state []gomatrixserverlib.PDU
	asked atomic.Int32
}

func (f *fakeStateFederation) QueryJoinedHostServerNamesInRoom(context.Context, *fsapi.QueryJoinedHostServerNamesInRoomRequest, *fsapi.QueryJoinedHostServerNamesInRoomResponse) error {
	return nil
}

func (f *fakeStateFederation) LookupState(_ context.Context, _, _ spec.ServerName, _, _ string, _ gomatrixserverlib.RoomVersion) (gomatrixserverlib.StateResponse, error) {
	f.asked.Add(1)
	return &fclient.RespState{StateEvents: gomatrixserverlib.NewEventJSONsFromEvents(f.state)}, nil
}

// The background fetch gives the room the memberships that were left out and removes the record.
func TestPartialStateResync(t *testing.T) {
	admin := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:r", test.PrivateKeyB))
	bob := test.NewUser(t, test.WithSigningServer("second.example", "ed25519:s", test.PrivateKeyA))
	local := test.NewUser(t)

	room := test.NewRoom(t, admin, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
	bobJoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(bob.ID))
	join := room.CreateAndInsert(t, local, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(local.ID))
	next := room.CreateAndInsert(t, local, "m.room.message", map[string]string{"body": "after"})

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := jetstream.NATSInstance{}
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)

		var partial, full []gomatrixserverlib.PDU
		for _, ev := range room.CurrentState() {
			full = append(full, ev.PDU)
			if ev.EventID() != bobJoin.EventID() && ev.EventID() != join.EventID() {
				partial = append(partial, ev.PDU)
			}
		}
		fed := &fakeStateFederation{state: full}
		rsAPI.SetFederationAPI(fed, nil)
		internalAPI := rsAPI.(*internal.RoomserverInternalAPI)
		internalAPI.Inputer.KeyRing = &test.NopJSONVerifier{}
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
		require.NoError(t, err)
		ctx := context.Background()

		require.NoError(t, api.SendEventWithPartialState(
			ctx, rsAPI, cfg.Global.ServerName, api.KindNew,
			&fclient.RespState{StateEvents: gomatrixserverlib.NewEventJSONsFromEvents(partial)},
			join, "remote.example", nil, false, []string{"remote.example"},
		))
		memberOf := func(user string) int {
			res := api.QueryCurrentStateResponse{}
			require.NoError(t, rsAPI.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
				RoomID:      room.ID,
				StateTuples: []gomatrixserverlib.StateKeyTuple{{EventType: spec.MRoomMember, StateKey: user}},
			}, &res))
			return len(res.StateEvents)
		}
		assert.Equal(t, 0, memberOf(bob.ID), "bob is not in the state yet")
		latest := func() []string {
			res := api.QueryLatestEventsAndStateResponse{}
			require.NoError(t, rsAPI.QueryLatestEventsAndState(ctx, &api.QueryLatestEventsAndStateRequest{RoomID: room.ID}, &res))
			return res.LatestEvents
		}
		before := latest()

		info, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			p, perr := db.PartialStateOfRoom(ctx, info.RoomNID)
			return perr == nil && p == nil
		}, 15*time.Second, 100*time.Millisecond, "the record goes when the state has been fetched")
		assert.GreaterOrEqual(t, int(fed.asked.Load()), 1)
		assert.Equal(t, 1, memberOf(bob.ID), "bob is in the state")
		assert.Equal(t, 1, memberOf(local.ID))
		assert.Equal(t, before, latest(), "no event was added to the room")

		// An event that comes after starts from a state that has the members.
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*rstypes.HeaderedEvent{next}, cfg.Global.ServerName, cfg.Global.ServerName, "test", nil, false))
		assert.Equal(t, 1, memberOf(bob.ID), "bob is still in the state after a new event")
		res := api.QueryMembershipsForRoomResponse{}
		require.NoError(t, rsAPI.QueryMembershipsForRoom(ctx, &api.QueryMembershipsForRoomRequest{RoomID: room.ID, JoinedOnly: true}, &res))
		assert.Len(t, res.JoinEvents, 3, "the creator, bob and the local user are joined")
	})
}
