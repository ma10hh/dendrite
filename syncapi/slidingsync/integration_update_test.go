// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/types"
	"github.com/element-hq/dendrite/test"
)

var wakeCounter atomic.Int64

// wake tells the notifier that something changed for the user, as the consumers do after storing events.
func (hn *harness) wake() {
	hn.notifier.OnNewEvent(nil, "", []string{hn.alice.ID}, types.StreamingToken{PDUPosition: types.StreamPosition(1 + wakeCounter.Add(1))})
}

// postWait sends a request that may wait for changes for up to timeoutMS.
func (hn *harness) postWait(ctx context.Context, pos string, timeoutMS int, body string) (int, gjson.Result) {
	hn.t.Helper()
	target := fmt.Sprintf("/sync?timeout=%d", timeoutMS)
	if pos != "" {
		target += "&pos=" + url.QueryEscape(pos)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)).WithContext(ctx)
	res := hn.handle.OnIncomingRequest(req, hn.device)
	return res.Code, gjson.Parse(mustJSON(hn.t, res.JSON))
}

func (hn *harness) room(res gjson.Result, roomID string) gjson.Result {
	return res.Get("rooms").Map()[roomID]
}

func TestIncrementalTimelineUpdates(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		busy, quiet := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		message(t, busy, hn.alice, "b1")
		message(t, quiet, hn.alice, "q1")
		hn.write(busy.Events()...)
		hn.write(quiet.Events()...)
		req := listReq("[[0,9]]", 3, "")

		first := hn.mustPost("", req)
		assertRooms(t, first, busy.ID, quiet.ID)

		// One new event: only that room comes back, with just that event, and nothing that did not change.
		fresh := message(t, busy, hn.alice, "b2")
		hn.write(fresh)
		second := hn.mustPost(first.Get("pos").String(), req)
		assertRooms(t, second, busy.ID)
		got := hn.room(second, busy.ID)
		if ids := eventIDs(got, "timeline"); len(ids) != 1 || ids[0] != fresh.EventID() {
			t.Errorf("timeline = %v, want only the new event", ids)
		}
		if got.Get("initial").Exists() || got.Get("num_live").Int() != 1 || got.Get("limited").Exists() {
			t.Errorf("an update must not be initial, and must count its live event: %s", got.Raw)
		}
		for _, unchanged := range []string{"name", "avatar", "joined_count", "invited_count", "heroes", "notification_count", "required_state", "is_dm"} {
			if got.Get(unchanged).Exists() {
				t.Errorf("%s did not change but was sent: %s", unchanged, got.Raw)
			}
		}
		if got.Get("bump_stamp").Int() <= 0 {
			t.Errorf("a new message moves bump_stamp: %s", got.Raw)
		}

		// Nothing happened since.
		third := hn.mustPost(second.Get("pos").String(), req)
		assertRooms(t, third)

		// More new events than the limit: the newest ones, flagged as a gap that can be paginated.
		var latest []string
		for i := 0; i < 5; i++ {
			ev := message(t, quiet, hn.alice, fmt.Sprintf("q%d", i+2))
			hn.write(ev)
			latest = append(latest, ev.EventID())
		}
		fourth := hn.mustPost(third.Get("pos").String(), req)
		assertRooms(t, fourth, quiet.ID)
		got = hn.room(fourth, quiet.ID)
		if ids := eventIDs(got, "timeline"); fmt.Sprint(ids) != fmt.Sprint(latest[2:]) {
			t.Errorf("timeline = %v, want the newest three %v", ids, latest[2:])
		}
		if !got.Get("limited").Bool() || got.Get("prev_batch").String() == "" || got.Get("num_live").Int() != 3 {
			t.Errorf("a gap must be reported as limited with a prev_batch: %s", got.Raw)
		}
	})
}

