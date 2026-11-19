// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/syncapi/types"
	"github.com/element-hq/dendrite/test"
)

// withExtensions is a request for one list of all rooms plus the given extensions.
func withExtensions(limit int, extensions string) string {
	return fmt.Sprintf(`{"lists":{"all":{"ranges":[[0,9]],"timeline_limit":%d}},"extensions":%s}`, limit, extensions)
}

// sendToDevice stores a to-device message for the device and announces it, as the consumer does.
func (hn *harness) sendToDevice(sender *test.User, n int) types.StreamPosition {
	hn.t.Helper()
	pos, err := hn.db.StoreNewSendForDeviceMessage(bg, hn.alice.ID, hn.device.ID, gomatrixserverlib.SendToDeviceEvent{
		Sender: sender.ID, Type: "m.room_key", Content: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n)),
	})
	if err != nil {
		hn.t.Fatalf("StoreNewSendForDeviceMessage: %s", err)
	}
	hn.notifier.OnNewSendToDevice(hn.alice.ID, []string{hn.device.ID}, types.StreamingToken{SendToDevicePosition: pos})
	return pos
}

func (hn *harness) accountDataChanged(roomID, dataType string) {
	hn.t.Helper()
	pos, err := hn.db.UpsertAccountData(bg, hn.alice.ID, roomID, dataType)
	if err != nil {
		hn.t.Fatalf("UpsertAccountData: %s", err)
	}
	hn.notifier.OnNewAccountData(hn.alice.ID, types.StreamingToken{AccountDataPosition: pos})
}

func (hn *harness) receipt(room *test.Room, user *test.User, receiptType string, eventID string) {
	hn.t.Helper()
	pos, err := hn.db.StoreReceipt(bg, room.ID, receiptType, user.ID, eventID, spec.Timestamp(1234))
	if err != nil {
		hn.t.Fatalf("StoreReceipt: %s", err)
	}
	hn.notifier.OnNewReceipt(room.ID, types.StreamingToken{ReceiptPosition: pos})
}

// onOtherConnection makes a request use its own connection, so that it does not replace the one under test.
func onOtherConnection(body string) string {
	return strings.Replace(body, `{"lists"`, `{"conn_id":"other","lists"`, 1)
}

func toDeviceNumbers(res gjson.Result) []int64 {
	var out []int64
	for _, ev := range res.Get("extensions.to_device.events").Array() {
		out = append(out, ev.Get("content.n").Int())
	}
	return out
}

func TestExtensionsAreOffByDefault(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		hn.write(test.NewRoom(t, hn.alice).Events()...)

		for name, body := range map[string]string{
			"no extensions":  listReq("[[0,9]]", 0, ""),
			"all disabled":   withExtensions(0, `{"to_device":{"enabled":false},"e2ee":{},"account_data":{"enabled":null},"receipts":{},"typing":{}}`),
			"unknown, other": withExtensions(0, `{"io.example.thing":{"enabled":true}}`),
		} {
			if res := hn.mustPost("", body); res.Get("extensions").Exists() {
				t.Errorf("%s: extensions must be absent when nothing is enabled: %s", name, res.Raw)
			}
		}
	})
}

