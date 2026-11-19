// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

const slidingSyncPath = "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync"

func TestSlidingSyncRoute(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		user := test.NewUser(t)
		alice := userapi.Device{
			ID:          "ALICEID",
			UserID:      user.ID,
			AccessToken: "ALICE_BEARER_TOKEN",
			AccountType: userapi.AccountTypeUser,
		}

		cfg, processCtx, close := testrig.CreateConfig(t, dbType)
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		defer close()

		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
		AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, &syncUserAPI{accounts: []userapi.Device{alice}}, &syncRoomserverAPI{}, caches, caching.DisableMetrics)

		post := func(token, query, body string) *httptest.ResponseRecorder {
			params := map[string]string{}
			if token != "" {
				params["access_token"] = token
			}
			if query != "" {
				params["pos"] = query
			}
			w := httptest.NewRecorder()
			routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodPost, slidingSyncPath, test.WithQueryParams(params), test.WithJSONBody(t, gjson.Parse(body).Value())))
			return w
		}

		if w := post("", "", `{}`); w.Code != http.StatusUnauthorized {
			t.Fatalf("missing token: got HTTP %d, want 401", w.Code)
		}
		if w := post("nope", "", `{}`); w.Code != http.StatusUnauthorized {
			t.Fatalf("unknown token: got HTTP %d, want 401", w.Code)
		}

		w := post(alice.AccessToken, "", `{"conn_id":"main"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("new session: got HTTP %d (%s), want 200", w.Code, w.Body.String())
		}
		pos := gjson.Get(w.Body.String(), "pos").String()
		if pos == "" {
			t.Fatalf("response has no pos: %s", w.Body.String())
		}

		if w := post(alice.AccessToken, pos, `{"conn_id":"main"}`); w.Code != http.StatusOK {
			t.Fatalf("continued session: got HTTP %d (%s), want 200", w.Code, w.Body.String())
		}

		w = post(alice.AccessToken, "unknown/7", `{"conn_id":"main"}`)
		if w.Code != http.StatusBadRequest || gjson.Get(w.Body.String(), "errcode").String() != "M_UNKNOWN_POS" {
			t.Fatalf("unknown pos: got HTTP %d (%s), want 400 M_UNKNOWN_POS", w.Code, w.Body.String())
		}

		// The legacy endpoint must keep working next to the new one.
		w = httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
			"access_token": alice.AccessToken, "timeout": "0",
		})))
		if w.Code != http.StatusOK {
			t.Fatalf("legacy /sync: got HTTP %d, want 200", w.Code)
		}
	})
}