func TestIncrementalRoomAttributes(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room := test.NewRoom(t, hn.alice)
		state(t, room, hn.alice, spec.MRoomName, "", map[string]string{"name": "Before"})
		state(t, room, hn.alice, spec.MRoomTopic, "", map[string]string{"topic": "Old topic"})
		hn.write(room.Events()...)
		req := listReq("[[0,9]]", 0, `,"required_state":[["m.room.name",""],["m.room.topic",""]]`)

		first := hn.mustPost("", req)
		if got := stateIDs(hn.room(first, room.ID)); len(got) != 2 {
			t.Fatalf("first required_state = %v", got)
		}
		pos := first.Get("pos").String()
		next := func(events ...*rstypes.HeaderedEvent) gjson.Result {
			t.Helper()
			hn.write(events...)
			res := hn.mustPost(pos, req)
			pos = res.Get("pos").String()
			return hn.room(res, room.ID)
		}

		// Only the changed state event is sent, and a changed name is sent as the name.
		got := next(state(t, room, hn.alice, spec.MRoomTopic, "", map[string]string{"topic": "New topic"}))
		if ids := stateIDs(got); len(ids) != 1 || ids[0] != "m.room.topic|" {
			t.Errorf("required_state = %v, want only the changed topic", ids)
		}
		got = next(state(t, room, hn.alice, spec.MRoomName, "", map[string]string{"name": "After"}))
		if got.Get("name").String() != "After" || len(stateIDs(got)) != 1 {
			t.Errorf("a rename must send the name and the name event only: %s", got.Raw)
		}

		// Avatars: set, then removed, which is expressed as an explicit null.
		got = next(state(t, room, hn.alice, "m.room.avatar", "", map[string]string{"url": "mxc://test/a"}))
		if got.Get("avatar").String() != "mxc://test/a" {
			t.Errorf("avatar = %s", got.Raw)
		}
		got = next(state(t, room, hn.alice, "m.room.avatar", "", map[string]string{}))
		if av := got.Get("avatar"); !av.Exists() || av.Type != gjson.Null {
			t.Errorf("a removed avatar must be an explicit null: %s", got.Raw)
		}

		// A member joining changes the counts, and only the counts.
		got = next(join(t, room, bob, "Bob"))
		if got.Get("joined_count").Int() != 2 || got.Get("invited_count").Int() != 0 {
			t.Errorf("member counts: %s", got.Raw)
		}
		if got.Get("name").Exists() || got.Get("heroes").Exists() {
			t.Errorf("a named room does not get its name or heroes again: %s", got.Raw)
		}
	})
}

func TestHeroesChangeInUnnamedRooms(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob, carol := test.NewUser(t), test.NewUser(t)
		room := test.NewRoom(t, hn.alice)
		join(t, room, bob, "Bob")
		hn.write(room.Events()...)

		first := hn.mustPost("", listReq("[[0,9]]", 0, ""))
		if n := len(hn.room(first, room.ID).Get("heroes").Array()); n != 1 {
			t.Fatalf("heroes = %d, want 1", n)
		}
		hn.write(join(t, room, carol, "Carol"))
		got := hn.room(hn.mustPost(first.Get("pos").String(), listReq("[[0,9]]", 0, "")), room.ID)
		if n := len(got.Get("heroes").Array()); n != 2 {
			t.Errorf("a new member must be sent as a hero of an unnamed room: %s", got.Raw)
		}
	})
}

func TestNotificationCountsChangeWithoutEvents(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)
		req := listReq("[[0,9]]", 5, "")

		first := hn.mustPost("", req)
		if _, err := hn.db.UpsertRoomUnreadNotificationCounts(bg, hn.alice.ID, room.ID, 4, 1); err != nil {
			t.Fatal(err)
		}
		second := hn.mustPost(first.Get("pos").String(), req)
		got := hn.room(second, room.ID)
		if got.Get("notification_count").Int() != 4 || got.Get("highlight_count").Int() != 1 {
			t.Fatalf("counts must be sent when they change: %s", got.Raw)
		}
		if got.Get("timeline").Exists() || got.Get("joined_count").Exists() {
			t.Errorf("only the counts changed: %s", got.Raw)
		}

		// Reading the room clears them, which is also a change.
		if _, err := hn.db.UpsertRoomUnreadNotificationCounts(bg, hn.alice.ID, room.ID, 0, 0); err != nil {
			t.Fatal(err)
		}
		third := hn.room(hn.mustPost(second.Get("pos").String(), req), room.ID)
		if !third.Get("notification_count").Exists() || third.Get("notification_count").Int() != 0 {
			t.Errorf("cleared counts must be sent as zero: %s", third.Raw)
		}
	})
}

func TestDirectMessageFlagChanges(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)
		req := listReq("[[0,9]]", 0, "")

		first := hn.mustPost("", req)
		hn.users.direct = map[string][]string{"@someone:test": {room.ID}}
		second := hn.mustPost(first.Get("pos").String(), req)
		if got := hn.room(second, room.ID); !got.Get("is_dm").Exists() || !got.Get("is_dm").Bool() {
			t.Fatalf("becoming a DM must be sent: %s", got.Raw)
		}
		hn.users.direct = nil
		third := hn.mustPost(second.Get("pos").String(), req)
		if got := hn.room(third, room.ID); !got.Get("is_dm").Exists() || got.Get("is_dm").Bool() {
			t.Errorf("no longer being a DM must be sent as false: %s", got.Raw)
		}
	})
}

