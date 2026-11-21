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
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"

	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

var testDevice = &userapi.Device{UserID: "@alice:test", ID: "DEVICE"}

func do(t *testing.T, h *Handler, pos, body string) (code int, resp *Response, errCode spec.MatrixErrorCode) {
	t.Helper()
	target := "/sync"
	if pos != "" {
		target += "?pos=" + url.QueryEscape(pos)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	res := h.OnIncomingRequest(req, testDevice)
	switch j := res.JSON.(type) {
	case *Response:
		return res.Code, j, ""
	case spec.MatrixError:
		return res.Code, nil, j.ErrCode
	default:
		t.Fatalf("unexpected response body type %T", res.JSON)
		return
	}
}

func newTestHandler() *Handler {
	return &Handler{
		store:   NewConnectionStore(time.Hour, 5),
		userAPI: &fakeUserAPI{},
		fill: func(context.Context, *userapi.Device, *Connection, *Request, *Response, types.StreamingToken) (func(), error) {
			return func() {}, nil
		},
	}
}

func TestInitialRequestStartsSession(t *testing.T) {
	h := newTestHandler()
	code, resp, _ := do(t, h, "", `{}`)
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if _, seq, err := ParsePos(resp.Pos); err != nil || seq != 1 {
		t.Fatalf("pos = %q (seq %d, err %v), want sequence 1", resp.Pos, seq, err)
	}
}

func TestRequestsAdvancePos(t *testing.T) {
	h := newTestHandler()
	_, first, _ := do(t, h, "", `{}`)
	code, second, _ := do(t, h, first.Pos, `{}`)
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if second.Pos == first.Pos {
		t.Error("pos must change between responses")
	}
	if _, seq, _ := ParsePos(second.Pos); seq != 2 {
		t.Errorf("sequence = %d, want 2", seq)
	}
}

func TestRetryReplaysLastResponse(t *testing.T) {
	h := newTestHandler()
	_, first, _ := do(t, h, "", `{}`)
	_, second, _ := do(t, h, first.Pos, `{}`)

	// The client never saw `second` and retries with `first.Pos`.
	code, replay, _ := do(t, h, first.Pos, `{}`)
	if code != http.StatusOK || replay.Pos != second.Pos {
		t.Fatalf("retry got (%d, %q), want the original response with pos %q", code, replay.Pos, second.Pos)
	}
	// ...and then continues from `second`.
	if code, _, _ := do(t, h, second.Pos, `{}`); code != http.StatusOK {
		t.Fatalf("continuing after a retry failed with %d", code)
	}
}

func TestStalePosIsRejected(t *testing.T) {
	h := newTestHandler()
	_, first, _ := do(t, h, "", `{}`)
	_, second, _ := do(t, h, first.Pos, `{}`)
	_, _, _ = do(t, h, second.Pos, `{}`)

	// first.Pos is now two responses behind: it cannot be replayed.
	code, _, errCode := do(t, h, first.Pos, `{}`)
	if code != http.StatusBadRequest || errCode != ErrorUnknownPos {
		t.Fatalf("got (%d, %q), want 400 M_UNKNOWN_POS", code, errCode)
	}
}

func TestUnknownSessionIsRejected(t *testing.T) {
	h := newTestHandler()
	code, _, errCode := do(t, h, "deadbeef/1", `{}`)
	if code != http.StatusBadRequest || errCode != ErrorUnknownPos {
		t.Fatalf("got (%d, %q), want 400 M_UNKNOWN_POS", code, errCode)
	}
}

func TestZeroSequencePosIsNotTreatedAsMissing(t *testing.T) {
	h := newTestHandler()
	_, first, _ := do(t, h, "", `{}`)
	_, _, _ = do(t, h, first.Pos, `{}`)
	sessionID, _, _ := ParsePos(first.Pos)

	code, _, errCode := do(t, h, sessionID+"/0", `{}`)
	if code != http.StatusBadRequest || errCode != ErrorUnknownPos {
		t.Fatalf("got (%d, %q), want 400 M_UNKNOWN_POS", code, errCode)
	}
}

func TestMalformedPosIsRejected(t *testing.T) {
	h := newTestHandler()
	code, _, errCode := do(t, h, "garbage", `{}`)
	if code != http.StatusBadRequest || errCode != spec.ErrorInvalidParam {
		t.Fatalf("got (%d, %q), want 400 M_INVALID_PARAM", code, errCode)
	}
}

func TestRequestWithoutPosRestartsSession(t *testing.T) {
	h := newTestHandler()
	_, old, _ := do(t, h, "", `{"conn_id":"main"}`)
	_, fresh, _ := do(t, h, "", `{"conn_id":"main"}`)

	if code, _, errCode := do(t, h, old.Pos, `{"conn_id":"main"}`); code != http.StatusBadRequest || errCode != ErrorUnknownPos {
		t.Fatalf("old session must be gone, got (%d, %q)", code, errCode)
	}
	if code, _, _ := do(t, h, fresh.Pos, `{"conn_id":"main"}`); code != http.StatusOK {
		t.Fatalf("new session must work, got %d", code)
	}
}

func TestConnectionsAreScopedByConnID(t *testing.T) {
	h := newTestHandler()
	_, main, _ := do(t, h, "", `{"conn_id":"main"}`)
	_, _, _ = do(t, h, "", `{"conn_id":"other"}`)

	if code, _, _ := do(t, h, main.Pos, `{"conn_id":"main"}`); code != http.StatusOK {
		t.Fatalf("starting another conn_id must not disturb this one, got %d", code)
	}
	// The same pos presented under a different conn_id must not resolve.
	if code, _, errCode := do(t, h, main.Pos, `{"conn_id":"other"}`); code != http.StatusBadRequest || errCode != ErrorUnknownPos {
		t.Fatalf("got (%d, %q), want 400 M_UNKNOWN_POS", code, errCode)
	}
}

func TestInvalidBodyIsRejected(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(`{not json`))
	if res := h.OnIncomingRequest(req, testDevice); res.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", res.Code)
	}
}

