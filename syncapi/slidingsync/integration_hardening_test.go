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
	"strings"
	"testing"
	"time"

	"github.com/element-hq/dendrite/test"
)

func TestRequestSizeLimits(t *testing.T) {
	h := newTestHandler()
	ranges := strings.TrimSuffix(strings.Repeat("[0,1],", maxRangesPerList+1), ",")
	pairs := strings.TrimSuffix(strings.Repeat(`["m.room.name",""],`, maxRequiredState+1), ",")
	names := strings.TrimSuffix(strings.Repeat(`"a",`, maxExtensionScope+1), ",")

	for name, body := range map[string]string{
		"too many ranges":                  fmt.Sprintf(`{"lists":{"a":{"ranges":[%s],"timeline_limit":1}}}`, ranges),
		"too much required_state in list":  fmt.Sprintf(`{"lists":{"a":{"ranges":[[0,1]],"timeline_limit":1,"required_state":[%s]}}}`, pairs),
		"too much required_state in sub":   fmt.Sprintf(`{"room_subscriptions":{"!r:x":{"timeline_limit":1,"required_state":[%s]}}}`, pairs),
		"extension names too many lists":   fmt.Sprintf(`{"extensions":{"typing":{"enabled":true,"lists":[%s]}}}`, names),
		"extension names too many rooms":   fmt.Sprintf(`{"extensions":{"receipts":{"enabled":true,"rooms":[%s]}}}`, names),
		"account data names too many both": fmt.Sprintf(`{"extensions":{"account_data":{"enabled":true,"lists":[%s],"rooms":[%s]}}}`, names, names),
	} {
		req := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(body))
		if res := h.OnIncomingRequest(req, testDevice); res.Code != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", name, res.Code)
		}
	}

	// Limits are generous: a normal request is untouched.
	normal := `{"lists":{"a":{"ranges":[[0,19],[40,59]],"timeline_limit":1,"required_state":[["m.room.name",""],["m.room.member","$LAZY"]]}}}`
	if res := h.OnIncomingRequest(httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(normal)), testDevice); res.Code != http.StatusOK {
		t.Errorf("a normal request got HTTP %d", res.Code)
	}
}

func TestVanishedRoomsAreForgottenByTheConnection(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		keep, gone := test.NewRoom(t, hn.alice), test.NewRoom(t, hn.alice)
		hn.write(keep.Events()...)
		hn.write(gone.Events()...)
		req := listReq("[[0,9]]", 1, "")

		first := hn.mustPost("", req)
		assertRooms(t, first, keep.ID, gone.ID)
		sessionID, _, _ := ParsePos(first.Get("pos").String())
		conn := hn.handle.store.Get(hn.alice.ID, hn.device.ID, "", sessionID)
		if len(conn.sent) != 2 {
			t.Fatalf("the connection remembers %d rooms, want 2", len(conn.sent))
		}

		if err := hn.db.PurgeRoom(bg, gone.ID); err != nil {
			t.Fatal(err)
		}
		second := hn.mustPost(first.Get("pos").String(), req)
		assertRooms(t, second)
		if _, stillThere := conn.sent[gone.ID]; stillThere || len(conn.sent) != 1 {
			t.Errorf("a purged room must be forgotten, the connection still remembers %d rooms", len(conn.sent))
		}
		if second.Get("lists.all.count").Int() != 1 {
			t.Errorf("count = %d, want 1", second.Get("lists.all.count").Int())
		}
	})
}

func TestShutdownEndsWaitingRequests(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		hn := newHarness(t, dbType)
		hn.write(test.NewRoom(t, hn.alice).Events()...)
		req := listReq("[[0,9]]", 1, "")
		first := hn.mustPost("", req)

		done := make(chan int, 1)
		go func() {
			code, _ := hn.postWait(context.Background(), first.Get("pos").String(), 20000, req)
			done <- code
		}()
		time.Sleep(200 * time.Millisecond)
		hn.stopServer()

		select {
		case code := <-done:
			if code != http.StatusServiceUnavailable {
				t.Errorf("got HTTP %d, want 503 for a request that was waiting at shutdown", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a waiting request kept the server from shutting down")
		}
	})
}

func TestMetricsCanBeRegisteredRepeatedly(t *testing.T) {
	store := NewConnectionStore(time.Hour, 5)
	registerMetrics(store)
	registerMetrics(NewConnectionStore(time.Hour, 5)) // the second call must not panic
}