func TestTimelineLimitExpansion(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		for i := 0; i < 4; i++ {
			message(t, room, hn.alice, fmt.Sprintf("m%d", i))
		}
		hn.write(room.Events()...)

		first := hn.mustPost("", listReq("[[0,9]]", 1, ""))
		if n := len(hn.room(first, room.ID).Get("timeline").Array()); n != 1 {
			t.Fatalf("timeline = %d events, want 1", n)
		}

		// Asking for more history returns the room again at once, with the larger timeline, marked as expanded.
		second := hn.mustPost(first.Get("pos").String(), listReq("[[0,9]]", 4, ""))
		got := hn.room(second, room.ID)
		if n := len(got.Get("timeline").Array()); n != 4 {
			t.Fatalf("expanded timeline = %d events, want 4: %s", n, got.Raw)
		}
		if !got.Get("unstable_expanded_timeline").Bool() || got.Get("num_live").Int() != 0 || got.Get("initial").Exists() {
			t.Errorf("an expanded timeline has no live events and is not initial: %s", got.Raw)
		}

		// Asking for the same again, or for less, returns nothing.
		assertRooms(t, hn.mustPost(second.Get("pos").String(), listReq("[[0,9]]", 4, "")))
	})
}

func TestRoomsMovingInAndOutOfTheWindow(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		top, other := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		message(t, top, hn.alice, "t1")
		message(t, other, hn.alice, "o1")
		hn.write(other.Events()...)
		hn.write(top.Events()...) // top is the newest
		req := listReq("[[0,0]]", 5, "")

		first := hn.mustPost("", req)
		assertRooms(t, first, top.ID)

		// While other is not in the window, it gets events. The window moves to it when it becomes the newest.
		var missed []string
		for i := 0; i < 2; i++ {
			ev := message(t, other, hn.alice, fmt.Sprintf("o%d", i+2))
			hn.write(ev)
			missed = append(missed, ev.EventID())
		}
		second := hn.mustPost(first.Get("pos").String(), req)
		assertRooms(t, second, other.ID)
		got := hn.room(second, other.ID)
		if !got.Get("initial").Bool() {
			t.Errorf("a room entering the window for the first time is initial: %s", got.Raw)
		}

		// top falls out of the window and is quiet: nothing is sent for it. When it gets an event while out of
		// the window and then comes back, only what it missed is sent, as an update.
		ev := message(t, top, hn.alice, "t2")
		hn.write(ev)
		third := hn.mustPost(second.Get("pos").String(), req)
		assertRooms(t, third, top.ID) // top is newest again, so it is back in the window
		got = hn.room(third, top.ID)
		if got.Get("initial").Exists() || fmt.Sprint(eventIDs(got, "timeline")) != fmt.Sprint([]string{ev.EventID()}) {
			t.Errorf("a returning room gets what it missed, not everything again: %s", got.Raw)
		}
		_ = missed
	})
}

func TestInviteBecomesJoinedRoom(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		bob := test.NewUser(t)
		room := test.NewRoom(t, bob)
		invite := membership(t, room, bob, hn.alice, spec.Invite)
		hn.invite(invite)
		req := listReq("[[0,9]]", 5, "")

		first := hn.mustPost("", req)
		if !hn.room(first, room.ID).Get("invite_state").Exists() {
			t.Fatalf("the invite must be listed: %s", first.Raw)
		}
		// The invite does not repeat.
		second := hn.mustPost(first.Get("pos").String(), req)
		assertRooms(t, second)

		// Accepting it makes it a joined room, sent in full.
		joined := join(t, room, hn.alice, "")
		hn.write(room.Events()...)
		if _, err := hn.db.RetireInviteEvent(bg, invite.EventID()); err != nil {
			t.Fatal(err)
		}
		_ = joined
		third := hn.mustPost(second.Get("pos").String(), req)
		got := hn.room(third, room.ID)
		if !got.Get("initial").Bool() || got.Get("invite_state").Exists() || len(got.Get("timeline").Array()) == 0 {
			t.Errorf("a joined room replaces its invite: %s", got.Raw)
		}
	})
}

func TestSelfLeftRoomIsReportedOnceIfItWasSent(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)
		req := listReq("[[0,9]]", 5, "")

		first := hn.mustPost("", req)
		assertRooms(t, first, room.ID)

		leave := membership(t, room, hn.alice, hn.alice, spec.Leave)
		hn.write(leave)
		second := hn.mustPost(first.Get("pos").String(), req)
		got := hn.room(second, room.ID)
		ids := eventIDs(got, "timeline")
		if len(ids) == 0 || ids[len(ids)-1] != leave.EventID() {
			t.Fatalf("the client must learn the room was left, with the leave last: %s", got.Raw)
		}
		if got.Get("required_state").Exists() || got.Get("name").Exists() {
			t.Errorf("no state is sent for a left room: %s", got.Raw)
		}

		assertRooms(t, hn.mustPost(second.Get("pos").String(), req))
	})
}

