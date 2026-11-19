package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
)

// tokenOnlyAPI knows one access token, "valid".
type tokenOnlyAPI struct{ userapi.ClientUserAPI }

func (tokenOnlyAPI) QueryAccessToken(_ context.Context, req *userapi.QueryAccessTokenRequest, res *userapi.QueryAccessTokenResponse) error {
	if req.AccessToken == "valid" {
		res.Device = &userapi.Device{ID: "D", UserID: "@alice:test"}
	}
	return nil
}

type rendezvousTest struct {
	t      *testing.T
	router *mux.Router
	store  *rendezvousStore
	now    time.Time
}

func newRendezvousTest(t *testing.T, enabled, allowUnauthenticated, unstable bool) *rendezvousTest {
	rt := &rendezvousTest{t: t, router: mux.NewRouter(), store: newRendezvousStore(), now: time.Now()}
	rt.store.now = func() time.Time { return rt.now }
	cfg := &config.ClientAPI{Rendezvous: config.Rendezvous{Enabled: enabled, AllowUnauthenticated: allowUnauthenticated}}
	limits := httputil.NewRateLimits(&config.RateLimiting{Enabled: false})
	setupRendezvousRoutes(rt.router, cfg, tokenOnlyAPI{}, limits, rt.store, unstable)
	return rt
}

func (rt *rendezvousTest) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	rt.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	rt.router.ServeHTTP(rec, req)
	return rec
}

func (rt *rendezvousTest) expect(rec *httptest.ResponseRecorder, code int, errcode string) gjson.Result {
	rt.t.Helper()
	if rec.Code != code {
		rt.t.Fatalf("got HTTP %d %s, want %d", rec.Code, rec.Body, code)
	}
	if got := gjson.GetBytes(rec.Body.Bytes(), "errcode").String(); got != errcode {
		rt.t.Fatalf("got errcode %q (%s), want %q", got, rec.Body, errcode)
	}
	return gjson.ParseBytes(rec.Body.Bytes())
}

func TestRendezvousDisabled(t *testing.T) {
	rt := newRendezvousTest(t, false, true, false)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/rendezvous"},
		{http.MethodPost, "/rendezvous"},
		{http.MethodGet, "/rendezvous/abc"},
		{http.MethodPut, "/rendezvous/abc/txn"},
		{http.MethodDelete, "/rendezvous/abc"},
	} {
		rt.expect(rt.do(tc.method, tc.path, `{"data": "x"}`), http.StatusNotFound, "M_UNRECOGNIZED")
	}
}

