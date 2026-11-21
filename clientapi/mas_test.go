package clientapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/test/testrig"
	"github.com/element-hq/dendrite/userapi"
	"github.com/tidwall/gjson"
)

const masSecret = "mas-shared-secret"

func TestMASEndpoints(t *testing.T) {
	var mu sync.Mutex
	active := map[string]string{} // access token -> introspection response
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("Authorization") != "Bearer "+masSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if resp, ok := active[r.PostForm.Get("token")]; ok {
			_, _ = w.Write([]byte(resp))
			return
		}
		_, _ = w.Write([]byte(`{"active": false}`))
	}))
	t.Cleanup(op.Close)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, close := testrig.CreateConfig(t, dbType)
		defer close()
		cfg.ClientAPI.MSC3861 = config.MSC3861{
			Enabled: true, Issuer: op.URL, SharedSecret: masSecret, IntrospectionEndpoint: op.URL,
		}
		natsInstance := jetstream.NATSInstance{}
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, caching.DisableMetrics)

		call := func(method, path string, secret string, body any) *httptest.ResponseRecorder {
			t.Helper()
			var opts []test.HTTPRequestOpt
			if body != nil {
				opts = append(opts, test.WithJSONBody(t, body))
			}
			req := test.NewRequest(t, method, path, opts...)
			if secret != "" {
				req.Header.Set("Authorization", "Bearer "+secret)
			}
			rec := httptest.NewRecorder()
			routers.SynapseAdmin.ServeHTTP(rec, req)
			return rec
		}
		mas := func(method, name string, body any) *httptest.ResponseRecorder {
			return call(method, "/_synapse/mas/"+name, masSecret, body)
		}
		whoami := func(token string) *httptest.ResponseRecorder {
			req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/account/whoami")
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			return rec
		}
		wantCode := func(rec *httptest.ResponseRecorder, want int, what string) {
			t.Helper()
			if rec.Code != want {
				t.Fatalf("%s: got HTTP %d %s, want %d", what, rec.Code, rec.Body.String(), want)
			}
		}

		// Only MAS may call the endpoints.
		wantCode(call(http.MethodGet, "/_synapse/mas/query_user?localpart=alice", "", nil), http.StatusForbidden, "no secret")
		wantCode(call(http.MethodGet, "/_synapse/mas/query_user?localpart=alice", "wrong", nil), http.StatusForbidden, "wrong secret")
		wantCode(mas(http.MethodGet, "query_user?localpart=alice", nil), http.StatusNotFound, "unknown user")

		// Provisioning creates the user, a second call only updates it.
		wantCode(mas(http.MethodGet, "is_localpart_available?localpart=alice", nil), http.StatusOK, "alice is free")
		wantCode(mas(http.MethodPost, "provision_user", map[string]any{
			"localpart": "alice", "set_displayname": "Alice", "set_emails": []string{"alice@example.com"},
		}), http.StatusCreated, "provision alice")
		wantCode(mas(http.MethodPost, "provision_user", map[string]any{"localpart": "alice", "set_displayname": "Alice A"}), http.StatusOK, "update alice")
		wantCode(mas(http.MethodPost, "provision_user", map[string]any{
			"localpart": "bob", "set_displayname": "x", "unset_displayname": true,
		}), http.StatusBadRequest, "set and unset")
		wantCode(mas(http.MethodGet, "is_localpart_available?localpart=alice", nil), http.StatusBadRequest, "alice is taken")
		rec := mas(http.MethodGet, "query_user?localpart=alice", nil)
		wantCode(rec, http.StatusOK, "query alice")
		if got := gjson.GetBytes(rec.Body.Bytes(), "display_name").String(); got != "Alice A" {
			t.Errorf("display_name = %q, want %q (%s)", got, "Alice A", rec.Body)
		}
		if gjson.GetBytes(rec.Body.Bytes(), "is_deactivated").Bool() {
			t.Error("alice must not be deactivated")
		}
		wantCode(mas(http.MethodPost, "unset_displayname", map[string]any{"localpart": "alice"}), http.StatusOK, "unset display name")
		wantCode(mas(http.MethodPost, "set_displayname", map[string]any{"localpart": "alice", "displayname": "Alice B"}), http.StatusOK, "set display name")

		// A token only works once MAS has provisioned its device.
		mu.Lock()
		active["tok1"] = `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:*", "device_id": "DEV1"}`
		mu.Unlock()
		wantCode(whoami("tok1"), http.StatusUnauthorized, "device not provisioned yet")
		wantCode(mas(http.MethodPost, "upsert_device", map[string]any{"localpart": "alice", "device_id": "DEV1", "display_name": "Phone"}), http.StatusCreated, "create device")
		wantCode(mas(http.MethodPost, "upsert_device", map[string]any{"localpart": "alice", "device_id": "DEV1", "display_name": "Phone 2"}), http.StatusOK, "update device")
		rec = whoami("tok1")
		wantCode(rec, http.StatusOK, "whoami with a provisioned device")
		if got := gjson.GetBytes(rec.Body.Bytes(), "device_id").String(); got != "DEV1" {
			t.Errorf("device_id = %q (%s)", got, rec.Body)
		}
		wantCode(mas(http.MethodPost, "update_device_display_name", map[string]any{"localpart": "alice", "device_id": "DEV1", "display_name": "Tablet"}), http.StatusOK, "rename device")
		wantCode(mas(http.MethodPost, "update_device_display_name", map[string]any{"localpart": "alice", "device_id": "NOPE", "display_name": "x"}), http.StatusNotFound, "rename unknown device")

		// sync_devices makes the device list exactly the given one, so DEV1 goes away.
		wantCode(mas(http.MethodPost, "sync_devices", map[string]any{"localpart": "alice", "devices": []string{"DEV2", "DEV3"}}), http.StatusOK, "sync devices")
		wantCode(whoami("tok1"), http.StatusUnauthorized, "device removed by sync_devices")
		mu.Lock()
		active["tok2"] = `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:*", "device_id": "DEV2"}`
		mu.Unlock()
		wantCode(whoami("tok2"), http.StatusOK, "device created by sync_devices")
		wantCode(mas(http.MethodPost, "delete_device", map[string]any{"localpart": "alice", "device_id": "DEV2"}), http.StatusNoContent, "delete device")
		wantCode(whoami("tok2"), http.StatusUnauthorized, "device deleted")

		// Deactivation takes the user's access away, reactivation gives the account back.
		wantCode(mas(http.MethodPost, "delete_user", map[string]any{"localpart": "alice", "erase": false}), http.StatusOK, "delete user")
		rec = mas(http.MethodGet, "query_user?localpart=alice", nil)
		if !gjson.GetBytes(rec.Body.Bytes(), "is_deactivated").Bool() {
			t.Errorf("alice should be deactivated: %s", rec.Body)
		}
		// The addresses of a deleted user are free for the next one: MAS gives them to another user.
		wantCode(mas(http.MethodPost, "provision_user", map[string]any{
			"localpart": "carol", "set_emails": []string{"alice@example.com"},
		}), http.StatusCreated, "an address of a deleted user goes to another user")
		wantCode(mas(http.MethodPost, "delete_user", map[string]any{"localpart": "carol", "erase": false}), http.StatusOK, "delete carol")
		wantCode(mas(http.MethodPost, "reactivate_user", map[string]any{"localpart": "alice"}), http.StatusOK, "reactivate user")
		rec = mas(http.MethodGet, "query_user?localpart=alice", nil)
		if gjson.GetBytes(rec.Body.Bytes(), "is_deactivated").Bool() {
			t.Errorf("alice should be active again: %s", rec.Body)
		}

		// The legacy endpoints are gone.
		req := test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/login", test.WithJSONBody(t, map[string]any{"type": "m.login.password"}))
		rec = httptest.NewRecorder()
		routers.Client.ServeHTTP(rec, req)
		wantCode(rec, http.StatusNotFound, "legacy login")

		// Cross-signing replacement is only allowed after MAS approved it.
		wantCode(mas(http.MethodPost, "allow_cross_signing_reset", map[string]any{"localpart": "nobody"}), http.StatusNotFound, "approval for unknown user")
		wantCode(mas(http.MethodPost, "allow_cross_signing_reset", map[string]any{"localpart": "alice"}), http.StatusOK, "approval")
	})
}
