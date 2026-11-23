// Copyright 2024 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/clientapi/httputil"
	"github.com/element-hq/dendrite/clientapi/producers"
	"github.com/element-hq/dendrite/internal/transactions"
	userapi "github.com/element-hq/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// SendToDevice handles PUT /_matrix/client/r0/sendToDevice/{eventType}/{txnId}
// sends the device events to the syncapi & federationsender
func SendToDevice(
	req *http.Request, device *userapi.Device,
	syncProducer *producers.SyncAPIProducer,
	txnCache *transactions.Cache,
	eventType string, txnID *string,
	maxMessages int,
) util.JSONResponse {
	if txnID != nil {
		if res, ok := txnCache.FetchTransaction(device.AccessToken, *txnID, req.URL); ok {
			return *res
		}
	}

	var httpReq struct {
		Messages map[string]map[string]json.RawMessage `json:"messages"`
	}
	resErr := httputil.UnmarshalJSONRequest(req, &httpReq)
	if resErr != nil {
		return *resErr
	}

	// One call can reach every device of many users (a device ID of * means all of them), so the size of a request
	// is bounded: a call with many participants sends its keys to each of them, which is what this is for, but nobody
	// needs a million.
	if maxMessages > 0 {
		count := 0
		for _, byUser := range httpReq.Messages {
			count += len(byUser)
		}
		if count > maxMessages {
			return util.JSONResponse{
				Code: http.StatusRequestEntityTooLarge,
				JSON: spec.MatrixError{ErrCode: "M_TOO_LARGE", Err: fmt.Sprintf("Too many to-device messages in one request (%d, at most %d)", count, maxMessages)},
			}
		}
	}

	for userID, byUser := range httpReq.Messages {
		for deviceID, message := range byUser {
			if err := syncProducer.SendToDevice(
				req.Context(), device.UserID, userID, deviceID, eventType, message,
			); err != nil {
				util.GetLogger(req.Context()).WithError(err).Error("eduProducer.SendToDevice failed")
				return util.JSONResponse{
					Code: http.StatusInternalServerError,
					JSON: spec.InternalServerError{},
				}
			}
		}
	}

	res := util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}

	if txnID != nil {
		txnCache.AddTransaction(device.AccessToken, *txnID, req.URL, &res)
	}

	return res
}
