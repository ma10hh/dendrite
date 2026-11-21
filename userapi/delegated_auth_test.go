package userapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/element-hq/dendrite/internal/delegatedauth"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/test"
	"github.com/element-hq/dendrite/userapi/api"
	"github.com/element-hq/dendrite/userapi/internal"
)

func TestQueryAccessTokenDelegated(t *testing.T) {
	tokens := map[string]string{
		"good":      `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:* urn:matrix:client:device:DEVICE1"}`,
		"nodevice":  `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:*"}`,
		"noapi":     `{"active": true, "username": "alice", "scope": "urn:matrix:client:device:DEVICE1"}`,
		"badname":   `{"active": true, "username": "Al ice", "scope": "urn:matrix:client:api:* urn:matrix:client:device:DEVICE1"}`,
		"otherdev":  `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:* urn:matrix:client:device:DEVICE2"}`,
		"remote":    `{"active": true, "username": "@bob:elsewhere.org", "scope": "urn:matrix:client:api:* urn:matrix:client:device:DEVICE1"}`,
		"inactive":  `{"active": false}`,
		"qualified": `{"active": true, "username": "@carol:example.com", "scope": "urn:matrix:client:api:* urn:matrix:client:device:DEVICE1"}`,
	}
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if resp, ok := tokens[r.PostForm.Get("token")]; ok {
			_, _ = w.Write([]byte(resp))
			return
		}
		_, _ = w.Write([]byte(`{"active": false}`))
	}))
	t.Cleanup(op.Close)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		userAPI, accountDB, close := MustMakeInternalAPI(t, apiTestOpts{}, dbType, nil)
		defer close()
		internalAPI := userAPI.(*internal.UserInternalAPI)
		internalAPI.DelegatedAuth = delegatedauth.New(&config.MSC3861{
			Enabled: true, Issuer: op.URL, ClientID: "dendrite", ClientSecret: "secret", IntrospectionEndpoint: op.URL,
		})
		ctx := context.Background()

		query := func(token string) api.QueryAccessTokenResponse {
			t.Helper()
			var res api.QueryAccessTokenResponse
			if err := userAPI.QueryAccessToken(ctx, &api.QueryAccessTokenRequest{AccessToken: token}, &res); err != nil {
				t.Fatalf("QueryAccessToken(%q): %v", token, err)
			}
			return res
		}

		// The first use of a token provisions the account and the device.
		res := query("good")
		if res.Device == nil || res.Device.UserID != "@alice:example.com" || res.Device.ID != "DEVICE1" {
			t.Fatalf("unexpected device %+v (err %q)", res.Device, res.Err)
		}
		if _, err := accountDB.GetAccountByLocalpart(ctx, "alice", serverName); err != nil {
			t.Errorf("account was not created: %v", err)
		}
		firstSession := res.Device.SessionID

		// Later uses must not recreate the device, as that would revoke it.
		res = query("good")
		if res.Device == nil || res.Device.SessionID != firstSession || firstSession == 0 {
			t.Errorf("device was recreated: %+v", res.Device)
		}

		// A different device ID creates a second device for the same account.
		if res = query("otherdev"); res.Device == nil || res.Device.ID != "DEVICE2" {
			t.Errorf("unexpected device %+v", res.Device)
		}
		devices, err := accountDB.GetDevicesByLocalpart(ctx, "alice", serverName)
		if err != nil || len(devices) != 2 {
			t.Errorf("got %d devices (%v), want 2", len(devices), err)
		}

		if res = query("qualified"); res.Device == nil || res.Device.UserID != "@carol:example.com" {
			t.Errorf("fully qualified usernames should work: %+v", res.Device)
		}

		for _, token := range []string{"nodevice", "noapi"} {
			if res = query(token); res.Device != nil || res.Err == "" {
				t.Errorf("%s: got device %+v err %q, want a forbidden error", token, res.Device, res.Err)
			}
		}
		for _, token := range []string{"inactive", "unknown"} {
			if res = query(token); res.Device != nil || res.Err != "" {
				t.Errorf("%s: got device %+v err %q, want an unknown token", token, res.Device, res.Err)
			}
		}
		// Tokens for users this server cannot represent are the token's fault, not a server error.
		for _, token := range []string{"badname", "remote"} {
			if res = query(token); res.Device != nil || res.Err == "" {
				t.Errorf("%s: got device %+v err %q, want a forbidden error", token, res.Device, res.Err)
			}
		}

		// Deactivated accounts must not be revived by a still-active provider token.
		if err = accountDB.DeactivateAccount(ctx, "alice", serverName); err != nil {
			t.Fatalf("DeactivateAccount: %v", err)
		}
		if res = query("good"); res.Device != nil || res.Err == "" {
			t.Errorf("deactivated account: got device %+v err %q, want a forbidden error", res.Device, res.Err)
		}

		// Tokens in the database are not accepted while authentication is delegated.
		if _, err = accountDB.CreateDevice(ctx, "alice", serverName, nil, "legacytoken", nil, "", ""); err != nil {
			t.Fatal(err)
		}
		if res = query("legacytoken"); res.Device != nil {
			t.Errorf("legacy token accepted: %+v", res.Device)
		}
	})
}

