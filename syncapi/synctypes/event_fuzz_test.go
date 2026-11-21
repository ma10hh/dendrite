package synctypes_test

import (
	"encoding/json"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/internal/eventutil"
	"github.com/element-hq/dendrite/syncapi/synctypes"
)

// FuzzUntrustedEvent feeds what a remote server could send as an event through what this server does with one it
// accepts: parsing for every room version, redacting, reading the fields the event auth and the consumers read, the
// headered form the roomserver stores, and the conversion for clients. Nothing may panic.
func FuzzUntrustedEvent(f *testing.F) {
	f.Add([]byte(`{"type":"m.room.message","room_id":"!r:example.com","sender":"@a:example.com","origin_server_ts":1,"content":{"body":"hi"},"auth_events":[],"prev_events":[],"depth":1,"hashes":{"sha256":"x"},"signatures":{}}`))
	f.Add([]byte(`{"type":"m.room.member","state_key":"@a:example.com","room_id":"!r:example.com","sender":"@a:example.com","origin_server_ts":1,"content":{"membership":"join","join_authorised_via_users_server":"@b:example.com"},"auth_events":["$a"],"prev_events":["$b"],"depth":2,"unsigned":{"age":1,"replaces_state":"$c","m.relations":{}}}`))
	f.Add([]byte(`{"type":"m.room.create","state_key":"","room_id":"!r:example.com","sender":"@a:example.com","origin_server_ts":1,"content":{"room_version":"11","creator":"@a:example.com"},"auth_events":[],"prev_events":[],"depth":1}`))
	f.Add([]byte(`{"type":"m.room.redaction","room_id":"!r:example.com","sender":"@a:example.com","redacts":"$x","content":{"redacts":"$x"},"origin_server_ts":9007199254740993,"depth":-1}`))
	f.Add([]byte(`{"type":"m.room.power_levels","state_key":"","content":{"users":{"@a:example.com":"100"},"events":{"x":1.5},"ban":null},"sender":"@a:example.com","room_id":"!r:example.com"}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"type":1}`))

	versions := gomatrixserverlib.RoomVersions()
	userID := func(_ spec.RoomID, sender spec.SenderID) (*spec.UserID, error) {
		return spec.NewUserID(string(sender), true)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for version := range versions {
			ev, err := gomatrixserverlib.MustGetRoomVersion(version).NewEventFromUntrustedJSON(data)
			if err != nil {
				continue
			}
			// What the server does first with an event from another server: the library accepts room IDs that it then panics on.
			if eventutil.CheckUntrustedEvent(ev) != nil {
				continue
			}
			_ = ev.EventID()
			_ = ev.AuthEventIDs()
			_ = ev.PrevEventIDs()
			_ = ev.Redacts()
			_ = ev.Content()
			_ = ev.Unsigned()
			_ = ev.JSON()
			_ = ev.Depth()
			_ = ev.OriginServerTS()
			_ = ev.StateKey()
			_ = ev.SenderID()
			if headered, err := ev.ToHeaderedJSON(); err == nil {
				back, err := gomatrixserverlib.NewEventFromHeaderedJSON(headered, false)
				if err != nil {
					t.Fatalf("an event cannot be read back from its headered form: %v", err)
				}
				if back.EventID() != ev.EventID() {
					t.Fatalf("event ID changed in the round trip: %s != %s", back.EventID(), ev.EventID())
				}
			}
			if ce, err := synctypes.ToClientEvent(ev, synctypes.FormatAll, userID); err == nil {
				if _, err := json.Marshal(ce); err != nil {
					t.Fatalf("a client event cannot be encoded: %v", err)
				}
			}
			_ = synctypes.ToClientEvents([]gomatrixserverlib.PDU{ev}, synctypes.FormatSync, userID)
			ev.Redact()
			_ = ev.JSON()
		}
	})
}
