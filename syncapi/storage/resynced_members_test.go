package storage_test

import (
	"context"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/syncapi/types"
	"github.com/element-hq/dendrite/test"
)

// Members added to a room that was joined with partial state are counted and are in the room's state, without being
// in its timeline.
func TestAddResyncedMembers(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()
		alice := test.NewUser(t)
		bob := test.NewUser(t, test.WithSigningServer("second.example", "ed25519:s", test.PrivateKeyA))
		carol := test.NewUser(t, test.WithSigningServer("third.example", "ed25519:t", test.PrivateKeyA))
		room := test.NewRoom(t, alice)
		bobJoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(bob.ID))
		carolInvite := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]string{"membership": "invite"}, test.WithStateKey(carol.ID))

		// The room as the local user joined it: only the creator's own state.
		var joined []*rstypes.HeaderedEvent
		for _, ev := range room.Events() {
			if ev.EventID() != bobJoin.EventID() && ev.EventID() != carolInvite.EventID() {
				joined = append(joined, ev)
			}
		}
		positions := MustWriteEvents(t, db, joined)
		last := positions[len(positions)-1]

		count := func() (int, int) {
			snapshot, err := db.NewDatabaseSnapshot(ctx)
			require.NoError(t, err)
			defer snapshot.Rollback() // nolint:errcheck
			j, i, err := snapshot.RoomMemberCounts(ctx, room.ID)
			require.NoError(t, err)
			return j, i
		}
		joinedBefore, invitedBefore := count()

		for _, ev := range []*rstypes.HeaderedEvent{bobJoin, carolInvite} {
			ev.StateKeyResolved = ev.StateKey()
			userID, err := spec.NewUserID(*ev.StateKey(), true)
			require.NoError(t, err)
			ev.UserID = *userID
		}
		require.NoError(t, db.AddResyncedMembers(ctx, []*rstypes.HeaderedEvent{bobJoin, carolInvite}, last, types.StreamPosition(1)))

		joinedAfter, invitedAfter := count()
		assert.Equal(t, joinedBefore+1, joinedAfter)
		assert.Equal(t, invitedBefore+1, invitedAfter)

		// They are not in the timeline: nothing was written to it.
		snapshot, err := db.NewDatabaseSnapshot(ctx)
		require.NoError(t, err)
		defer snapshot.Rollback() // nolint:errcheck
		filter := synctypes.DefaultRoomEventFilter()
		recent, err := snapshot.RecentEvents(ctx, []string{room.ID}, types.Range{From: 0, To: last + 100}, &filter, true, true)
		require.NoError(t, err)
		for _, ev := range recent[room.ID].Events {
			assert.NotEqual(t, bobJoin.EventID(), ev.EventID())
		}
	})
}