func TestQueryAccessTokenMAS(t *testing.T) {
	tokens := map[string]string{
		"alice":       `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:*", "device_id": "DEVICE1"}`,
		"alice-admin": `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:* urn:synapse:admin:*", "device_id": "DEVICE1"}`,
		"alice-gone":  `{"active": true, "username": "alice", "scope": "urn:matrix:client:api:*", "device_id": "GONE"}`,
		"bob":         `{"active": true, "username": "bob", "scope": "urn:matrix:client:api:*", "device_id": "DEVICE1"}`,
	}
	var introspections atomic.Int32
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		introspections.Add(1)
		_ = r.ParseForm()
		if resp, ok := tokens[r.PostForm.Get("token")]; ok {
			_, _ = w.Write([]byte(resp))
			return
		}
		_, _ = w.Write([]byte(`{"active": false}`))
	}))
	t.Cleanup(op.Close)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		userAPI, accountDB, close := MustMakeInternalAPI(t, apiTestOpts{}, dbType, nil)
		defer close()
		internalAPI := userAPI.(*internal.UserInternalAPI)
		internalAPI.DelegatedAuth = delegatedauth.New(&config.MSC3861{
			Enabled: true, Issuer: op.URL, SharedSecret: "shared", IntrospectionEndpoint: op.URL,
		})
		ctx := context.Background()
		query := func(token string) api.QueryAccessTokenResponse {
			t.Helper()
			var res api.QueryAccessTokenResponse
			if err := userAPI.QueryAccessToken(ctx, &api.QueryAccessTokenRequest{AccessToken: token}, &res); err != nil {
				t.Fatalf("QueryAccessToken(%q): %v", token, err)
			}
			return res
		}

		// Nothing is created for users and devices MAS has not provisioned.
		if res := query("bob"); res.Device != nil || res.Err == "" {
			t.Errorf("unprovisioned user: got device %+v err %q, want a forbidden error", res.Device, res.Err)
		}
		if _, err := accountDB.GetAccountByLocalpart(ctx, "bob", serverName); err == nil {
			t.Error("the account of an unprovisioned user must not be created")
		}
		if _, err := accountDB.CreateAccount(ctx, "alice", serverName, "", "", api.AccountTypeUser); err != nil {
			t.Fatal(err)
		}
		if res := query("alice"); res.Device != nil || res.Err != "" {
			t.Errorf("unprovisioned device: got device %+v err %q, want an unknown token", res.Device, res.Err)
		}
		if devices, _ := accountDB.GetDevicesByLocalpart(ctx, "alice", serverName); len(devices) != 0 {
			t.Errorf("a device was created for an unprovisioned session: %+v", devices)
		}

		// Once MAS has upserted the device the token works, and admin sessions are admins.
		deviceID := "DEVICE1"
		if _, err := accountDB.CreateDevice(ctx, "alice", serverName, &deviceID, "unused", nil, "", ""); err != nil {
			t.Fatal(err)
		}
		if res := query("alice"); res.Device == nil || res.Device.ID != "DEVICE1" || res.Device.AccountType != api.AccountTypeUser {
			t.Errorf("provisioned device: got %+v err %q", res.Device, res.Err)
		}
		if res := query("alice-admin"); res.Device == nil || res.Device.AccountType != api.AccountTypeAdmin {
			t.Errorf("admin scope: got %+v err %q, want an admin device", res.Device, res.Err)
		}

		// Deleting the device (a logout) ends the session even while the introspection is cached.
		if err := accountDB.RemoveDevices(ctx, "alice", serverName, []string{"DEVICE1"}); err != nil {
			t.Fatal(err)
		}
		before := introspections.Load()
		if res := query("alice"); res.Device != nil {
			t.Errorf("the cached token of a deleted device still works: %+v", res.Device)
		}
		if res := query("alice"); res.Device != nil {
			t.Errorf("deleted device came back: %+v", res.Device)
		}
		if introspections.Load() == before {
			t.Error("the cache entry of the deleted device was not invalidated")
		}
	})
}

