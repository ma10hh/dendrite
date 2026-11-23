// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package httputil

import (
	"net/http"
	"strconv"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// ParseTimestampToEventQuery reads the ts (milliseconds) and dir (f or b) parameters of timestamp_to_event, on the
// client and the federation API. forward is true for dir=f.
func ParseTimestampToEventQuery(req *http.Request) (ts int64, forward bool, errRes *util.JSONResponse) {
	query := req.URL.Query()
	rawTS := query.Get("ts")
	if rawTS == "" {
		return 0, false, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("ts is missing")}
	}
	ts, err := strconv.ParseInt(rawTS, 10, 64)
	if err != nil || ts < 0 {
		return 0, false, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("ts must be a number of milliseconds")}
	}
	switch query.Get("dir") {
	case "f":
		forward = true
	case "b":
	case "":
		return 0, false, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("dir is missing")}
	default:
		return 0, false, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("dir must be f or b")}
	}
	return ts, forward, nil
}

// TimestampToEventResponse is the response of timestamp_to_event, on the client and the federation API.
type TimestampToEventResponse struct {
	EventID        string `json:"event_id"`
	OriginServerTS int64  `json:"origin_server_ts"`
}
