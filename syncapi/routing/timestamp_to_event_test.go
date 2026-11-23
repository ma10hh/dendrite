package routing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/test"
)

func (h *threadsHarness) messageAt(at time.Time, vis gomatrixserverlib.HistoryVisibility) *rstypes.HeaderedEvent {
	h.t.Helper()
	ev := h.room.CreateAndInsert(h.t, h.alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "x"}, test.WithTimestamp(at))
	h.write(ev, vis)
	return ev
}

func (h *threadsHarness) timestampToEvent(query string) (int, gjson.Result) {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+h.room.ID+"/timestamp_to_event"+query, nil)
	res := TimestampToEvent(req, h.device, h.db, h.rs, h.room.ID)
	b, err := json.Marshal(res.JSON)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Code, gjson.ParseBytes(b)
}

func TestTimestampToEvent(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newThreadsHarness(t, dbType)
		base := time.Now().Add(time.Hour).Truncate(time.Second)
		m1 := h.messageAt(base, gomatrixserverlib.HistoryVisibilityShared)
		m2 := h.messageAt(base.Add(time.Hour), gomatrixserverlib.HistoryVisibilityShared)
		m3 := h.messageAt(base.Add(2*time.Hour), gomatrixserverlib.HistoryVisibilityShared)
		at := func(d time.Duration) string { return strconv.FormatInt(base.Add(d).UnixMilli(), 10) }

		for _, tc := range []struct {
			name  string
			query string
			want  *rstypes.HeaderedEvent
			code  int
		}{
			{"forward between two events finds the later", "?dir=f&ts=" + at(30*time.Minute), m2, http.StatusOK},
			{"backward between two events finds the earlier", "?dir=b&ts=" + at(90*time.Minute), m2, http.StatusOK},
			{"forward at the exact time finds that event", "?dir=f&ts=" + at(time.Hour), m2, http.StatusOK},
			{"backward at the exact time finds that event", "?dir=b&ts=" + at(2*time.Hour), m3, http.StatusOK},
			{"forward after the last event", "?dir=f&ts=" + at(3*time.Hour), nil, http.StatusNotFound},
			{"backward before the first event", "?dir=b&ts=1", nil, http.StatusNotFound},
			{"no ts", "?dir=f", nil, http.StatusBadRequest},
			{"no dir", "?ts=1", nil, http.StatusBadRequest},
			{"bad dir", "?ts=1&dir=x", nil, http.StatusBadRequest},
			{"bad ts", "?ts=yesterday&dir=f", nil, http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, res := h.timestampToEvent(tc.query)
				if code != tc.code {
					t.Fatalf("got %d %s, want %d", code, res.Raw, tc.code)
				}
				if tc.want != nil {
					if got := res.Get("event_id").String(); got != tc.want.EventID() {
						t.Errorf("event %s, want %s", got, tc.want.EventID())
					}
					if got := res.Get("origin_server_ts").Int(); got != int64(tc.want.OriginServerTS()) {
						t.Errorf("origin_server_ts %d, want %d", got, tc.want.OriginServerTS())
					}
				}
			})
		}
		_ = m1

		t.Run("a user that is not in the room and cannot read it", func(t *testing.T) {
			h.rs.inRoom, h.rs.beenInRoom = false, false
			defer func() { h.rs.inRoom, h.rs.beenInRoom = true, true }()
			if code, res := h.timestampToEvent("?dir=f&ts=1"); code != http.StatusForbidden {
				t.Errorf("got %d %s, want 403", code, res.Raw)
			}
		})
		t.Run("a room that does not exist", func(t *testing.T) {
			h.rs.roomExists = false
			defer func() { h.rs.roomExists = true }()
			if code, res := h.timestampToEvent("?dir=f&ts=1"); code != http.StatusForbidden {
				t.Errorf("got %d %s, want 403", code, res.Raw)
			}
		})
	})
}

func TestTimestampToEventSkipsWhatTheUserMaySee(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newThreadsHarness(t, dbType)
		base := time.Now().Add(time.Hour).Truncate(time.Second)
		hidden := h.messageAt(base, gomatrixserverlib.HistoryVisibilityJoined)
		visible := h.messageAt(base.Add(time.Hour), gomatrixserverlib.HistoryVisibilityJoined)
		h.rs.membershipAt = func(id string) string {
			if id == hidden.EventID() {
				return spec.Leave
			}
			return spec.Join
		}
		code, res := h.timestampToEvent("?dir=f&ts=" + strconv.FormatInt(base.UnixMilli(), 10))
		if code != http.StatusOK || res.Get("event_id").String() != visible.EventID() {
			t.Errorf("got %d %s, want the event the user may see (%s)", code, res.Raw, visible.EventID())
		}
		// Looking back from the hidden event ends at an event before it, which is the creation of the room.
		code, res = h.timestampToEvent("?dir=b&ts=" + strconv.FormatInt(base.UnixMilli(), 10))
		if id := res.Get("event_id").String(); code != http.StatusOK || id == hidden.EventID() {
			t.Errorf("got %d %s, want an event that is not the hidden one", code, res.Raw)
		}
	})
}
