package routing

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/clientapi/httputil"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
)

type PublicRoomReq struct {
	Since              string `json:"since,omitempty"`
	Limit              int16  `json:"limit,omitempty"`
	Filter             filter `json:"filter,omitempty"`
	IncludeAllNetworks bool   `json:"include_all_networks,omitempty"`
	NetworkID          string `json:"third_party_instance_id,omitempty"`
}

type filter struct {
	SearchTerms string   `json:"generic_search_term,omitempty"`
	RoomTypes   []string `json:"room_types,omitempty"`
}

// GetPostPublicRooms implements GET and POST /publicRooms
func GetPostPublicRooms(req *http.Request, rsAPI roomserverAPI.FederationRoomserverAPI) util.JSONResponse {
	var request PublicRoomReq
	if fillErr := fillPublicRoomsReq(req, &request); fillErr != nil {
		return *fillErr
	}
	if request.Limit == 0 {
		request.Limit = 50
	}
	response, err := publicRooms(req.Context(), request, rsAPI)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: response,
	}
}

func publicRooms(
	ctx context.Context, request PublicRoomReq, rsAPI roomserverAPI.FederationRoomserverAPI,
) (*fclient.RespPublicRooms, error) {

	var response fclient.RespPublicRooms
	var limit int16
	var offset int64
	limit = request.Limit
	offset, err := strconv.ParseInt(request.Since, 10, 64)
	// ParseInt returns 0 and an error when trying to parse an empty string
	// In that case, we want to assign 0 so we ignore the error
	if err != nil && len(request.Since) > 0 {
		util.GetLogger(ctx).WithError(err).Error("strconv.ParseInt failed")
		return nil, err
	}

	if request.IncludeAllNetworks && request.NetworkID != "" {
		return nil, fmt.Errorf("include_all_networks and third_party_instance_id can not be used together")
	}

	var queryRes roomserverAPI.QueryPublishedRoomsResponse
	err = rsAPI.QueryPublishedRooms(ctx, &roomserverAPI.QueryPublishedRoomsRequest{
		NetworkID: request.NetworkID,
	}, &queryRes)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryPublishedRooms failed")
		return nil, err
	}
	// A search term or room types are matched against the state of the rooms, so every room has to be looked at.
	filtering := request.Filter.SearchTerms != "" || len(request.Filter.RoomTypes) > 0
	total := len(queryRes.RoomIDs)
	var matching []fclient.PublicRoom
	if filtering {
		if matching, err = roomserverAPI.PopulatePublicRooms(ctx, queryRes.RoomIDs, rsAPI); err != nil {
			return nil, err
		}
		matching = roomserverAPI.FilterPublicRooms(matching, request.Filter.SearchTerms, request.Filter.RoomTypes)
		total = len(matching)
	}
	response.TotalRoomCountEstimate = total

	if offset > 0 {
		response.PrevBatch = strconv.Itoa(int(offset) - 1)
	}
	nextIndex := int(offset) + int(limit)
	if total > nextIndex {
		response.NextBatch = strconv.Itoa(nextIndex)
	}

	if offset < 0 {
		offset = 0
	}
	nextIndex = min(nextIndex, total)
	offset = min(offset, int64(nextIndex))
	if filtering {
		response.Chunk = matching[offset:nextIndex]
		return &response, nil
	}
	response.Chunk, err = roomserverAPI.PopulatePublicRooms(ctx, queryRes.RoomIDs[offset:nextIndex], rsAPI)
	return &response, err
}

// fillPublicRoomsReq fills the Limit, Since and Filter attributes of a GET or POST request
// on /publicRooms by parsing the incoming HTTP request
// Filter is only filled for POST requests
func fillPublicRoomsReq(httpReq *http.Request, request *PublicRoomReq) *util.JSONResponse {
	if httpReq.Method == http.MethodGet {
		limit, err := strconv.Atoi(httpReq.FormValue("limit"))
		// Atoi returns 0 and an error when trying to parse an empty string
		// In that case, we want to assign 0 so we ignore the error
		if err != nil && len(httpReq.FormValue("limit")) > 0 {
			util.GetLogger(httpReq.Context()).WithError(err).Error("strconv.Atoi failed")
			return &util.JSONResponse{
				Code: http.StatusInternalServerError,
				JSON: spec.InternalServerError{},
			}
		}
		request.Limit = int16(limit)
		request.Since = httpReq.FormValue("since")
		return nil
	} else if httpReq.Method == http.MethodPost {
		return httputil.UnmarshalJSONRequest(httpReq, request)
	}

	return &util.JSONResponse{
		Code: http.StatusMethodNotAllowed,
		JSON: spec.NotFound("Bad method"),
	}
}
