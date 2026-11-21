// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
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
	"github.com/element-hq/dendrite/roomserver/storage/tables"
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
)

// leaveFederation is the other server: asked to let the user leave, it fails, or it answers the knock with an invite
// in the meantime.
type leaveFederation struct {
	fsapi.RoomserverFederationAPI
	during func()
}

func (f *leaveFederation) PerformLeave(context.Context, *fsapi.PerformLeaveRequest, *fsapi.PerformLeaveResponse) error {
	if f.during != nil {
		f.during()
	}
	return errors.New("the other server is not there")
}

// A user can withdraw a knock on a room of another server whatever that server does, the sync API is told that it
// ended with a leave (which writes the leave event for the clients), and an invite that arrived on the way is kept.
func TestWithdrawRemoteKnock(t *testing.T) {
	remote := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:r", test.PrivateKeyB))
	local := test.NewUser(t)
	room := test.NewRoom(t, remote, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
	room.CreateAndInsert(t, remote, spec.MRoomJoinRules, map[string]string{"join_rule": "knock"}, test.WithStateKey(""))
	knock := room.CreateAndInsert(t, local, spec.MRoomMember, map[string]string{"membership": "knock"}, test.WithStateKey(local.ID))
	invite := room.CreateAndInsert(t, remote, spec.MRoomMember, map[string]string{"membership": "invite"}, test.WithStateKey(local.ID))
	userID, err := spec.NewUserID(local.ID, true)
	require.NoError(t, err)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := jetstream.NATSInstance{}
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		fed := &leaveFederation{}
		rsAPI.SetFederationAPI(fed, nil)
		leaver := rsAPI.(*internal.RoomserverInternalAPI).Leaver
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
		require.NoError(t, err)
		ctx := context.Background()

		setMembership := func(state tables.MembershipState, ev *types.HeaderedEvent) {
			t.Helper()
			updater, err := db.MembershipUpdater(ctx, room.ID, local.ID, true, gomatrixserverlib.RoomVersionV10)
			require.NoError(t, err)
			_, _, err = updater.Update(state, &types.Event{EventNID: 0, PDU: ev.PDU})
			require.NoError(t, err)
			require.NoError(t, updater.Commit())
		}
		isKnock := func() bool {
			updater, err := db.MembershipUpdater(ctx, room.ID, local.ID, true, gomatrixserverlib.RoomVersionV10)
			require.NoError(t, err)
			defer updater.Rollback() // nolint:errcheck
			return updater.IsKnock()
		}
		leave := func() ([]api.OutputEvent, error) {
			return leaver.PerformLeave(ctx, &api.PerformLeaveRequest{RoomID: room.ID, Leaver: *userID}, &api.PerformLeaveResponse{})
		}

		t.Run("the knock ends with a leave even if the other server fails", func(t *testing.T) {
			setMembership(tables.MembershipStateKnock, knock)
			require.True(t, isKnock())
			outputs, err := leave()
			require.NoError(t, err)
			require.Len(t, outputs, 1)
			require.NotNil(t, outputs[0].RetireKnockEvent)
			assert.Equal(t, spec.Leave, outputs[0].RetireKnockEvent.Membership, "the sync API only writes a leave for this")
			assert.False(t, isKnock(), "the knock is gone")
		})

		t.Run("an invite that came in the meantime is not deleted", func(t *testing.T) {
			setMembership(tables.MembershipStateKnock, knock)
			fed.during = func() { setMembership(tables.MembershipStateInvite, invite) }
			defer func() { fed.during = nil }()
			outputs, _ := leave()
			for _, out := range outputs {
				assert.Nil(t, out.RetireKnockEvent, "the knock was answered, it is not retired as withdrawn")
			}
			updater, err := db.MembershipUpdater(ctx, room.ID, local.ID, true, gomatrixserverlib.RoomVersionV10)
			require.NoError(t, err)
			defer updater.Rollback() // nolint:errcheck
			assert.False(t, updater.IsKnock())
			assert.True(t, updater.IsInvite(), "the invite is still there for the user to answer")
		})
	})
}
