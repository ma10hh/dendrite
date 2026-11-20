// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/internal/httputil"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// The users part of the Synapse admin API, which tools like synapse-admin and Draupnir use:
// https://element-hq.github.io/synapse/latest/admin_api/user_admin_api.html

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// synapseUser is how the admin API shows a user.
type synapseUser struct {
	Name        string  `json:"name"`
	IsGuest     int     `json:"is_guest"`
	Admin       int     `json:"admin"`
	UserType    *string `json:"user_type"`
	Deactivated int     `json:"deactivated"`
	ShadowBan   int     `json:"shadow_banned"`
	DisplayName *string `json:"displayname"`
	AvatarURL   *string `json:"avatar_url"`
	CreationTS  int64   `json:"creation_ts"`
	Erased      bool    `json:"erased"`
	Locked      bool    `json:"locked"`
	AppService  *string `json:"appservice_id,omitempty"`
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func synapseUserFromEntry(e userapi.AccountListEntry) synapseUser {
	u := synapseUser{
		Name:        e.UserID,
		IsGuest:     boolInt(e.AccountType == userapi.AccountTypeGuest),
		Admin:       boolInt(e.AccountType == userapi.AccountTypeAdmin),
		Deactivated: boolInt(e.Deactivated),
		DisplayName: optionalString(e.DisplayName),
		AvatarURL:   optionalString(e.AvatarURL),
		CreationTS:  e.CreatedTS,
		AppService:  optionalString(e.AppService),
	}
	if e.AccountType == userapi.AccountTypeAppService {
		appservice := "appservice"
		u.UserType = &appservice
	}
	return u
}

// parseBoolParam reads an optional boolean parameter.
func parseBoolParam(req *http.Request, name string, def bool) (bool, *util.JSONResponse) {
	v := req.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(name + " must be true or false")}
	}
	return b, nil
}

// SynapseAdminListUsers implements GET /_synapse/admin/v2/users.
func SynapseAdminListUsers(req *http.Request, userAPI userapi.ClientUserAPI) util.JSONResponse {
	q := req.URL.Query()
	filter := userapi.AccountListFilter{Name: q.Get("name"), UserID: q.Get("user_id"), OrderBy: q.Get("order_by")}
	if _, ok := map[string]bool{"": true, "name": true, "creation_ts": true, "displayname": true, "admin": true, "guest": true, "deactivated": true}[filter.OrderBy]; !ok {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("unknown order_by")}
	}
	switch q.Get("dir") {
	case "", "f":
	case "b":
		filter.Backwards = true
	default:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("dir must be f or b")}
	}
	var resp *util.JSONResponse
	if filter.Guests, resp = parseBoolParam(req, "guests", true); resp != nil {
		return *resp
	}
	if filter.Deactivated, resp = parseBoolParam(req, "deactivated", false); resp != nil {
		return *resp
	}
	for name, dst := range map[string]*int{"from": &filter.From, "limit": &filter.Limit} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(name + " must be a non-negative integer")}
			}
			*dst = n
		}
	}
	if filter.Limit == 0 {
		filter.Limit = 100
	}

	entries, total, err := userAPI.AdminListAccounts(req.Context(), filter)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("AdminListAccounts failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	users := make([]synapseUser, 0, len(entries))
	for _, e := range entries {
		users = append(users, synapseUserFromEntry(e))
	}
	body := map[string]any{"users": users, "total": total}
	if next := filter.From + len(entries); next < total {
		body["next_token"] = strconv.Itoa(next)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}

// localAdminUser reads the {userId} of a request and checks that it is a user of this server.
func localAdminUser(req *http.Request, cfg *config.ClientAPI) (userID, localpart string, serverName spec.ServerName, errResp *util.JSONResponse) {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		r := util.ErrorResponse(err)
		return "", "", "", &r
	}
	userID = vars["userId"]
	localpart, serverName, err = cfg.Matrix.SplitLocalID('@', userID)
	if err != nil {
		return "", "", "", &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("This endpoint can only be used with local users")}
	}
	return userID, localpart, serverName, nil
}

