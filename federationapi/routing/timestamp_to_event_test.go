package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/roomserver/api"
)

type timestampRoomserver struct {
	api.FederationRoomserverAPI
	servers map[spec.ServerName]bool // servers that are in the room; the empty name is this server
	eventID string
	ts      int64
	lastTS  int64
	forward bool
}

func (r *timestampRoomserver) QueryServerJoinedToRoom(_ context.Context, req *api.QueryServerJoinedToRoomRequest, res *api.QueryServerJoinedToRoomResponse) error {
	res.IsInRoom = r.servers[req.ServerName]
	return nil
}

func (r *timestampRoomserver) QueryEventAtTimestamp(_ context.Context, _ string, ts int64, forward bool) (string, int64, error) {
	r.lastTS, r.forward = ts, forward
	if r.eventID == "" {
		return "", 0, api.ErrNoEventAtTimestamp
	}
	return r.eventID, r.ts, nil
}

func TestTimestampToEventFederation(t *testing.T) {
	const roomID = "!room:example.org"
	call := func(rs *timestampRoomserver, origin spec.ServerName, query string) (int, any) {
		httpReq := httptest.NewRequest(http.MethodGet, "/timestamp_to_event/"+roomID+query, nil)
		fedReq := fclient.NewFederationRequest(http.MethodGet, origin, "example.org", httpReq.URL.RequestURI())
		res := TimestampToEvent(httpReq, &fedReq, rs, roomID)
		return res.Code, res.JSON
	}

	rs := &timestampRoomserver{servers: map[spec.ServerName]bool{"": true, "other.org": true}, eventID: "$found", ts: 1234}
	code, body := call(rs, "other.org", "?ts=1000&dir=f")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, httputil.TimestampToEventResponse{EventID: "$found", OriginServerTS: 1234}, body)
	assert.True(t, rs.forward)
	assert.EqualValues(t, 1000, rs.lastTS)

	call(rs, "other.org", "?ts=1000&dir=b")
	assert.False(t, rs.forward)

	code, _ = call(rs, "stranger.org", "?ts=1000&dir=f")
	assert.Equal(t, http.StatusForbidden, code, "a server that is not in the room gets nothing")

	code, _ = call(&timestampRoomserver{servers: map[spec.ServerName]bool{"other.org": true}}, "other.org", "?ts=1&dir=f")
	assert.Equal(t, http.StatusNotFound, code, "this server is not in the room")

	code, _ = call(&timestampRoomserver{servers: rs.servers}, "other.org", "?ts=1&dir=f")
	assert.Equal(t, http.StatusNotFound, code, "no event")

	for _, q := range []string{"", "?ts=1", "?dir=f", "?ts=x&dir=f", "?ts=1&dir=up"} {
		code, _ = call(rs, "other.org", q)
		assert.Equal(t, http.StatusBadRequest, code, q)
	}
}
