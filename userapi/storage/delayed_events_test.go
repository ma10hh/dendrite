package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/userapi/api"
)

func TestDelayedEventsStorage(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateUserDatabase(t, dbType)
		defer closeDB()

		empty, key := "", "_@a:x_DEV"
		events := []api.DelayedEvent{
			{DelayID: "one", UserID: "@a:x", DeviceID: "D", RoomID: "!r:x", EventType: "m.room.topic", StateKey: &empty, Content: []byte(`{"topic":"t"}`), DelayMS: 5000, RunningSinceMS: 1000},
			{DelayID: "two", UserID: "@a:x", DeviceID: "D", RoomID: "!r:x", EventType: "org.matrix.msc3401.call.member", StateKey: &key, Content: []byte(`{}`), DelayMS: 1000, RunningSinceMS: 1000},
			{DelayID: "three", UserID: "@a:x", DeviceID: "D", RoomID: "!r:x", EventType: "m.room.message", Content: []byte(`{"body":"b"}`), DelayMS: 100, RunningSinceMS: 1000},
			{DelayID: "other", UserID: "@b:x", DeviceID: "D", RoomID: "!r:x", EventType: "m.room.topic", StateKey: &empty, Content: []byte(`{}`), DelayMS: 100, RunningSinceMS: 1000},
		}
		for _, e := range events {
			require.NoError(t, db.InsertDelayedEvent(ctx, e))
		}

		got, err := db.DelayedEvent(ctx, "one")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, events[0], *got)
		got, err = db.DelayedEvent(ctx, "three")
		require.NoError(t, err)
		assert.Nil(t, got.StateKey, "a message has no state key, which is not the same as an empty one")
		got, err = db.DelayedEvent(ctx, "missing")
		require.NoError(t, err)
		assert.Nil(t, got)

		mine, err := db.DelayedEventsForUser(ctx, "@a:x")
		require.NoError(t, err)
		var ids []string
		for _, e := range mine {
			ids = append(ids, e.DelayID)
		}
		assert.Equal(t, []string{"three", "two", "one"}, ids, "the next to be sent first")
		n, err := db.CountDelayedEvents(ctx, "@a:x")
		require.NoError(t, err)
		assert.Equal(t, 3, n)
		all, err := db.AllDelayedEvents(ctx)
		require.NoError(t, err)
		assert.Len(t, all, 4)

		found, err := db.RestartDelayedEvent(ctx, "one", 9999)
		require.NoError(t, err)
		assert.True(t, found)
		got, _ = db.DelayedEvent(ctx, "one")
		assert.EqualValues(t, 9999, got.RunningSinceMS)
		assert.EqualValues(t, 5000, got.DelayMS, "only the start changes")
		found, err = db.RestartDelayedEvent(ctx, "missing", 1)
		require.NoError(t, err)
		assert.False(t, found)

		removed, err := db.DeleteDelayedStateEvents(ctx, "!r:x", "m.room.topic", "")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"one", "other"}, removed, "every user's delayed event for that state")
		removed, err = db.DeleteDelayedStateEvents(ctx, "!r:x", "m.room.message", "")
		require.NoError(t, err)
		assert.Empty(t, removed, "an event that is not a state event is never matched")

		found, err = db.DeleteDelayedEvent(ctx, "two")
		require.NoError(t, err)
		assert.True(t, found)
		found, err = db.DeleteDelayedEvent(ctx, "two")
		require.NoError(t, err)
		assert.False(t, found)
	})
}