// lookupAdminAccount finds the account, or answers 404.
func lookupAdminAccount(req *http.Request, userAPI userapi.ClientUserAPI, localpart string, serverName spec.ServerName) (*userapi.Account, *util.JSONResponse) {
	var res userapi.QueryAccountByLocalpartResponse
	err := userAPI.QueryAccountByLocalpart(req.Context(), &userapi.QueryAccountByLocalpartRequest{Localpart: localpart, ServerName: serverName}, &res)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && res.Account == nil) {
		return nil, &util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("User not found")}
	}
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryAccountByLocalpart failed")
		return nil, &util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return res.Account, nil
}

// synapseUserDetails is the user with what only the single user view has.
type synapseUserDetails struct {
	synapseUser
	ThreePIDs   []synapseThreePID `json:"threepids"`
	ExternalIDs []any             `json:"external_ids"`
}

type synapseThreePID struct {
	Medium      string `json:"medium"`
	Address     string `json:"address"`
	AddedAt     int64  `json:"added_at"`
	ValidatedAt int64  `json:"validated_at"`
}

func adminUserDetails(req *http.Request, userAPI userapi.ClientUserAPI, acc *userapi.Account) (*synapseUserDetails, error) {
	entries, _, err := userAPI.AdminListAccounts(req.Context(), userapi.AccountListFilter{
		UserID: acc.UserID, Guests: true, Deactivated: true, Limit: 50,
	})
	if err != nil {
		return nil, err
	}
	var entry *userapi.AccountListEntry
	for i := range entries {
		if entries[i].UserID == acc.UserID {
			entry = &entries[i]
		}
	}
	if entry == nil {
		return nil, sql.ErrNoRows
	}
	var threePIDs userapi.QueryThreePIDsForLocalpartResponse
	if err = userAPI.QueryThreePIDsForLocalpart(req.Context(), &userapi.QueryThreePIDsForLocalpartRequest{Localpart: acc.Localpart, ServerName: acc.ServerName}, &threePIDs); err != nil {
		return nil, err
	}
	out := &synapseUserDetails{synapseUser: synapseUserFromEntry(*entry), ThreePIDs: []synapseThreePID{}, ExternalIDs: []any{}}
	for _, t := range threePIDs.ThreePIDs {
		out.ThreePIDs = append(out.ThreePIDs, synapseThreePID{Medium: t.Medium, Address: t.Address})
	}
	return out, nil
}

