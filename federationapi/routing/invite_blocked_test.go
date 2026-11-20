package routing

import (
	"context"
	"net/http"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/test"
)

type blockedRoomserver struct {
	api.FederationRoomserverAPI
	blocked bool
	invited int
}

func (r *blockedRoomserver) QueryRoomBlocked(context.Context, string) (bool, error) {
	return r.blocked, nil
}

func (r *blockedRoomserver) HandleInvite(context.Context, *types.HeaderedEvent) error {
	r.invited++
	return nil
}

func TestInviteToBlockedRoom(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	var invite gomatrixserverlib.PDU
	for _, ev := range room.Events() {
		invite = ev.PDU
	}
	require.NotNil(t, invite)

	rs := &blockedRoomserver{blocked: true}
	_, res := handleInviteResult(context.Background(), invite, nil, rs)
	require.NotNil(t, res)
	assert.Equal(t, http.StatusForbidden, res.Code)
	assert.Zero(t, rs.invited, "the roomserver is not told of the invite")

	rs.blocked = false
	ev, res := handleInviteResult(context.Background(), invite, nil, rs)
	assert.Nil(t, res)
	assert.NotNil(t, ev)
	assert.Equal(t, 1, rs.invited)
}
