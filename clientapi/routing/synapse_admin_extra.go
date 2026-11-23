// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// Small endpoints of the Synapse admin API that look at or change one user:
// https://element-hq.github.io/synapse/latest/admin_api/user_admin_api.html

// SynapseAdminUserAdmin implements GET and PUT /_synapse/admin/v1/users/{userId}/admin.
func SynapseAdminUserAdmin(req *http.Request, admin *userapi.Device, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	userID, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	acc, errResp := lookupAdminAccount(req, userAPI, localpart, serverName)
	if errResp != nil {
		return *errResp
	}
	if req.Method == http.MethodGet {
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]bool{"admin": acc.AccountType == userapi.AccountTypeAdmin}}
	}
	var body struct {
		Admin *bool `json:"admin"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
	}
	if body.Admin == nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("admin is missing")}
	}
	if acc.AccountType == userapi.AccountTypeAppService || acc.AccountType == userapi.AccountTypeGuest {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Guests and appservice users cannot be changed with this endpoint")}
	}
	// An administrator who takes their own rights away may leave nobody who can use the admin API.
	if admin != nil && admin.UserID == userID && !*body.Admin {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("You may not demote yourself")}
	}
	newType := userapi.AccountTypeUser
	if *body.Admin {
		newType = userapi.AccountTypeAdmin
	}
	if acc.AccountType != newType {
		if err := userAPI.AdminSetAccountType(req.Context(), localpart, serverName, newType); err != nil {
			return adminServerError(req, "AdminSetAccountType", err)
		}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

// SynapseAdminPushers implements GET /_synapse/admin/v1/users/{userId}/pushers.
func SynapseAdminPushers(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	_, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	if _, errResp = lookupAdminAccount(req, userAPI, localpart, serverName); errResp != nil {
		return *errResp
	}
	var res userapi.QueryPushersResponse
	if err := userAPI.QueryPushers(req.Context(), &userapi.QueryPushersRequest{Localpart: localpart, ServerName: serverName}, &res); err != nil {
		return adminServerError(req, "QueryPushers", err)
	}
	pushers := res.Pushers
	if pushers == nil {
		pushers = []userapi.Pusher{}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"pushers": pushers, "total": len(pushers)}}
}

// SynapseAdminAccountData implements GET /_synapse/admin/v1/users/{userId}/accountdata.
func SynapseAdminAccountData(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	userID, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	if _, errResp = lookupAdminAccount(req, userAPI, localpart, serverName); errResp != nil {
		return *errResp
	}
	var res userapi.QueryAccountDataResponse
	if err := userAPI.QueryAccountData(req.Context(), &userapi.QueryAccountDataRequest{UserID: userID}, &res); err != nil {
		return adminServerError(req, "QueryAccountData", err)
	}
	global, rooms := res.GlobalAccountData, res.RoomAccountData
	if global == nil {
		global = map[string]json.RawMessage{}
	}
	if rooms == nil {
		rooms = map[string]map[string]json.RawMessage{}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"account_data": map[string]any{"global": global, "rooms": rooms}}}
}

// SynapseAdminUsernameAvailable implements GET /_synapse/admin/v1/username_available?username=.
func SynapseAdminUsernameAvailable(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	if req.URL.Query().Get("username") == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("Missing username")}
	}
	// The checks of /register/available are the ones to use: reserved names and the namespaces of appservices count.
	check := req.Clone(req.Context())
	check.Host = string(cfg.Matrix.ServerName)
	return RegisterAvailable(check, cfg, userAPI)
}

// stubOK answers the Synapse admin endpoints that have nothing to show in Dendrite, so that tools that poll them do not
// report an error.
func stubOK(body any) util.JSONResponse {
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}

// SynapseAdminBackgroundUpdates implements the endpoints for the background updates of Synapse. Dendrite has none: its
// migrations run at start, before the server answers.
func SynapseAdminBackgroundUpdates(req *http.Request) util.JSONResponse {
	if strings.HasSuffix(req.URL.Path, "/status") {
		return stubOK(map[string]any{"enabled": true, "current_updates": map[string]any{}})
	}
	return stubOK(map[string]any{"enabled": true})
}

// SynapseAdminExperimentalFeatures implements GET and PUT /_synapse/admin/v1/experimental_features/{userId}. The features
// of Synapse's list do not exist here, so the list is empty, and a change of one is refused.
func SynapseAdminExperimentalFeatures(req *http.Request) util.JSONResponse {
	if req.Method == http.MethodPut {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Dendrite has no experimental features that can be changed for a user")}
	}
	return stubOK(map[string]any{"features": map[string]bool{}})
}

// SynapseAdminScheduledTasks implements GET /_synapse/admin/v1/scheduled_tasks. Dendrite has no scheduler of Synapse's
// kind, so there are none.
func SynapseAdminScheduledTasks(*http.Request) util.JSONResponse {
	return stubOK(map[string]any{"scheduled_tasks": []any{}})
}