func TestTxnIDIsEchoed(t *testing.T) {
	h := newTestHandler()
	_, resp, _ := do(t, h, "", `{"txn_id":"abc"}`)
	if resp.TxnID != "abc" {
		t.Errorf("txn_id = %q, want abc", resp.TxnID)
	}
	_, next, _ := do(t, h, resp.Pos, `{"txn_id":"def"}`)
	if next.TxnID != "def" {
		t.Errorf("txn_id = %q, want def", next.TxnID)
	}
}

func TestRequestLimits(t *testing.T) {
	h := newTestHandler()

	lists := map[string]string{}
	for i := 0; i <= maxListsAndSubscriptions; i++ {
		lists[fmt.Sprintf("l%d", i)] = `{"ranges":[[0,9]],"timeline_limit":1}`
	}
	subs := map[string]string{}
	for i := 0; i <= maxListsAndSubscriptions; i++ {
		subs[fmt.Sprintf("!r%d:x", i)] = `{"timeline_limit":1}`
	}
	build := func(field string, entries map[string]string) string {
		var parts []string
		for k, v := range entries {
			parts = append(parts, fmt.Sprintf("%q:%s", k, v))
		}
		return fmt.Sprintf(`{%q:{%s}}`, field, strings.Join(parts, ","))
	}

	for name, body := range map[string]string{
		"too many lists":         build("lists", lists),
		"too many subscriptions": build("room_subscriptions", subs),
		"conn_id too long":       fmt.Sprintf(`{"conn_id":%q}`, strings.Repeat("x", maxConnIDLength+1)),
	} {
		code, _, errCode := do(t, h, "", body)
		if code != http.StatusBadRequest || errCode != spec.ErrorInvalidParam {
			t.Errorf("%s: got (%d, %q), want 400 M_INVALID_PARAM", name, code, errCode)
		}
	}
	if code, _, _ := do(t, h, "", build("lists", map[string]string{"ok": `{"ranges":[[0,9]],"timeline_limit":1}`})); code != http.StatusOK {
		t.Errorf("a normal request must be accepted, got %d", code)
	}
}

