package retention_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"

	"github.com/element-hq/dendrite/internal/sqlutil"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/syncapi/retention"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/syncapi/types"
	"github.com/element-hq/dendrite/test"
)

type fakePruner struct {
	mu     sync.Mutex
	pruned map[string][]string
	err    error
}

func (f *fakePruner) PerformAdminPruneEvents(_ context.Context, roomID string, ids []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.pruned == nil {
		f.pruned = map[string][]string{}
	}
	f.pruned[roomID] = append(f.pruned[roomID], ids...)
	return len(ids), nil
}

func mustCreateDatabase(t *testing.T, dbType test.DBType) (storage.Database, func()) {
	t.Helper()
	connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
	cm := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
	db, err := storage.NewSyncServerDatasource(context.Background(), cm, &config.DatabaseOptions{
		ConnectionString: config.DataSource(connStr),
	})
	if err != nil {
		t.Fatalf("NewSyncServerDatasource returned %s", err)
	}
	return db, closeDB
}

func writeEvents(t *testing.T, db storage.Database, events []*rstypes.HeaderedEvent) {
	t.Helper()
	for _, ev := range events {
		var addState []*rstypes.HeaderedEvent
		var addStateIDs []string
		if ev.StateKey() != nil {
			ev.StateKeyResolved = ev.StateKey()
			addState = append(addState, ev)
			addStateIDs = append(addStateIDs, ev.EventID())
		}
		if _, err := db.WriteEvent(context.Background(), ev, addState, addStateIDs, nil, nil, false, gomatrixserverlib.HistoryVisibilityShared); err != nil {
			t.Fatalf("WriteEvent: %s", err)
		}
	}
}

// remaining returns the event IDs of the timeline of the room.
func remaining(t *testing.T, db storage.Database, roomID string) map[string]bool {
	t.Helper()
	snapshot, err := db.NewDatabaseSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback() // nolint: errcheck
	from := types.TopologyToken{Depth: math.MaxInt64, PDUPosition: math.MaxInt64}
	to := types.TopologyToken{}
	events, _, _, err := snapshot.GetEventsInTopologicalRange(context.Background(),
		&from, &to, roomID,
		&synctypes.RoomEventFilter{Limit: 1000}, true)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, ev := range events {
		ids[ev.EventID()] = true
	}
	return ids
}

func TestRunDeletesOldMessagesOnly(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateDatabase(t, dbType)
		defer closeDB()

		now := time.Now()
		alice := test.NewUser(t)
		withPolicy := test.NewRoom(t, alice)
		withPolicy.CreateAndInsert(t, alice, "m.room.retention", map[string]any{"max_lifetime": (24 * time.Hour).Milliseconds()}, test.WithStateKey(""))
		oldMsg := withPolicy.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "old"}, test.WithTimestamp(now.Add(-48*time.Hour)))
		oldTopic := withPolicy.CreateAndInsert(t, alice, "m.room.topic", map[string]any{"topic": "old topic"}, test.WithStateKey(""), test.WithTimestamp(now.Add(-47*time.Hour)))
		newMsg := withPolicy.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "new"}, test.WithTimestamp(now.Add(-time.Hour)))

		noPolicy := test.NewRoom(t, alice)
		keptMsg := noPolicy.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "kept"}, test.WithTimestamp(now.Add(-400*24*time.Hour)))

		writeEvents(t, db, withPolicy.Events())
		writeEvents(t, db, noPolicy.Events())

		pruner := &fakePruner{}
		cfg := &config.MessageRetention{Enabled: true}
		n, err := retention.Run(context.Background(), cfg, db, pruner, nil, now)
		assert.NoError(t, err)
		assert.Equal(t, 1, n, "only the old message of the room with a policy")
		assert.Equal(t, []string{oldMsg.EventID()}, pruner.pruned[withPolicy.ID])
		assert.Empty(t, pruner.pruned[noPolicy.ID])

		got := remaining(t, db, withPolicy.ID)
		assert.False(t, got[oldMsg.EventID()], "the old message is gone from the timeline")
		assert.True(t, got[newMsg.EventID()])
		assert.True(t, got[oldTopic.EventID()], "state events stay")
		assert.True(t, remaining(t, db, noPolicy.ID)[keptMsg.EventID()], "a room without policy is untouched")

		// A second run has nothing left to do.
		n, err = retention.Run(context.Background(), cfg, db, pruner, nil, now)
		assert.NoError(t, err)
		assert.Equal(t, 0, n)

		// The default policy reaches the room without an event, and the limits move the lifetime of the other.
		cfg = &config.MessageRetention{
			Enabled:       true,
			DefaultPolicy: config.RetentionPolicy{MaxLifetime: 30 * 24 * time.Hour},
		}
		n, err = retention.Run(context.Background(), cfg, db, pruner, nil, now)
		assert.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.False(t, remaining(t, db, noPolicy.ID)[keptMsg.EventID()])

		cfg.AllowedLifetimeMin = 7 * 24 * time.Hour
		n, err = retention.Run(context.Background(), cfg, db, pruner, nil, now.Add(-40*time.Hour))
		assert.NoError(t, err)
		assert.Equal(t, 0, n, "a lifetime of a day is raised to a week, and nothing is a week old")
		assert.True(t, remaining(t, db, withPolicy.ID)[newMsg.EventID()])
	})
}

func TestRunKeepsMessagesWhenTheRoomserverFails(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateDatabase(t, dbType)
		defer closeDB()

		now := time.Now()
		alice := test.NewUser(t)
		room := test.NewRoom(t, alice)
		room.CreateAndInsert(t, alice, "m.room.retention", map[string]any{"max_lifetime": (24 * time.Hour).Milliseconds()}, test.WithStateKey(""))
		old := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "old"}, test.WithTimestamp(now.Add(-48*time.Hour)))
		writeEvents(t, db, room.Events())

		failing := &fakePruner{err: assert.AnError}
		n, err := retention.Run(context.Background(), &config.MessageRetention{Enabled: true}, db, failing, nil, now)
		assert.NoError(t, err, "a failing room is logged and the run goes on")
		assert.Equal(t, 0, n)
		assert.True(t, remaining(t, db, room.ID)[old.EventID()], "the sync API keeps what the roomserver did not prune, so the next run finds it again")
	})
}