func TestRendezvousAuthenticationPolicy(t *testing.T) {
	closed := newRendezvousTest(t, true, false, false)
	if closed.expect(closed.do(http.MethodGet, "/rendezvous", ""), http.StatusOK, "").Get("create_available").Bool() {
		t.Error("anonymous requesters must not be offered creation when it needs a token")
	}
	if !closed.expect(closed.do(http.MethodGet, "/rendezvous", "", "Authorization", "Bearer valid"), http.StatusOK, "").Get("create_available").Bool() {
		t.Error("a user must be offered creation")
	}
	closed.expect(closed.do(http.MethodGet, "/rendezvous", "", "Authorization", "Bearer expired"), http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	closed.expect(closed.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusForbidden, "M_FORBIDDEN")
	closed.expect(closed.do(http.MethodPost, "/rendezvous", `{"data": "x"}`, "Authorization", "Bearer valid"), http.StatusOK, "")

	open := newRendezvousTest(t, true, true, false)
	if !open.expect(open.do(http.MethodGet, "/rendezvous", ""), http.StatusOK, "").Get("create_available").Bool() {
		t.Error("open servers must offer creation to everybody")
	}
	open.expect(open.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusOK, "")
	// A bad token is an error even where no token is needed.
	open.expect(open.do(http.MethodPost, "/rendezvous", `{"data": "x"}`, "Authorization", "Bearer expired"), http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
}

func TestRendezvousExchange(t *testing.T) {
	rt := newRendezvousTest(t, true, true, false)
	created := rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "hello from A"}`), http.StatusOK, "")
	id := created.Get("id").String()
	if id == "" || created.Get("sequence_token").String() != "1" || created.Get("expires_in_ms").Int() != 300000 {
		t.Fatalf("unexpected creation response %s", created.Raw)
	}

	got := rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, ""), http.StatusOK, "")
	if got.Get("data").String() != "hello from A" || got.Get("sequence_token").String() != "1" {
		t.Fatalf("unexpected payload %s", got.Raw)
	}

	// B answers with the token it saw.
	sent := rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/txnB1", `{"sequence_token": "1", "data": "hello from B"}`), http.StatusOK, "")
	if sent.Get("sequence_token").String() != "2" {
		t.Fatalf("unexpected send response %s", sent.Raw)
	}
	got = rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, ""), http.StatusOK, "")
	if got.Get("data").String() != "hello from B" || got.Get("sequence_token").String() != "2" {
		t.Fatalf("unexpected payload %s", got.Raw)
	}

	// A write that was based on an old payload is refused.
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/txnA1", `{"sequence_token": "1", "data": "stale"}`), http.StatusConflict, "M_CONCURRENT_WRITE")

	// A retry of a successful send returns the earlier answer and changes nothing, even with a different body.
	again := rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/txnB1", `{"sequence_token": "1", "data": "different"}`), http.StatusOK, "")
	if again.Get("sequence_token").String() != "2" {
		t.Errorf("a retry must get the recorded token, got %s", again.Raw)
	}
	again = rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/txnB1", `not json`), http.StatusOK, "")
	if again.Get("sequence_token").String() != "2" {
		t.Errorf("a retry is answered before the body is checked, got %s", again.Raw)
	}
	got = rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, ""), http.StatusOK, "")
	if got.Get("data").String() != "hello from B" {
		t.Errorf("a retry must not change the payload, got %s", got.Raw)
	}

	// The same data again still gets a new token.
	next := rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/txnA2", `{"sequence_token": "2", "data": "hello from B"}`), http.StatusOK, "")
	if next.Get("sequence_token").String() != "3" {
		t.Errorf("every write needs a new sequence token, got %s", next.Raw)
	}

	// Cancelling ends the session for both.
	rt.expect(rt.do(http.MethodDelete, "/rendezvous/"+id, ""), http.StatusOK, "")
	rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, ""), http.StatusNotFound, "M_NOT_FOUND")
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/txnX", `{"sequence_token": "3", "data": "x"}`), http.StatusNotFound, "M_NOT_FOUND")
	rt.expect(rt.do(http.MethodDelete, "/rendezvous/"+id, ""), http.StatusNotFound, "M_NOT_FOUND")
}

func TestRendezvousUnstableErrorCode(t *testing.T) {
	rt := newRendezvousTest(t, true, true, true)
	id := rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusOK, "").Get("id").String()
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/t1", `{"sequence_token": "9", "data": "x"}`), http.StatusConflict, "IO_ELEMENT_MSC4388_CONCURRENT_WRITE")
}

func TestRendezvousValidation(t *testing.T) {
	rt := newRendezvousTest(t, true, true, false)
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `not json`), http.StatusBadRequest, "M_NOT_JSON")
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `{}`), http.StatusBadRequest, "M_BAD_JSON")
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": 5}`), http.StatusBadRequest, "M_BAD_JSON")
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "`+strings.Repeat("a", rendezvousMaxData+1)+`"}`), http.StatusRequestEntityTooLarge, "M_TOO_LARGE")
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "`+strings.Repeat("a", rendezvousMaxBody)+`"}`), http.StatusRequestEntityTooLarge, "M_TOO_LARGE")
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "`+strings.Repeat("a", rendezvousMaxData)+`"}`), http.StatusOK, "")

	id := rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusOK, "").Get("id").String()
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/bad%20id", `{"sequence_token": "1", "data": "x"}`), http.StatusBadRequest, "M_INVALID_PARAM")
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/t1", `{"data": "x"}`), http.StatusBadRequest, "M_BAD_JSON")
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/t2", `{"sequence_token": "1", "data": "`+strings.Repeat("a", rendezvousMaxData+1)+`"}`), http.StatusRequestEntityTooLarge, "M_TOO_LARGE")
	rt.expect(rt.do(http.MethodPut, "/rendezvous/nope/t3", `{"sequence_token": "1", "data": "x"}`), http.StatusNotFound, "M_NOT_FOUND")

	// A browser opening the URL does not get the payload.
	rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, "", "Sec-Fetch-Mode", "navigate"), http.StatusForbidden, "M_FORBIDDEN")
	rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, "", "Sec-Fetch-Dest", "document"), http.StatusForbidden, "M_FORBIDDEN")
	rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, "", "Sec-Fetch-Mode", "cors"), http.StatusOK, "")
}

func TestRendezvousExpiryAndLimits(t *testing.T) {
	rt := newRendezvousTest(t, true, true, false)
	id := rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusOK, "").Get("id").String()

	rt.now = rt.now.Add(100 * time.Second)
	got := rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, ""), http.StatusOK, "")
	if ms := got.Get("expires_in_ms").Int(); ms < 199000 || ms > 200000 {
		t.Errorf("expires_in_ms = %d, want about 200000", ms)
	}
	rt.now = rt.now.Add(201 * time.Second)
	rt.expect(rt.do(http.MethodGet, "/rendezvous/"+id, ""), http.StatusNotFound, "M_NOT_FOUND")
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/t1", `{"sequence_token": "1", "data": "x"}`), http.StatusNotFound, "M_NOT_FOUND")

	// The number of sessions is capped, and expired ones make room.
	for i := 0; i < rendezvousMaxSessions; i++ {
		if _, _, err := rt.store.create("x"); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusTooManyRequests, "M_LIMIT_EXCEEDED")
	rt.now = rt.now.Add(time.Hour)
	rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusOK, "")

	// So is the number of sends to one session.
	id = rt.expect(rt.do(http.MethodPost, "/rendezvous", `{"data": "x"}`), http.StatusOK, "").Get("id").String()
	for i := 0; i < rendezvousMaxTxns; i++ {
		body, _ := json.Marshal(map[string]string{"sequence_token": itoa(i + 1), "data": "x"})
		rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/t"+itoa(i), string(body)), http.StatusOK, "")
	}
	body, _ := json.Marshal(map[string]string{"sequence_token": itoa(rendezvousMaxTxns + 1), "data": "x"})
	rt.expect(rt.do(http.MethodPut, "/rendezvous/"+id+"/over", string(body)), http.StatusTooManyRequests, "M_LIMIT_EXCEEDED")
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