// SynapseAdminGetUser implements GET /_synapse/admin/v2/users/{userId}.
func SynapseAdminGetUser(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	_, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	acc, errResp := lookupAdminAccount(req, userAPI, localpart, serverName)
	if errResp != nil {
		return *errResp
	}
	details, err := adminUserDetails(req, userAPI, acc)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("adminUserDetails failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: details}
}

// SynapseAdminPutUser implements PUT /_synapse/admin/v2/users/{userId}: it creates the user, or changes it.
// nolint:gocyclo
func SynapseAdminPutUser(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	userID, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	var body struct {
		Password    *string `json:"password"`
		DisplayName *string `json:"displayname"`
		AvatarURL   *string `json:"avatar_url"`
		Admin       *bool   `json:"admin"`
		Deactivated *bool   `json:"deactivated"`
		// LogoutDevices only applies when the password is set.
		LogoutDevices *bool `json:"logout_devices"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
	}
	if cfg.MSC3861.Enabled && (body.Password != nil || body.Deactivated != nil) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.Unknown("Passwords and deactivation are managed by the authentication service")}
	}
	if body.Password != nil && *body.Password != "" {
		if err := internal.ValidatePassword(*body.Password); err != nil {
			return *internal.PasswordResponse(err)
		}
	}

	acc, notFound := lookupAdminAccount(req, userAPI, localpart, serverName)
	created := false
	if notFound != nil {
		if notFound.Code != http.StatusNotFound {
			return *notFound
		}
		if err := internal.ValidateUsername(localpart, serverName); err != nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidUsername(err.Error())}
		}
		accountType := userapi.AccountTypeUser
		if body.Admin != nil && *body.Admin {
			accountType = userapi.AccountTypeAdmin
		}
		password := ""
		if body.Password != nil {
			password = *body.Password
		}
		var res userapi.PerformAccountCreationResponse
		if err := userAPI.PerformAccountCreation(req.Context(), &userapi.PerformAccountCreationRequest{
			AccountType: accountType, Localpart: localpart, ServerName: serverName, Password: password, OnConflict: userapi.ConflictAbort,
		}, &res); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("PerformAccountCreation failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		acc, created = res.Account, true
	} else {
		if acc.AccountType == userapi.AccountTypeAppService || acc.AccountType == userapi.AccountTypeGuest {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Guests and appservice users cannot be changed with this endpoint")}
		}
		if body.Password != nil && *body.Password != "" {
			logout := body.LogoutDevices == nil || *body.LogoutDevices
			if err := userAPI.PerformPasswordUpdate(req.Context(), &userapi.PerformPasswordUpdateRequest{
				Localpart: localpart, ServerName: serverName, Password: *body.Password, LogoutDevices: logout,
			}, &userapi.PerformPasswordUpdateResponse{}); err != nil {
				return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.Unknown("Failed to update the password: " + err.Error())}
			}
		}
		if body.Admin != nil && (acc.AccountType == userapi.AccountTypeAdmin) != *body.Admin {
			newType := userapi.AccountTypeUser
			if *body.Admin {
				newType = userapi.AccountTypeAdmin
			}
			if err := userAPI.AdminSetAccountType(req.Context(), localpart, serverName, newType); err != nil {
				util.GetLogger(req.Context()).WithError(err).Error("AdminSetAccountType failed")
				return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
			}
			acc.AccountType = newType
		}
	}

	if body.DisplayName != nil {
		if _, _, err := userAPI.SetDisplayName(req.Context(), localpart, serverName, *body.DisplayName); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("SetDisplayName failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	}
	if body.AvatarURL != nil {
		if _, _, err := userAPI.SetAvatarURL(req.Context(), localpart, serverName, *body.AvatarURL); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("SetAvatarURL failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	}
	if body.Deactivated != nil && *body.Deactivated != acc.Deactivated {
		var err error
		if *body.Deactivated {
			err = userAPI.PerformAccountDeactivation(req.Context(), &userapi.PerformAccountDeactivationRequest{Localpart: localpart, ServerName: serverName}, &userapi.PerformAccountDeactivationResponse{})
		} else {
			err = userAPI.PerformAccountReactivation(req.Context(), &userapi.PerformAccountReactivationRequest{Localpart: localpart, ServerName: serverName}, &userapi.PerformAccountReactivationResponse{})
		}
		if err != nil {
			util.GetLogger(req.Context()).WithError(err).WithField("user_id", userID).Error("changing the deactivation of an account failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	}

	acc, notFound = lookupAdminAccount(req, userAPI, localpart, serverName)
	if notFound != nil {
		return *notFound
	}
	details, err := adminUserDetails(req, userAPI, acc)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("adminUserDetails failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	return util.JSONResponse{Code: code, JSON: details}
}

// SynapseAdminDeactivate implements POST /_synapse/admin/v1/deactivate/{userId}.
func SynapseAdminDeactivate(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	if cfg.MSC3861.Enabled {
		return delegatedAdminDisabled()
	}
	_, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	if _, errResp = lookupAdminAccount(req, userAPI, localpart, serverName); errResp != nil {
		return *errResp
	}
	if err := userAPI.PerformAccountDeactivation(req.Context(), &userapi.PerformAccountDeactivationRequest{Localpart: localpart, ServerName: serverName}, &userapi.PerformAccountDeactivationResponse{}); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("PerformAccountDeactivation failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"id_server_unbind_result": "no-support"}}
}

// SynapseAdminResetPassword implements POST /_synapse/admin/v1/reset_password/{userId}.
func SynapseAdminResetPassword(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	if cfg.MSC3861.Enabled {
		return delegatedAdminDisabled()
	}
	_, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	var body struct {
		NewPassword   string `json:"new_password"`
		LogoutDevices *bool  `json:"logout_devices"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
	}
	if body.NewPassword == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("new_password")}
	}
	if err := internal.ValidatePassword(body.NewPassword); err != nil {
		return *internal.PasswordResponse(err)
	}
	if _, errResp = lookupAdminAccount(req, userAPI, localpart, serverName); errResp != nil {
		return *errResp
	}
	if err := userAPI.PerformPasswordUpdate(req.Context(), &userapi.PerformPasswordUpdateRequest{
		Localpart: localpart, ServerName: serverName, Password: body.NewPassword, LogoutDevices: body.LogoutDevices == nil || *body.LogoutDevices,
	}, &userapi.PerformPasswordUpdateResponse{}); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("PerformPasswordUpdate failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

type synapseDevice struct {
	DeviceID    string  `json:"device_id"`
	DisplayName *string `json:"display_name"`
	LastSeenIP  *string `json:"last_seen_ip"`
	LastSeenTS  *int64  `json:"last_seen_ts"`
	UserAgent   *string `json:"last_seen_user_agent,omitempty"`
}

// SynapseAdminListDevices implements GET /_synapse/admin/v2/users/{userId}/devices.
func SynapseAdminListDevices(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	userID, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	if _, errResp = lookupAdminAccount(req, userAPI, localpart, serverName); errResp != nil {
		return *errResp
	}
	var res userapi.QueryDevicesResponse
	if err := userAPI.QueryDevices(req.Context(), &userapi.QueryDevicesRequest{UserID: userID}, &res); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryDevices failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	devices := make([]synapseDevice, 0, len(res.Devices))
	for _, d := range res.Devices {
		dev := synapseDevice{DeviceID: d.ID, DisplayName: optionalString(d.DisplayName), LastSeenIP: optionalString(d.LastSeenIP), UserAgent: optionalString(d.UserAgent)}
		if d.LastSeenTS != 0 {
			ts := d.LastSeenTS
			dev.LastSeenTS = &ts
		}
		devices = append(devices, dev)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"devices": devices, "total": len(devices)}}
}

