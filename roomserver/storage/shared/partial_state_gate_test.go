package shared_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/roomserver/storage/shared"
	"github.com/element-hq/dendrite/roomserver/storage/tables"
	"github.com/element-hq/dendrite/roomserver/types"
)

type countingPartialTable struct {
	tables.PartialStateRooms
	rows       []types.PartialState
	selects    int
	selectsAll int
}

func (c *countingPartialTable) SelectPartialState(context.Context, *sql.Tx, types.RoomNID) (*types.PartialState, error) {
	c.selects++
	if len(c.rows) > 0 {
		return &c.rows[0], nil
	}
	return nil, nil
}

func (c *countingPartialTable) SelectAllPartialState(context.Context, *sql.Tx) ([]types.PartialState, error) {
	c.selectsAll++
	return c.rows, nil
}

// Every event asks whether its room has partial state. A server that never joins rooms that way, and has none from
// before, must not pay a query for it.
func TestPartialStateOfRoomIsNotAskedWhenNoRoomCanBePartial(t *testing.T) {
	ctx := context.Background()

	none := &countingPartialTable{}
	d := &shared.Database{PartialStateTable: none}
	for i := 0; i < 5; i++ {
		got, err := d.PartialStateOfRoom(ctx, types.RoomNID(i+1))
		require.NoError(t, err)
		assert.Nil(t, got)
	}
	assert.Equal(t, 0, none.selects, "no query for a room")
	assert.Equal(t, 1, none.selectsAll, "one look at the table, when the first room is asked about")

	some := &countingPartialTable{rows: []types.PartialState{{RoomNID: 1, JoinEventID: "$j"}}}
	d = &shared.Database{PartialStateTable: some}
	got, err := d.PartialStateOfRoom(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, some.selects, "a room that is partial from before the start is found")
}