func TestRetryAfterLostResponseKeepsTxnID(t *testing.T) {
	h := newTestHandler()
	_, first, _ := do(t, h, "", `{}`)
	_, second, _ := do(t, h, first.Pos, `{"txn_id":"t1"}`)
	_, replay, _ := do(t, h, first.Pos, `{"txn_id":"t2"}`)
	if replay.Pos != second.Pos || replay.TxnID != "t1" {
		t.Fatalf("replay = (%q, %q), want the original response (%q, t1)", replay.Pos, replay.TxnID, second.Pos)
	}
}

func TestSetPresence(t *testing.T) {
	type call struct{ user, presence string }
	var calls []call
	h := newTestHandler()
	h.SetPresenceUpdater(func(user, presence string) { calls = append(calls, call{user, presence}) })

	doQuery := func(query string) (int, spec.MatrixErrorCode) {
		req := httptest.NewRequest(http.MethodPost, "/sync"+query, strings.NewReader(`{}`))
		res := h.OnIncomingRequest(req, testDevice)
		if e, ok := res.JSON.(spec.MatrixError); ok {
			return res.Code, e.ErrCode
		}
		return res.Code, ""
	}

	for _, tc := range []struct{ query, want string }{
		{"", ""},
		{"?set_presence=online", "online"},
		{"?set_presence=offline", "offline"},
		{"?set_presence=unavailable", "unavailable"},
	} {
		calls = nil
		if code, _ := doQuery(tc.query); code != http.StatusOK {
			t.Fatalf("%q: code = %d", tc.query, code)
		}
		if len(calls) != 1 || calls[0] != (call{"@alice:test", tc.want}) {
			t.Errorf("%q: presence updates = %v, want one for %q", tc.query, calls, tc.want)
		}
	}

	// An unknown value is refused before anything happens, so a bad request cannot change presence or the session.
	calls = nil
	if code, errCode := doQuery("?set_presence=busy"); code != http.StatusBadRequest || errCode != spec.ErrorInvalidParam {
		t.Errorf("unknown value: got %d %s, want 400 M_INVALID_PARAM", code, errCode)
	}
	if len(calls) != 0 {
		t.Errorf("a refused request changed presence: %v", calls)
	}

	// Without an updater (presence not wired up) requests still work.
	h.SetPresenceUpdater(nil)
	if code, _ := doQuery("?set_presence=online"); code != http.StatusOK {
		t.Errorf("no updater: code = %d", code)
	}
}

// The spaces of a filter are looked up for all the lists together, so the bound is for the whole request.
func TestTooLargeBoundsSpacesOverAllLists(t *testing.T) {
	spaces := func(from, n int) []string {
		var out []string
		for i := from; i < from+n; i++ {
			out = append(out, fmt.Sprintf("!space%d:example.com", i))
		}
		return out
	}
	lists := func(per, count int) *Request {
		req := &Request{Lists: map[string]ListRequest{}}
		for i := 0; i < count; i++ {
			req.Lists[fmt.Sprintf("list%d", i)] = ListRequest{Filters: &ListFilters{Spaces: spaces(i*per, per)}}
		}
		return req
	}
	if msg := tooLarge(lists(50, 2)); msg != "" {
		t.Errorf("100 different spaces are the limit, got %q", msg)
	}
	if msg := tooLarge(lists(60, 2)); msg == "" {
		t.Error("120 different spaces in two lists are over the limit")
	}
	// The same spaces in several lists are the same spaces.
	same := &Request{Lists: map[string]ListRequest{}}
	for i := 0; i < 50; i++ {
		same.Lists[fmt.Sprintf("list%d", i)] = ListRequest{Filters: &ListFilters{Spaces: spaces(0, 100)}}
	}
	if msg := tooLarge(same); msg != "" {
		t.Errorf("one hundred spaces named again and again are not over the limit, got %q", msg)
	}
	if n := len(needsFor(same).spaces); n != 100 {
		t.Errorf("needsFor has %d spaces, want the 100 different ones", n)
	}
	long := &Request{Lists: map[string]ListRequest{"l": {Filters: &ListFilters{Spaces: []string{"!" + strings.Repeat("a", 300) + ":example.com"}}}}}
	if msg := tooLarge(long); msg == "" {
		t.Error("a space ID of 300 bytes is over the limit")
	}
}
