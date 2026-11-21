package roomserver_test

import (
	"context"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
)

// The state of a room without its memberships is for callers that ask for the ones they need: /context with lazy
// loading used to load all of them and throw them away.
func TestQueryStateAfterEventsOmitMembers(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
	room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": "join"}, test.WithStateKey(bob.ID))
	last := room.CreateAndInsert(t, alice, spec.MRoomName, map[string]string{"name": "x"}, test.WithStateKey(""))

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

		query := func(omit bool, tuples ...gomatrixserverlib.StateKeyTuple) (types []string) {
			t.Helper()
			var res api.QueryStateAfterEventsResponse
			require.NoError(t, rsAPI.QueryStateAfterEvents(ctx, &api.QueryStateAfterEventsRequest{
				RoomID: room.ID, PrevEventIDs: []string{last.EventID()}, StateToFetch: tuples, OmitMembers: omit,
			}, &res))
			require.True(t, res.PrevEventsExist)
			for _, ev := range res.StateEvents {
				types = append(types, ev.Type()+"|"+*ev.StateKey())
			}
			return types
		}
		assert.Contains(t, query(false), spec.MRoomMember+"|"+bob.ID)
		without := query(true)
		for _, ty := range without {
			assert.NotContains(t, ty, spec.MRoomMember, "no membership with OmitMembers")
		}
		assert.Contains(t, without, spec.MRoomName+"|", "the rest of the state is there")
		assert.Contains(t, without, spec.MRoomCreate+"|")
		// A member that is asked for by name is a different call and is still given.
		named := query(true, gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: bob.ID})
		assert.Contains(t, named, spec.MRoomMember+"|"+bob.ID)
		assert.NotContains(t, named, spec.MRoomMember+"|"+alice.ID)
	})
}
