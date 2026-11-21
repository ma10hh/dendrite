package eventutil

import (
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
)

func TestCheckUntrustedEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		version gomatrixserverlib.RoomVersion
		valid   bool
	}{
		"fine":            {`{"type":"m.room.message","room_id":"!r:example.com","sender":"@a:example.com","origin_server_ts":1,"content":{},"auth_events":[],"prev_events":[],"depth":1,"event_id":"$x:example.com"}`, gomatrixserverlib.RoomVersionV2, true},
		"empty parts":     {`{"type":"m.room.message","room_id":"!:","sender":"@a:example.com","origin_server_ts":1,"content":{},"auth_events":[],"prev_events":[],"depth":1,"event_id":"$x:example.com"}`, gomatrixserverlib.RoomVersionV2, false},
		"no domain":       {`{"type":"m.room.message","room_id":"!abc:","sender":"@a:example.com","origin_server_ts":1,"content":{},"auth_events":[],"prev_events":[],"depth":1}`, gomatrixserverlib.RoomVersionV10, false},
		"no room":         {`{"type":"m.room.message","sender":"@a:example.com","origin_server_ts":1,"content":{},"auth_events":[],"prev_events":[],"depth":1}`, gomatrixserverlib.RoomVersionV10, false},
		"room not string": {`{"type":"m.room.message","room_id":5,"sender":"@a:example.com","origin_server_ts":1,"content":{},"auth_events":[],"prev_events":[],"depth":1}`, gomatrixserverlib.RoomVersionV10, false},
	} {
		t.Run(name, func(t *testing.T) {
			ev, err := gomatrixserverlib.MustGetRoomVersion(tc.version).NewEventFromUntrustedJSON([]byte(tc.raw))
			if err != nil {
				if tc.valid {
					t.Fatalf("parsing: %v", err)
				}
				return // the library already refuses it
			}
			if got := CheckUntrustedEvent(ev); (got == nil) != tc.valid {
				t.Errorf("CheckUntrustedEvent = %v, want valid=%v", got, tc.valid)
			}
		})
	}
}
