package tables_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/storage/postgres"
	"github.com/element-hq/dendrite/roomserver/storage/sqlite3"
	"github.com/element-hq/dendrite/roomserver/storage/tables"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
)

func TestBlockedRoomsTable(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		connStr, close := test.PrepareDBConnectionString(t, dbType)
		defer close()
		db, err := sqlutil.Open(&config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}, sqlutil.NewExclusiveWriter())
		require.NoError(t, err)
		var tab tables.BlockedRooms
		switch dbType {
		case test.DBTypePostgres:
			require.NoError(t, postgres.CreateBlockedRoomsTable(db))
			tab, err = postgres.PrepareBlockedRoomsTable(db)
		case test.DBTypeSQLite:
			require.NoError(t, sqlite3.CreateBlockedRoomsTable(db))
			tab, err = sqlite3.PrepareBlockedRoomsTable(db)
		}
		require.NoError(t, err)

		blocked, err := tab.IsRoomBlocked(ctx, nil, "!a:test")
		require.NoError(t, err)
		assert.False(t, blocked)

		require.NoError(t, tab.InsertBlockedRoom(ctx, nil, "!a:test"))
		require.NoError(t, tab.InsertBlockedRoom(ctx, nil, "!a:test"), "blocking twice is not an error")
		blocked, err = tab.IsRoomBlocked(ctx, nil, "!a:test")
		require.NoError(t, err)
		assert.True(t, blocked)
		blocked, err = tab.IsRoomBlocked(ctx, nil, "!b:test")
		require.NoError(t, err)
		assert.False(t, blocked, "only the blocked room is blocked")

		require.NoError(t, tab.DeleteBlockedRoom(ctx, nil, "!a:test"))
		blocked, err = tab.IsRoomBlocked(ctx, nil, "!a:test")
		require.NoError(t, err)
		assert.False(t, blocked)
		require.NoError(t, tab.DeleteBlockedRoom(ctx, nil, "!a:test"), "lifting what is not there is not an error")
	})
}