func TestLongPollReturnsWhenSomethingHappens(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)
		req := listReq("[[0,9]]", 5, "")
		first := hn.mustPost("", req)

		type result struct {
			res     gjson.Result
			elapsed time.Duration
		}
		done := make(chan result, 1)
		go func() {
			start := time.Now()
			_, res := hn.postWait(context.Background(), first.Get("pos").String(), 20000, req)
			done <- result{res, time.Since(start)}
		}()

		select {
		case <-done:
			t.Fatal("the request must wait while nothing changes")
		case <-time.After(300 * time.Millisecond):
		}

		ev := message(t, room, hn.alice, "wake up")
		hn.write(ev)
		hn.wake()

		select {
		case r := <-done:
			if r.elapsed > 10*time.Second {
				t.Errorf("took %s, the wake-up was ignored", r.elapsed)
			}
			if ids := eventIDs(hn.room(r.res, room.ID), "timeline"); len(ids) != 1 || ids[0] != ev.EventID() {
				t.Errorf("the woken request must carry the new event: %s", r.res.Raw)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the request did not return after a change")
		}
	})
}

func TestLongPollTimesOutEmpty(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)
		req := listReq("[[0,9]]", 5, "")
		first := hn.mustPost("", req)

		start := time.Now()
		done := make(chan gjson.Result, 1)
		go func() {
			_, res := hn.postWait(context.Background(), first.Get("pos").String(), 600, req)
			done <- res
		}()
		// Unrelated wake-ups, such as typing in another room, must not end the wait early.
		time.Sleep(100 * time.Millisecond)
		hn.wake()
		res := <-done
		if elapsed := time.Since(start); elapsed < 550*time.Millisecond {
			t.Errorf("returned after %s, before the timeout", elapsed)
		}
		assertRooms(t, res)
		if res.Get("pos").String() == "" || res.Get("pos").String() == first.Get("pos").String() {
			t.Errorf("an empty response still moves pos on: %s", res.Raw)
		}
		if res.Get("lists.all.count").Int() != 1 {
			t.Errorf("an empty response still carries the list counts: %s", res.Raw)
		}
	})
}

func TestTimeoutIsIgnoredWhenStartingASession(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		start := time.Now()
		code, _ := hn.postWait(context.Background(), "", 20000, listReq("[[0,9]]", 5, ""))
		if code != http.StatusOK || time.Since(start) > 5*time.Second {
			t.Errorf("a first request must be answered at once, got %d after %s", code, time.Since(start))
		}
	})
}

func TestCancelledLongPollLosesNothing(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		room := test.NewRoom(t, hn.alice)
		hn.write(room.Events()...)
		req := listReq("[[0,9]]", 5, "")
		first := hn.mustPost("", req)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() {
			code, _ := hn.postWait(ctx, first.Get("pos").String(), 20000, req)
			done <- code
		}()
		time.Sleep(200 * time.Millisecond)
		cancel()
		if code := <-done; code != http.StatusRequestTimeout {
			t.Fatalf("a cancelled request got HTTP %d, want 408", code)
		}

		// The client retries the same pos. Nothing was consumed, so an event written meanwhile is delivered.
		ev := message(t, room, hn.alice, "while nobody was waiting")
		hn.write(ev)
		res := hn.mustPost(first.Get("pos").String(), req)
		if ids := eventIDs(hn.room(res, room.ID), "timeline"); len(ids) != 1 || ids[0] != ev.EventID() {
			t.Errorf("the retry must receive the event: %s", res.Raw)
		}
	})
}

func TestTimeoutValidation(t *testing.T) {
	h := newTestHandler()
	for _, bad := range []string{"abc", "-1", "1.5"} {
		req := httptest.NewRequest(http.MethodPost, "/sync?timeout="+bad, strings.NewReader(`{}`))
		if res := h.OnIncomingRequest(req, testDevice); res.Code != http.StatusBadRequest {
			t.Errorf("timeout=%s: got HTTP %d, want 400", bad, res.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/sync?timeout=99999999999", strings.NewReader(`{}`))
	if d, err := parseTimeout(req); err != nil || d != maxTimeout {
		t.Errorf("a huge timeout is capped at %s, got %s (%v)", maxTimeout, d, err)
	}
}