func TestExtensionConfigIsValidated(t *testing.T) {
	h := newTestHandler()
	for name, body := range map[string]string{
		"since is not a number": `{"extensions":{"to_device":{"enabled":true,"since":"abc"}}}`,
		"since is negative":     `{"extensions":{"to_device":{"enabled":true,"since":"-1"}}}`,
		"limit is negative":     `{"extensions":{"to_device":{"enabled":true,"limit":-5}}}`,
		"wrong type":            `{"extensions":{"typing":{"enabled":"yes"}}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(body))
		if res := h.OnIncomingRequest(req, testDevice); res.Code != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", name, res.Code)
		}
	}
}

func TestToDeliveryLimitAndAcknowledgement(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		hn.sendToDevice(bob, 1)
		p2 := hn.sendToDevice(bob, 2)
		p3 := hn.sendToDevice(bob, 3)

		// The limit holds messages back, and next_batch points at the last one returned.
		first := hn.mustPost("", withExtensions(0, `{"to_device":{"enabled":true,"limit":2}}`))
		if got := toDeviceNumbers(first); !reflect.DeepEqual(got, []int64{1, 2}) {
			t.Fatalf("first batch = %v, want [1 2]", got)
		}
		if got := first.Get("extensions.to_device.next_batch").String(); got != strconv.FormatInt(int64(p2), 10) {
			t.Errorf("next_batch = %q, want %d", got, p2)
		}

		// Sending it back as `since` continues after it and acknowledges what came before.
		second := hn.mustPost(first.Get("pos").String(), withExtensions(0, fmt.Sprintf(`{"to_device":{"enabled":true,"since":"%d"}}`, p2)))
		if got := toDeviceNumbers(second); !reflect.DeepEqual(got, []int64{3}) {
			t.Fatalf("second batch = %v, want [3]", got)
		}
		restarted := hn.mustPost("", onOtherConnection(withExtensions(0, `{"to_device":{"enabled":true}}`)))
		if got := toDeviceNumbers(restarted); !reflect.DeepEqual(got, []int64{3}) {
			t.Errorf("acknowledged messages must be gone, a new session got %v", got)
		}

		// With nothing left the extension is still there, so that the client has a token to send back.
		third := hn.mustPost(second.Get("pos").String(), withExtensions(0, fmt.Sprintf(`{"to_device":{"enabled":true,"since":"%d"}}`, p3)))
		if !third.Get("extensions.to_device").Exists() || len(toDeviceNumbers(third)) != 0 {
			t.Errorf("an empty to_device must still be present: %s", third.Raw)
		}
		if got := third.Get("extensions.to_device.next_batch").String(); got != strconv.FormatInt(int64(p3), 10) {
			t.Errorf("next_batch = %q, want %d", got, p3)
		}
		if got := toDeviceNumbers(hn.mustPost("", onOtherConnection(withExtensions(0, `{"to_device":{"enabled":true}}`)))); len(got) != 0 {
			t.Errorf("everything was acknowledged, but a new session got %v", got)
		}
	})
}

func TestToDeviceDropsIgnoredSendersButAcknowledgesThem(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		friend, pest := test.NewUser(t), test.NewUser(t)
		if err := hn.db.UpdateIgnoresForUser(bg, hn.alice.ID, &types.IgnoredUsers{List: map[string]interface{}{pest.ID: map[string]any{}}}); err != nil {
			t.Fatal(err)
		}
		hn.sendToDevice(pest, 1)
		last := hn.sendToDevice(friend, 2)
		hn.sendToDevice(pest, 3)

		res := hn.mustPost("", withExtensions(0, `{"to_device":{"enabled":true}}`))
		if got := toDeviceNumbers(res); !reflect.DeepEqual(got, []int64{2}) {
			t.Errorf("messages = %v, want only the one from the user who is not ignored", got)
		}
		if next := res.Get("extensions.to_device.next_batch").Int(); next < int64(last) {
			t.Errorf("next_batch %d must move past the ignored messages too", next)
		}
	})
}

func TestToDeviceLimitZeroAndCap(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		for i := 1; i <= 3; i++ {
			hn.sendToDevice(bob, i)
		}
		res := hn.mustPost("", withExtensions(0, `{"to_device":{"enabled":true,"limit":0}}`))
		if len(toDeviceNumbers(res)) != 0 || !res.Get("extensions.to_device.next_batch").Exists() {
			t.Errorf("limit 0 returns no messages but keeps the extension: %s", res.Raw)
		}
		res = hn.mustPost("", withExtensions(0, `{"to_device":{"enabled":true,"limit":1000000}}`))
		if got := toDeviceNumbers(res); len(got) != 3 {
			t.Errorf("a huge limit is capped, not rejected: %v", got)
		}
	})
}

func TestToDeviceMessageWakesALongPoll(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		body := withExtensions(0, `{"to_device":{"enabled":true}}`)
		first := hn.mustPost("", body)

		done := make(chan gjson.Result, 1)
		go func() {
			_, res := hn.postWait(context.Background(), first.Get("pos").String(), 20000,
				withExtensions(0, fmt.Sprintf(`{"to_device":{"enabled":true,"since":"%s"}}`, first.Get("extensions.to_device.next_batch").String())))
			done <- res
		}()
		select {
		case <-done:
			t.Fatal("the request must wait while there is nothing to deliver")
		case <-time.After(300 * time.Millisecond):
		}

		hn.sendToDevice(bob, 7)
		select {
		case res := <-done:
			if got := toDeviceNumbers(res); !reflect.DeepEqual(got, []int64{7}) {
				t.Errorf("woken with %v, want [7]", got)
			}
			if res.Get("rooms").Exists() {
				t.Errorf("a response with only extension data has no rooms: %s", res.Raw)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a to-device message did not end the long poll")
		}
	})
}

func TestE2EEKeyCountsAndKeyChanges(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		carol, stranger := test.NewUser(t), test.NewUser(t)
		shared := test.NewRoom(t, hn.alice)
		join(t, shared, carol, "")
		hn.write(shared.Events()...)
		hn.rs.members = map[string][]string{shared.ID: {hn.alice.ID, carol.ID}}
		hn.users.otk = map[string]int{"signed_curve25519": 7}
		hn.users.fallback = []string{"signed_curve25519"}
		body := withExtensions(0, `{"e2ee":{"enabled":true}}`)

		// The first response has the key counts but cannot say what changed before it.
		first := hn.mustPost("", body)
		e2ee := first.Get("extensions.e2ee")
		if e2ee.Get("device_one_time_keys_count.signed_curve25519").Int() != 7 || e2ee.Get("device_unused_fallback_key_types.0").String() != "signed_curve25519" {
			t.Errorf("key counts: %s", e2ee.Raw)
		}
		if e2ee.Get("device_lists").Exists() {
			t.Errorf("device_lists cannot be known on the first response: %s", e2ee.Raw)
		}

		// A user we share a room with changed their keys; one we do not share a room with is not reported.
		hn.users.keyChanges, hn.users.keyOffset = []string{carol.ID, stranger.ID}, 5
		hn.notifier.OnNewKeyChange(types.StreamingToken{DeviceListPosition: 5}, hn.alice.ID, carol.ID)
		second := hn.mustPost(first.Get("pos").String(), body)
		if got := second.Get("extensions.e2ee.device_lists.changed").Array(); len(got) != 1 || got[0].String() != carol.ID {
			t.Errorf("changed = %v, want only %s: %s", got, carol.ID, second.Raw)
		}
		if second.Get("extensions.e2ee.device_one_time_keys_count.signed_curve25519").Int() != 7 {
			t.Errorf("key counts are sent with every response: %s", second.Raw)
		}

		// Nothing new since.
		third := hn.mustPost(second.Get("pos").String(), body)
		if third.Get("extensions.e2ee.device_lists").Exists() {
			t.Errorf("the same change must not be reported twice: %s", third.Raw)
		}
	})
}

func TestE2EEDeviceListsFollowMembership(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob, carol := test.NewUser(t), test.NewUser(t)
		old := test.NewRoom(t, hn.alice)
		join(t, old, carol, "")
		hn.write(old.Events()...)
		hn.rs.members = map[string][]string{old.ID: {hn.alice.ID, carol.ID}}
		body := withExtensions(0, `{"e2ee":{"enabled":true}}`)
		first := hn.mustPost("", body)

		// Joining a room with somebody new makes them a user whose devices must be fetched.
		fresh := test.NewRoom(t, bob)
		membership(t, fresh, bob, hn.alice, spec.Invite)
		join(t, fresh, hn.alice, "")
		hn.write(fresh.Events()...)
		hn.rs.members[fresh.ID] = []string{bob.ID, hn.alice.ID}
		second := hn.mustPost(first.Get("pos").String(), body)
		changed := second.Get("extensions.e2ee.device_lists.changed").Array()
		if len(changed) != 1 || changed[0].String() != bob.ID {
			t.Errorf("changed = %v, want only the new user %s: %s", changed, bob.ID, second.Raw)
		}

		// Leaving the only room shared with somebody means their devices no longer need tracking.
		hn.write(membership(t, old, hn.alice, hn.alice, spec.Leave))
		hn.rs.members[old.ID] = []string{carol.ID}
		third := hn.mustPost(second.Get("pos").String(), body)
		left := third.Get("extensions.e2ee.device_lists.left").Array()
		if len(left) != 1 || left[0].String() != carol.ID {
			t.Errorf("left = %v, want %s: %s", left, carol.ID, third.Raw)
		}
	})
}

func TestKeyCountsAloneDoNotEndALongPoll(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		hn.users.otk = map[string]int{"signed_curve25519": 3}
		body := withExtensions(0, `{"e2ee":{"enabled":true}}`)
		first := hn.mustPost("", body)

		start := time.Now()
		_, res := hn.postWait(context.Background(), first.Get("pos").String(), 500, body)
		if time.Since(start) < 450*time.Millisecond {
			t.Errorf("returned after %s: key counts alone must not end the poll", time.Since(start))
		}
		if res.Get("extensions.e2ee.device_one_time_keys_count.signed_curve25519").Int() != 3 {
			t.Errorf("the response at the timeout still carries the counts: %s", res.Raw)
		}
	})
}

func TestAccountDataExtension(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		visible, hidden := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		hn.write(visible.Events()...)
		hn.write(hidden.Events()...)
		hn.users.global = map[string]json.RawMessage{"m.push_rules": json.RawMessage(`{"global":{}}`)}
		hn.users.room = map[string]map[string]json.RawMessage{
			visible.ID: {"m.tag": json.RawMessage(`{"tags":{"u.work":{}}}`)},
			hidden.ID:  {"m.tag": json.RawMessage(`{"tags":{"m.favourite":{}}}`)},
		}
		hn.accountDataChanged("", "m.push_rules")
		hn.accountDataChanged(visible.ID, "m.tag")
		hn.accountDataChanged(hidden.ID, "m.tag")

		// Only the rooms in the window are covered, so the other room's data is held back.
		body := `{"lists":{"all":{"ranges":[[0,0]],"timeline_limit":1}},"extensions":{"account_data":{"enabled":true}}}`
		first := hn.mustPost("", body)
		if first.Get("extensions.account_data.global.0.type").String() != "m.push_rules" {
			t.Errorf("global account data is sent on the first response: %s", first.Raw)
		}
		rooms := first.Get("extensions.account_data.rooms").Map()
		if len(rooms) != 1 {
			t.Fatalf("account data of %d rooms, want only the one in the response: %s", len(rooms), first.Raw)
		}
		returned := roomIDs(first)
		for id := range returned {
			if got := rooms[id].Get("0.type").String(); got != "m.tag" {
				t.Errorf("room account data for %s = %s", id, rooms[id].Raw)
			}
		}

		// Nothing changed.
		second := hn.mustPost(first.Get("pos").String(), body)
		if got := second.Get("extensions.account_data"); !got.Exists() || got.Get("global").Exists() || got.Get("rooms").Exists() {
			t.Errorf("unchanged data is not repeated, but the extension stays present: %s", second.Raw)
		}

		// A change is sent once, and without the data that did not change.
		hn.users.global = map[string]json.RawMessage{"m.push_rules": json.RawMessage(`{"global":{"changed":true}}`), "m.other": json.RawMessage(`{}`)}
		hn.accountDataChanged("", "m.other")
		third := hn.mustPost(second.Get("pos").String(), body)
		if got := third.Get("extensions.account_data.global").Array(); len(got) != 1 || got[0].Get("type").String() != "m.other" {
			t.Errorf("only the changed data is sent: %s", third.Raw)
		}
	})
}

func TestAccountDataOfARoomNotInTheResponseArrivesWhenItAppears(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)
		hn.users.room = map[string]map[string]json.RawMessage{room.ID: {"m.tag": json.RawMessage(`{"tags":{}}`)}}
		body := withExtensions(1, `{"account_data":{"enabled":true}}`)
		first := hn.mustPost("", body)

		// The room's account data changes while the room has nothing else to report: nothing is sent.
		hn.accountDataChanged(room.ID, "m.tag")
		second := hn.mustPost(first.Get("pos").String(), body)
		assertRooms(t, second)
		if second.Get("extensions.account_data.rooms").Exists() {
			t.Fatalf("a room that is not in the response gets no account data: %s", second.Raw)
		}

		// When the room is next in a response, the change comes with it.
		hn.write(message(t, room, hn.alice, "now it is news"))
		third := hn.mustPost(second.Get("pos").String(), body)
		if got := third.Get("extensions.account_data.rooms").Map()[room.ID]; got.Get("0.type").String() != "m.tag" {
			t.Errorf("held-back account data was lost: %s", third.Raw)
		}
	})
}

func TestReceiptsExtension(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob, dave, pest := test.NewUser(t), test.NewUser(t), test.NewUser(t)
		if err := hn.db.UpdateIgnoresForUser(bg, hn.alice.ID, &types.IgnoredUsers{List: map[string]interface{}{pest.ID: map[string]any{}}}); err != nil {
			t.Fatal(err)
		}
		room := test.NewRoom(t, hn.alice)
		join(t, room, bob, "")
		first := message(t, room, hn.alice, "one")
		hn.write(room.Events()...)
		hn.receipt(room, bob, "m.read", first.EventID())
		hn.receipt(room, dave, "m.read.private", first.EventID()) // somebody else's private receipt: never sent
		hn.receipt(room, hn.alice, "m.read.private", first.EventID())
		hn.receipt(room, pest, "m.read", first.EventID())
		body := withExtensions(1, `{"receipts":{"enabled":true}}`)

		// A room sent in full gets its receipts. Others' private receipts and ignored users' are left out.
		res := hn.mustPost("", body)
		users := res.Get("extensions.receipts.rooms").Map()[room.ID].Get("content").Get(first.EventID() + `.m\.read`).Map()
		if _, ok := users[bob.ID]; !ok || len(users) != 2 {
			t.Errorf("receipt users = %v, want bob and alice's own private receipt, and not dave's: %s", users, res.Raw)
		}
		if got := res.Get("extensions.receipts.rooms").Map()[room.ID].Get("type").String(); got != spec.MReceipt {
			t.Errorf("type = %q", got)
		}

		// Later only new receipts are sent, and only along with a room that is in the response.
		second := message(t, room, hn.alice, "two")
		hn.write(second)
		hn.receipt(room, bob, "m.read", second.EventID())
		next := hn.mustPost(res.Get("pos").String(), body)
		content := next.Get("extensions.receipts.rooms").Map()[room.ID].Get("content")
		if len(content.Map()) != 1 || !content.Get(second.EventID()).Exists() {
			t.Errorf("only the new receipt is sent: %s", next.Raw)
		}
		quiet := hn.mustPost(next.Get("pos").String(), body)
		if got := quiet.Get("extensions.receipts"); !got.Exists() || got.Get("rooms").Exists() {
			t.Errorf("nothing new, but receipts were sent again: %s", quiet.Raw)
		}
	})
}

func TestTypingExtension(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room, idle := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		join(t, room, bob, "")
		hn.write(room.Events()...)
		hn.write(idle.Events()...)
		body := withExtensions(1, `{"typing":{"enabled":true}}`)

		typed := hn.typing.AddTypingUser(bob.ID, room.ID, nil)
		hn.notifier.OnNewTyping(room.ID, types.StreamingToken{TypingPosition: types.StreamPosition(typed)})
		res := hn.mustPost("", body)
		event := res.Get("extensions.typing.rooms").Map()[room.ID]
		if event.Get("type").String() != spec.MTyping || event.Get("content.user_ids.0").String() != bob.ID {
			t.Errorf("a room sent in full gets who is typing now: %s", res.Raw)
		}
		if _, ok := res.Get("extensions.typing.rooms").Map()[idle.ID]; ok {
			t.Errorf("nobody has typed in the other room: %s", res.Raw)
		}

		// Typing stops: the room reports an empty list, once it is in a response again.
		stopped := hn.typing.RemoveUser(bob.ID, room.ID)
		hn.notifier.OnNewTyping(room.ID, types.StreamingToken{TypingPosition: types.StreamPosition(stopped)})
		hn.write(message(t, room, hn.alice, "typing is over"))
		next := hn.mustPost(res.Get("pos").String(), body)
		event = next.Get("extensions.typing.rooms").Map()[room.ID]
		if !event.Exists() || len(event.Get("content.user_ids").Array()) != 0 {
			t.Errorf("an empty list tells the client typing stopped: %s", next.Raw)
		}
	})
}

func TestExtensionScoping(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		a, b := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		hn.write(a.Events()...)
		hn.write(b.Events()...)
		hn.users.room = map[string]map[string]json.RawMessage{
			a.ID: {"m.tag": json.RawMessage(`{}`)}, b.ID: {"m.tag": json.RawMessage(`{}`)},
		}
		hn.accountDataChanged(a.ID, "m.tag")
		hn.accountDataChanged(b.ID, "m.tag")

		body := func(scope string) string {
			return fmt.Sprintf(`{"lists":{"one":{"ranges":[[0,0]],"timeline_limit":0},"two":{"ranges":[[1,1]],"timeline_limit":0}},
				"extensions":{"account_data":{"enabled":true%s}}}`, scope)
		}
		rooms := func(res gjson.Result) int { return len(res.Get("extensions.account_data.rooms").Map()) }

		for name, tc := range map[string]struct {
			scope string
			want  int
		}{
			"default is every room in the response": {``, 2},
			"every room explicitly":                 {`,"rooms":["*"]`, 2},
			"no rooms and no lists":                 {`,"rooms":[],"lists":[]`, 0},
			"only one list":                         {`,"rooms":[],"lists":["one"]`, 1},
			"both lists by name":                    {`,"rooms":[],"lists":["one","two"]`, 2},
			"an unknown list":                       {`,"rooms":[],"lists":["nope"]`, 0},
			"a named room":                          {fmt.Sprintf(`,"rooms":[%q],"lists":[]`, a.ID), 1},
			"a room that is not in the response":    {`,"rooms":["!elsewhere:test"],"lists":[]`, 0},
		} {
			if got := rooms(hn.mustPost("", body(tc.scope))); got != tc.want {
				t.Errorf("%s: account data for %d rooms, want %d", name, got, tc.want)
			}
		}
	})
}
