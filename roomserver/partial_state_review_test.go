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

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/internal"
	"github.com/element-hq/dendrite/roomserver/internal/perform"
	"github.com/element-hq/dendrite/roomserver/storage"
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
)

// partialRoom joins a local user to a room with partial state, through a server that does not answer the resync at
// first, and returns what the tests need.
type partialRoom struct {
	rsAPI    api.RoomserverInternalAPI
	internal *internal.RoomserverInternalAPI
	db       storage.Database
	fed      *fakeStateFederation
	room     *test.Room
	local    *test.User
	nid      types.RoomNID
}

func newPartialRoom(t *testing.T, dbType test.DBType, room *test.Room, partial []gomatrixserverlib.PDU, full []gomatrixserverlib.PDU, join *types.HeaderedEvent) *partialRoom {
	cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeFn)
	cfg.FederationAPI.PartialStateJoins = true
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	natsInstance := jetstream.NATSInstance{}
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	fed := &fakeStateFederation{state: full}
	fed.fail.Store(true)
	rsAPI.SetFederationAPI(fed, nil)
	internalAPI := rsAPI.(*internal.RoomserverInternalAPI)
	internalAPI.Inputer.KeyRing = &test.NopJSONVerifier{}
	db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
	require.NoError(t, err)
	require.NoError(t, api.SendEventWithPartialState(
		context.Background(), rsAPI, cfg.Global.ServerName, api.KindNew,
		&fclient.RespState{StateEvents: gomatrixserverlib.NewEventJSONsFromEvents(partial)},
		join, "remote.example", nil, false, []string{"remote.example"},
	))
	info, err := db.RoomInfo(context.Background(), room.ID)
	require.NoError(t, err)
	return &partialRoom{rsAPI: rsAPI, internal: internalAPI, db: db, fed: fed, room: room, nid: info.RoomNID}
}

func (p *partialRoom) record(t *testing.T) *types.PartialState {
	t.Helper()
	rec, err := p.db.PartialStateOfRoom(context.Background(), p.nid)
	require.NoError(t, err)
	return rec
}

// The resync ends only with the state of the room: an answer without members is no state, and the other server may
// be one that is still joining itself.
func TestPartialStateResyncNeedsMembers(t *testing.T) {
	admin := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:r", test.PrivateKeyB))
	local := test.NewUser(t)
	room := test.NewRoom(t, admin, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
	join := room.CreateAndInsert(t, local, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(local.ID))
	var partial, noMembers []gomatrixserverlib.PDU
	for _, ev := range room.CurrentState() {
		if ev.Type() != spec.MRoomMember {
			noMembers = append(noMembers, ev.PDU)
			if ev.EventID() != join.EventID() {
				partial = append(partial, ev.PDU)
			}
		}
		if ev.Type() == spec.MRoomMember && ev.EventID() != join.EventID() {
			partial = append(partial, ev.PDU)
		}
	}

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		p := newPartialRoom(t, dbType, room, partial, noMembers, join)
		require.NotNil(t, p.record(t))
		p.fed.fail.Store(false) // the other server answers now, with a state that has no members
		require.Eventually(t, func() bool { return p.fed.asked.Load() >= 2 }, 15*time.Second, 50*time.Millisecond, "the resync asked")
		time.Sleep(500 * time.Millisecond)
		assert.NotNil(t, p.record(t), "an answer without members does not end the resync")
	})
}

// A room whose user left is dropped only when nobody of the server is in it, and not before the join has been
// processed: the other users of the server need the state too.
func TestPartialStateRecordIsKeptWhileSomeoneIsIn(t *testing.T) {
	admin := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:r", test.PrivateKeyB))
	local := test.NewUser(t)
	second := test.NewUser(t)
	room := test.NewRoom(t, admin, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
	join := room.CreateAndInsert(t, local, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(local.ID))
	secondJoin := room.CreateAndInsert(t, second, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(second.ID))
	leave := room.CreateAndInsert(t, local, spec.MRoomMember, map[string]string{"membership": "leave"}, test.WithStateKey(local.ID))
	secondLeave := room.CreateAndInsert(t, second, spec.MRoomMember, map[string]string{"membership": "leave"}, test.WithStateKey(second.ID))
	var partial []gomatrixserverlib.PDU
	for _, ev := range room.Events() {
		if ev.EventID() == join.EventID() {
			break
		}
		partial = append(partial, ev.PDU)
	}
	// What the state at the join has: the events of the room before the join, as far as they are state.
	var stateAtJoin []gomatrixserverlib.PDU
	for _, ev := range partial {
		if ev.StateKey() != nil {
			stateAtJoin = append(stateAtJoin, ev)
		}
	}

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		p := newPartialRoom(t, dbType, room, stateAtJoin, nil, join)
		ctx := context.Background()
		in := func(evs ...*types.HeaderedEvent) {
			t.Helper()
			require.NoError(t, api.SendEvents(ctx, p.rsAPI, api.KindNew, evs, "test", "test", "test", nil, false))
		}
		in(secondJoin)
		resyncer := func(grace time.Duration) *perform.PartialStateResyncer {
			return &perform.PartialStateResyncer{DB: p.db, Admin: p.internal.Admin, LeftGrace: grace}
		}
		rec := p.record(t)
		require.NotNil(t, rec)

		// The user who joined leaves, another user of this server is in the room: the record stays.
		in(leave)
		_ = resyncer(-1).Resync(ctx, *rec)
		assert.NotNil(t, p.record(t), "the second user needs the state")

		// Nobody is in the room, but the join is a moment ago: not yet taken for a leave.
		in(secondLeave)
		_ = resyncer(time.Hour).Resync(ctx, *rec)
		assert.NotNil(t, p.record(t), "the grace after the join")

		// Nobody, and the grace is over: dropped.
		require.NoError(t, resyncer(-1).Resync(ctx, *rec))
		assert.Nil(t, p.record(t), "nobody of this server is in the room")
	})
}