// SynapseAdminDeleteDevices implements DELETE /_synapse/admin/v2/users/{userId}/devices/{deviceId} and
// POST /_synapse/admin/v2/users/{userId}/delete_devices.
func SynapseAdminDeleteDevices(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	userID, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	if _, errResp = lookupAdminAccount(req, userAPI, localpart, serverName); errResp != nil {
		return *errResp
	}
	var ids []string
	if deviceID := mux.Vars(req)["deviceId"]; deviceID != "" {
		decoded, err := httputil.URLDecodeMapValues(map[string]string{"d": deviceID})
		if err != nil {
			return util.ErrorResponse(err)
		}
		ids = []string{decoded["d"]}
	} else {
		var body struct {
			Devices []string `json:"devices"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body.Devices) == 0 {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("devices must be a list of device IDs")}
		}
		ids = body.Devices
	}
	if err := userAPI.PerformDeviceDeletion(req.Context(), &userapi.PerformDeviceDeletionRequest{UserID: userID, DeviceIDs: ids}, &userapi.PerformDeviceDeletionResponse{}); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("PerformDeviceDeletion failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

// SynapseAdminJoinedRooms implements GET /_synapse/admin/v1/users/{userId}/joined_rooms.
func SynapseAdminJoinedRooms(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI, rsAPI roomserverAPI.ClientRoomserverAPI) util.JSONResponse {
	userID, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	if _, errResp = lookupAdminAccount(req, userAPI, localpart, serverName); errResp != nil {
		return *errResp
	}
	user, err := spec.NewUserID(userID, true)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid user ID")}
	}
	rooms, err := rsAPI.QueryRoomsForUser(req.Context(), *user, "join")
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryRoomsForUser failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	ids := make([]string, 0, len(rooms))
	for _, r := range rooms {
		ids = append(ids, r.String())
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"joined_rooms": ids, "total": len(ids)}}
}

// delegatedAdminDisabled is the answer of endpoints that the authentication service has taken over (MSC3861).
func delegatedAdminDisabled() util.JSONResponse {
	return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.Unrecognized("This is managed by the authentication service")}
}
