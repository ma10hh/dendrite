// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"math"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// What an administrator can restrict about one account, in the Synapse admin API:
// shadow bans, rate limit overrides and the expiry of the account.

// restrictedAccount reads the {userId} of the request and finds the local account.
func restrictedAccount(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) (acc *userapi.Account, errResp *util.JSONResponse) {
	_, localpart, serverName, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return nil, errResp
	}
	return lookupAdminAccount(req, userAPI, localpart, serverName)
}

func adminServerError(req *http.Request, what string, err error) util.JSONResponse {
	util.GetLogger(req.Context()).WithError(err).Error(what + " failed")
	return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
}

// SynapseAdminShadowBan implements POST and DELETE /_synapse/admin/v1/users/{userId}/shadow_ban.
func SynapseAdminShadowBan(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	acc, errResp := restrictedAccount(req, cfg, userAPI)
	if errResp != nil {
		return *errResp
	}
	if acc.AccountType == userapi.AccountTypeAdmin || acc.AccountType == userapi.AccountTypeAppService {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Administrators and application services cannot be shadow banned")}
	}
	if err := userAPI.AdminSetShadowBan(req.Context(), acc.Localpart, acc.ServerName, req.Method == http.MethodPost); err != nil {
		return adminServerError(req, "AdminSetShadowBan", err)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

type rateLimitBody struct {
	MessagesPerSecond *float64 `json:"messages_per_second"`
	BurstCount        *int     `json:"burst_count"`
}

// SynapseAdminRateLimit implements GET, POST and DELETE /_synapse/admin/v1/users/{userId}/override_ratelimit.
func SynapseAdminRateLimit(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	acc, errResp := restrictedAccount(req, cfg, userAPI)
	if errResp != nil {
		return *errResp
	}
	ctx := req.Context()
	switch req.Method {
	case http.MethodPost:
		var body rateLimitBody
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
		}
		limit := userapi.RateLimitOverride{}
		if body.MessagesPerSecond != nil {
			limit.MessagesPerSecond = *body.MessagesPerSecond
		}
		if body.BurstCount != nil {
			limit.BurstCount = *body.BurstCount
		}
		if limit.MessagesPerSecond < 0 || math.IsNaN(limit.MessagesPerSecond) || math.IsInf(limit.MessagesPerSecond, 0) || limit.BurstCount < 0 {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("messages_per_second and burst_count must not be negative")}
		}
		if (limit.MessagesPerSecond == 0) != (limit.BurstCount == 0) {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Both must be above 0, or both 0 for no limit")}
		}
		if err := userAPI.AdminSetRateLimitOverride(ctx, acc.Localpart, acc.ServerName, &limit); err != nil {
			return adminServerError(req, "AdminSetRateLimitOverride", err)
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: limit}
	case http.MethodDelete:
		if err := userAPI.AdminSetRateLimitOverride(ctx, acc.Localpart, acc.ServerName, nil); err != nil {
			return adminServerError(req, "AdminSetRateLimitOverride", err)
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
	default:
		r, err := userAPI.AdminAccountRestrictions(ctx, acc.Localpart, acc.ServerName)
		if err != nil {
			return adminServerError(req, "AdminAccountRestrictions", err)
		}
		if r.RateLimit == nil {
			return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: r.RateLimit}
	}
}

// SynapseAdminAccountValidity implements POST /_synapse/admin/v1/account_validity/validity: it sets the time at which
// an account expires. There are no renewal emails or tokens: only an administrator renews an account.
func SynapseAdminAccountValidity(req *http.Request, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI) util.JSONResponse {
	var body struct {
		UserID        string `json:"user_id"`
		ExpirationTS  *int64 `json:"expiration_ts"`
		RenewalEmails *bool  `json:"enable_renewal_emails"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
	}
	if body.UserID == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("user_id is missing")}
	}
	if body.ExpirationTS == nil || *body.ExpirationTS < 0 {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("expiration_ts must be a time in milliseconds, 0 for never")}
	}
	if body.RenewalEmails != nil && *body.RenewalEmails {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Renewal emails are not supported")}
	}
	localpart, serverName, err := cfg.Matrix.SplitLocalID('@', body.UserID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("This endpoint can only be used with local users")}
	}
	acc, errResp := lookupAdminAccount(req, userAPI, localpart, serverName)
	if errResp != nil {
		return *errResp
	}
	if acc.AccountType == userapi.AccountTypeAdmin || acc.AccountType == userapi.AccountTypeAppService {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Administrators and application services do not expire")}
	}
	if err = userAPI.AdminSetAccountExpiry(req.Context(), localpart, serverName, *body.ExpirationTS); err != nil {
		return adminServerError(req, "AdminSetAccountExpiry", err)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]int64{"expiration_ts": *body.ExpirationTS}}
}

// SynapseAdminUserByThreePID implements GET /_synapse/admin/v1/threepid/{medium}/users/{address}.
func SynapseAdminUserByThreePID(req *http.Request, userAPI userapi.ClientUserAPI) util.JSONResponse {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		return util.ErrorResponse(err)
	}
	var res userapi.QueryLocalpartForThreePIDResponse
	if err = userAPI.QueryLocalpartForThreePID(req.Context(), &userapi.QueryLocalpartForThreePIDRequest{
		ThreePID: vars["address"], Medium: vars["medium"],
	}, &res); err != nil {
		return adminServerError(req, "QueryLocalpartForThreePID", err)
	}
	if res.Localpart == "" {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("User not found")}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"user_id": "@" + res.Localpart + ":" + string(res.ServerName)}}
}
