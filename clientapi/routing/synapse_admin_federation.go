// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/federationapi/api"
	"github.com/element-hq/dendrite/internal/httputil"
)

// The federation destinations of the Synapse admin API:
// https://element-hq.github.io/synapse/latest/usage/administration/admin_api/federation.html
//
// What Dendrite knows lives in memory: the servers that this one has sent to, or looked at, since it started, and for
// each how the failures stand. Only the blacklist and the assumed offline flag are in the database.

type synapseDestination struct {
	Destination string `json:"destination"`
	// RetryLastTS is when the last attempt failed, RetryInterval how long the server waits before the next one.
	RetryLastTS   int64  `json:"retry_last_ts"`
	RetryInterval int64  `json:"retry_interval"`
	FailureTS     *int64 `json:"failure_ts"`
	// Dendrite has no stream ordering of events per destination.
	LastSuccessfulStreamOrdering *int64 `json:"last_successful_stream_ordering"`
	// Not in Synapse's answer.
	Blacklisted    bool   `json:"blacklisted"`
	AssumedOffline bool   `json:"assumed_offline"`
	FailureCount   uint32 `json:"failure_count"`
	SuccessCount   uint32 `json:"success_count"`
}

func synapseDestinationOf(d api.DestinationInfo) synapseDestination {
	out := synapseDestination{
		Destination: string(d.ServerName), RetryLastTS: d.LastFailureMS, RetryInterval: d.BackoffIntervalMS,
		Blacklisted: d.Blacklisted, AssumedOffline: d.AssumedOffline, FailureCount: d.FailureCount, SuccessCount: d.SuccessCount,
	}
	if d.FirstFailureMS > 0 {
		out.FailureTS = &d.FirstFailureMS
	}
	return out
}

var destinationOrder = map[string]func(a, b api.DestinationInfo) bool{
	"":                                func(a, b api.DestinationInfo) bool { return a.ServerName < b.ServerName },
	"destination":                     func(a, b api.DestinationInfo) bool { return a.ServerName < b.ServerName },
	"retry_last_ts":                   func(a, b api.DestinationInfo) bool { return a.LastFailureMS < b.LastFailureMS },
	"retry_interval":                  func(a, b api.DestinationInfo) bool { return a.BackoffIntervalMS < b.BackoffIntervalMS },
	"failure_ts":                      func(a, b api.DestinationInfo) bool { return a.FirstFailureMS < b.FirstFailureMS },
	"last_successful_stream_ordering": func(a, b api.DestinationInfo) bool { return a.ServerName < b.ServerName },
}

// SynapseAdminListDestinations implements GET /_synapse/admin/v1/federation/destinations.
func SynapseAdminListDestinations(req *http.Request, fsAPI api.ClientFederationAPI) util.JSONResponse {
	q := req.URL.Query()
	less, ok := destinationOrder[q.Get("order_by")]
	if !ok {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("unknown order_by")}
	}
	backwards := false
	switch q.Get("dir") {
	case "", "f":
	case "b":
		backwards = true
	default:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("dir must be f or b")}
	}
	from, limit := 0, 100
	for name, dst := range map[string]*int{"from": &from, "limit": &limit} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(name + " must be a non-negative integer")}
			}
			*dst = n
		}
	}
	all, err := fsAPI.QueryDestinations(req.Context())
	if err != nil {
		return adminServerError(req, "QueryDestinations", err)
	}
	search := strings.ToLower(q.Get("destination"))
	var matching []api.DestinationInfo
	for _, d := range all {
		if search == "" || strings.Contains(strings.ToLower(string(d.ServerName)), search) {
			matching = append(matching, d)
		}
	}
	sort.SliceStable(matching, func(i, j int) bool {
		if backwards {
			return less(matching[j], matching[i])
		}
		return less(matching[i], matching[j])
	})
	page := []synapseDestination{}
	for i := from; i < len(matching) && i < from+limit; i++ {
		page = append(page, synapseDestinationOf(matching[i]))
	}
	body := map[string]any{"destinations": page, "total": len(matching)}
	if next := from + len(page); next < len(matching) {
		body["next_token"] = strconv.Itoa(next)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}

// knownDestination finds the destination, or answers 404: a server that was never contacted and has nothing recorded in
// the database is not a destination.
func knownDestination(req *http.Request, fsAPI api.ClientFederationAPI) (api.DestinationInfo, *util.JSONResponse) {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		r := util.ErrorResponse(err)
		return api.DestinationInfo{}, &r
	}
	name := spec.ServerName(vars["destination"])
	notFound := &util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown destination")}
	if name == "" {
		return api.DestinationInfo{}, notFound
	}
	known, err := fsAPI.QueryDestinations(req.Context())
	if err != nil {
		r := adminServerError(req, "QueryDestinations", err)
		return api.DestinationInfo{}, &r
	}
	for _, d := range known {
		if d.ServerName == name {
			return d, nil
		}
	}
	d, err := fsAPI.QueryDestination(req.Context(), name)
	if err != nil {
		r := adminServerError(req, "QueryDestination", err)
		return api.DestinationInfo{}, &r
	}
	if !d.Blacklisted && !d.AssumedOffline {
		return api.DestinationInfo{}, notFound
	}
	return d, nil
}

// SynapseAdminGetDestination implements GET /_synapse/admin/v1/federation/destinations/{destination}.
func SynapseAdminGetDestination(req *http.Request, fsAPI api.ClientFederationAPI) util.JSONResponse {
	d, errResp := knownDestination(req, fsAPI)
	if errResp != nil {
		return *errResp
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: synapseDestinationOf(d)}
}

// SynapseAdminResetDestination implements POST /_synapse/admin/v1/federation/destinations/{destination}/reset_connection:
// the failures are forgotten and the server is taken off the blacklist, so that the next attempt is made at once.
func SynapseAdminResetDestination(req *http.Request, fsAPI api.ClientFederationAPI) util.JSONResponse {
	d, errResp := knownDestination(req, fsAPI)
	if errResp != nil {
		return *errResp
	}
	if _, err := fsAPI.ResetDestination(req.Context(), d.ServerName); err != nil {
		return adminServerError(req, "ResetDestination", err)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}
