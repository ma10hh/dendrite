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
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
)

func TestPartialStateTable(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		connStr, close := test.PrepareDBConnectionString(t, dbType)
		defer close()
		db, err := sqlutil.Open(&config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}, sqlutil.NewExclusiveWriter())
		require.NoError(t, err)
		var tab tables.PartialStateRooms
		switch dbType {
		case test.DBTypePostgres:
			require.NoError(t, postgres.CreatePartialStateTable(db))
			tab, err = postgres.PreparePartialStateTable(db)
		case test.DBTypeSQLite:
			require.NoError(t, sqlite3.CreatePartialStateTable(db))
			tab, err = sqlite3.PreparePartialStateTable(db)
		}
		require.NoError(t, err)

		none, err := tab.SelectPartialState(ctx, nil, 1)
		require.NoError(t, err)
		assert.Nil(t, none, "a room whose state is whole has no record")

		second := types.PartialState{RoomNID: 2, JoinEventID: "$second", Servers: []string{"a.example", "b.example"}, JoinedAt: 2000}
		first := types.PartialState{RoomNID: 1, JoinEventID: "$first", Servers: []string{"c.example"}, JoinedAt: 1000}
		require.NoError(t, tab.InsertPartialState(ctx, nil, second))
		require.NoError(t, tab.InsertPartialState(ctx, nil, first))

		got, err := tab.SelectPartialState(ctx, nil, 2)
		require.NoError(t, err)
		assert.Equal(t, &second, got)

		all, err := tab.SelectAllPartialState(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, []types.PartialState{first, second}, all, "the oldest join comes first")

		// Marking a room again replaces what is known.
		again := types.PartialState{RoomNID: 2, JoinEventID: "$again", Servers: []string{"d.example"}, JoinedAt: 3000}
		require.NoError(t, tab.InsertPartialState(ctx, nil, again))
		got, err = tab.SelectPartialState(ctx, nil, 2)
		require.NoError(t, err)
		assert.Equal(t, &again, got)

		require.NoError(t, tab.DeletePartialState(ctx, nil, 1))
		all, err = tab.SelectAllPartialState(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, []types.PartialState{again}, all)
		require.NoError(t, tab.DeletePartialState(ctx, nil, 1), "deleting what is not there is not an error")
	})
}
