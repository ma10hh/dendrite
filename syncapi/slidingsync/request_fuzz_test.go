package slidingsync

import (
	"encoding/json"
	"testing"
)

// FuzzRequest runs a request body through what the handler does with it before it touches any data: decoding,
// validation, the extensions, the position token and the matcher built from required_state. A client can send
// anything, so none of it may panic.
func FuzzRequest(f *testing.F) {
	f.Add([]byte(`{"conn_id":"a","lists":{"all":{"ranges":[[0,19]],"timeline_limit":1,"required_state":[["m.room.name",""],["*","*"],["m.room.member","$LAZY"]],"filters":{"is_dm":true,"room_types":[null,"m.space"],"spaces":["!s:x"],"tags":["u.a"]}}},"room_subscriptions":{"!r:x":{"timeline_limit":5,"required_state":[["m.room.member","$ME"]]}},"extensions":{"to_device":{"enabled":true,"since":"5","limit":10},"e2ee":{"enabled":true},"typing":{"enabled":true,"lists":["*"],"rooms":["*"]}}}`))
	f.Add([]byte(`{"lists":{"x":{"ranges":[[-1,9223372036854775807],[5,2]]}},"extensions":{"to_device":{"limit":-3,"since":"-1"},"receipts":{"lists":null}}}`))
	f.Add([]byte(`{"extensions":{"account_data":5},"lists":null,"room_subscriptions":{"":{}}}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var req Request
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		_ = validate(&req)
		for _, list := range req.Lists {
			m := newStateMatcher(list.RequiredState, "@me:example.com")
			_ = m.matches("m.room.member", "@me:example.com")
			_ = m.needsAllState()
			_ = m.wildcardKeyTypes()
			_ = m.exactPairs()
			_ = m.coversAllMembers()
		}
		for _, sub := range req.RoomSubscriptions {
			m := newStateMatcher(sub.RequiredState, "@me:example.com")
			_ = m.exactPairs()
		}
		out, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("a decoded request cannot be encoded: %v", err)
		}
		var again Request
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatalf("an encoded request cannot be decoded: %v", err)
		}
	})
}

func FuzzParsePos(f *testing.F) {
	f.Add("abc/12")
	f.Add("/5")
	f.Add("a/-1")
	f.Add("a/99999999999999999999")
	f.Fuzz(func(t *testing.T, pos string) {
		if session, seq, err := ParsePos(pos); err == nil && (session == "" || seq != seq) {
			t.Fatalf("ParsePos(%q) = %q, %d", pos, session, seq)
		}
	})
}