func TestQueryAccessTokenAppserviceDevice(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		userAPI, accountDB, close := MustMakeInternalAPI(t, apiTestOpts{}, dbType, nil)
		defer close()
		internalAPI := userAPI.(*internal.UserInternalAPI)
		internalAPI.AppServices = []config.ApplicationService{{ID: "bridge", ASToken: "as_token", SenderLocalpart: "bridgebot"}}
		ctx := context.Background()
		for _, localpart := range []string{"bridgebot", "ghost"} {
			if _, err := accountDB.CreateAccount(ctx, localpart, serverName, "", "bridge", api.AccountTypeAppService); err != nil {
				t.Fatal(err)
			}
		}
		query := func(userID, deviceID string) api.QueryAccessTokenResponse {
			t.Helper()
			var res api.QueryAccessTokenResponse
			if err := userAPI.QueryAccessToken(ctx, &api.QueryAccessTokenRequest{
				AccessToken: "as_token", AppServiceUserID: userID, AppServiceDeviceID: deviceID,
			}, &res); err != nil {
				t.Fatal(err)
			}
			return res
		}

		// Without a device ID the appservice has its placeholder device.
		if res := query("@ghost:example.com", ""); res.Device == nil || res.Device.ID != "AS_Device" || res.Device.UserID != "@ghost:example.com" {
			t.Errorf("got %+v err %q", res.Device, res.Err)
		}
		// Without a user the appservice keeps the device it registered for itself, named after its sender_localpart, as
		// before: bridge bots have their keys and sessions on it. (The appservice makes it at startup.)
		botID := "bridgebot"
		if _, err := accountDB.CreateDevice(ctx, "bridgebot", serverName, &botID, "as_token", nil, "", ""); err != nil {
			t.Fatal(err)
		}
		if res := query("", ""); res.Device == nil || res.Device.ID != "bridgebot" || res.Device.UserID != "@bridgebot:example.com" {
			t.Errorf("sender without a user: got %+v err %q, want the registered device", res.Device, res.Err)
		}

		// A device that does not exist cannot be asserted.
		if res := query("@ghost:example.com", "GHOSTDEV"); res.Device != nil || !res.UnknownDevice {
			t.Errorf("got device %+v unknown %v, want an unknown device", res.Device, res.UnknownDevice)
		}
		if res := query("", "BOTDEV"); res.Device != nil || !res.UnknownDevice {
			t.Errorf("sender: got device %+v unknown %v, want an unknown device", res.Device, res.UnknownDevice)
		}

		for _, d := range []struct{ localpart, id string }{{"ghost", "GHOSTDEV"}, {"bridgebot", "BOTDEV"}} {
			id := d.id
			if _, err := accountDB.CreateDevice(ctx, d.localpart, serverName, &id, "unused-"+d.id, nil, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if res := query("@ghost:example.com", "GHOSTDEV"); res.Device == nil || res.Device.ID != "GHOSTDEV" || res.Device.AppserviceID != "bridge" || res.Device.UserID != "@ghost:example.com" {
			t.Errorf("got %+v", res.Device)
		}
		if res := query("", "BOTDEV"); res.Device == nil || res.Device.ID != "BOTDEV" || res.Device.UserID != "@bridgebot:example.com" {
			t.Errorf("sender: got %+v", res.Device)
		}
		// One user's device cannot be asserted for another user.
		if res := query("@ghost:example.com", "BOTDEV"); res.Device != nil || !res.UnknownDevice {
			t.Errorf("got device %+v unknown %v, want an unknown device", res.Device, res.UnknownDevice)
		}
	})
}
