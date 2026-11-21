package routing

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/internal"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

const (
	defaultThreadsLimit = 20
	maxThreadsLimit     = 100
	// maxLatestCandidates bounds how many replies are looked at to find the latest one the user may see.
	maxLatestCandidates = 20
)

// ThreadsResponse is the response of the threads list (MSC3856).
type ThreadsResponse struct {
	Chunk     []synctypes.ClientEvent `json:"chunk"`
	NextBatch string                  `json:"next_batch,omitempty"`
}

// Threads handles GET /rooms/{roomId}/threads (MSC3856): the roots of the threads of a room, the ones with the
// latest reply first, each with the bundled aggregation of its thread.
//
// nolint:gocyclo
func Threads(
	req *http.Request, device *userapi.Device,
	syncDB storage.Database, rsAPI api.SyncRoomserverAPI,
	rawRoomID string,
) util.JSONResponse {
	ctx := req.Context()
	roomID, err := spec.NewRoomID(rawRoomID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid room ID")}
	}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("device.UserID invalid")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}

	query := req.URL.Query()
	onlyParticipated := false
	switch include := query.Get("include"); include {
	case "", "all":
	case "participated":
		onlyParticipated = true
	default:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("include must be all or participated")}
	}
	limit := defaultThreadsLimit
	if l := query.Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit <= 0 {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("limit must be a positive integer")}
		}
		limit = min(limit, maxThreadsLimit)
	}
	before := int64(1) << 62
	if from := query.Get("from"); from != "" {
		if before, err = strconv.ParseInt(from, 10, 64); err != nil || before <= 0 {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid from token")}
		}
	}

	// Like /messages, this is for people who can see the history of the room.
	membership, err := getMembershipForUser(ctx, roomID.String(), device.UserID, rsAPI)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("getMembershipForUser failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !membership.RoomExists {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("room does not exist")}
	}
	if membership.IsRoomForgotten {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("user already forgot about this room")}
	}

	snapshot, err := syncDB.NewDatabaseSnapshot(ctx)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("failed to get a snapshot for threads")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	var succeeded bool
	defer sqlutil.EndTransactionWithCheck(snapshot, &succeeded, &err)

	if !membership.IsInRoom && !membership.HasBeenInRoom {
		visibility, vErr := snapshot.GetStateEvent(ctx, roomID.String(), spec.MRoomHistoryVisibility, "")
		if vErr != nil && !errors.Is(vErr, sql.ErrNoRows) {
			err = vErr
			return util.ErrorResponse(err)
		}
		if visibility == nil || gjson.GetBytes(visibility.Content(), "history_visibility").String() != "world_readable" {
			return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("you cannot see the history of this room")}
		}
	}

	entries, err := snapshot.Threads(ctx, roomID.String(), device.UserID, onlyParticipated, before, limit+1)
	if err != nil {
		return util.ErrorResponse(err)
	}
	res := ThreadsResponse{Chunk: []synctypes.ClientEvent{}}
	if len(entries) > limit {
		entries = entries[:limit]
		res.NextBatch = strconv.FormatInt(entries[len(entries)-1].LatestRelationID, 10)
	}

	ids := make([]string, 0, 2*len(entries))
	for _, e := range entries {
		ids = append(ids, e.RootEventID)
		if e.LatestEventID != "" {
			ids = append(ids, e.LatestEventID)
		}
	}
	loaded, err := snapshot.Events(ctx, ids)
	if err != nil {
		return util.ErrorResponse(err)
	}
	byID := make(map[string]*rstypes.HeaderedEvent, len(loaded))
	for _, ev := range loaded {
		byID[ev.EventID()] = ev
	}
	ignored, err := ignoredSenders(snapshot, req, device.UserID)
	if err != nil {
		return util.ErrorResponse(err)
	}

	// The count is of the replies the user has not ignored, as in the bundles of /event, /messages and /context.
	counts := map[string]int{}
	if len(ignored) > 0 && len(entries) > 0 {
		roots := make([]string, 0, len(entries))
		for _, e := range entries {
			roots = append(roots, e.RootEventID)
		}
		bundles, berr := snapshot.RelationBundles(ctx, roomID.String(), roots, device.UserID, true)
		if berr != nil {
			return util.ErrorResponse(berr)
		}
		for id := range bundles.Threads {
			for _, child := range bundles.ThreadChildren[id] {
				if !ignored[child.Sender] {
					counts[id]++
				}
			}
		}
	}

	// One pass for the roots and the latest replies, the replies of other users may need a second look below.
	visible, err := visibleEvents(req, snapshot, rsAPI, *userID, "threads", loaded)
	if err != nil {
		return util.ErrorResponse(err)
	}

	toClient := func(ev *rstypes.HeaderedEvent) (*synctypes.ClientEvent, error) {
		return synctypes.ToClientEvent(ev.PDU, synctypes.FormatAll, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
			return rsAPI.QueryUserIDForSender(ctx, roomID, senderID)
		})
	}
	for _, e := range entries {
		root := byID[e.RootEventID]
		if root == nil || !visible[e.RootEventID] {
			continue
		}
		latest := byID[e.LatestEventID]
		if latest == nil || !visible[e.LatestEventID] || ignored[string(latest.SenderID())] {
			// The latest reply is not for this user to see: show the latest one that is, without moving the thread.
			var err2 error
			if latest, err2 = latestVisibleReply(req, snapshot, rsAPI, *userID, roomID.String(), e.RootEventID, ignored); err2 != nil {
				return util.ErrorResponse(err2)
			}
			if latest == nil {
				continue
			}
		}
		if ignored[string(root.SenderID())] {
			// What the client would have if it had put the list together itself.
			root.Redact()
		}
		latestClient, err2 := toClient(latest)
		if err2 != nil {
			util.GetLogger(ctx).WithError(err2).WithField("event_id", latest.EventID()).Error("failed converting a thread reply")
			continue
		}
		rootClient, err2 := toClient(root)
		if err2 != nil {
			util.GetLogger(ctx).WithError(err2).WithField("event_id", root.EventID()).Error("failed converting a thread root")
			continue
		}
		count := e.Count
		if len(ignored) > 0 {
			count = counts[e.RootEventID]
		}
		rootClient.Unsigned = withThreadSummary(rootClient.Unsigned, *latestClient, count, e.Participated)
		res.Chunk = append(res.Chunk, *rootClient)
	}

	succeeded = true
	return util.JSONResponse{Code: http.StatusOK, JSON: res}
}

// withThreadSummary adds the bundled aggregation of a thread to the unsigned data of its root.
func withThreadSummary(unsigned []byte, latest synctypes.ClientEvent, count int, participated bool) []byte {
	if len(unsigned) == 0 {
		unsigned = []byte("{}")
	}
	out, err := sjson.SetBytes(unsigned, `m\.relations.m\.thread`, map[string]any{
		"latest_event":              latest,
		"count":                     count,
		"current_user_participated": participated,
	})
	if err != nil {
		return unsigned
	}
	return out
}

// ignoredSenders returns the users the user ignores.
func ignoredSenders(snapshot storage.DatabaseTransaction, req *http.Request, userID string) (map[string]bool, error) {
	ignores, err := snapshot.IgnoresForUser(req.Context(), userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	out := make(map[string]bool, len(ignores.List))
	for user := range ignores.List {
		out[user] = true
	}
	return out, nil
}

// visibleEvents returns the IDs of the events the user may see, by the history visibility of the room.
func visibleEvents(
	req *http.Request, snapshot storage.DatabaseTransaction, rsAPI api.SyncRoomserverAPI,
	userID spec.UserID, endpoint string, events []*rstypes.HeaderedEvent,
) (map[string]bool, error) {
	filtered, err := internal.ApplyHistoryVisibilityFilter(req.Context(), snapshot, rsAPI, events, nil, userID, endpoint)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]bool, len(filtered))
	for _, ev := range filtered {
		visible[ev.EventID()] = true
	}
	return visible, nil
}

// latestVisibleReply returns the newest reply in a thread that the user may see and has not ignored, or nil.
func latestVisibleReply(
	req *http.Request, snapshot storage.DatabaseTransaction, rsAPI api.SyncRoomserverAPI,
	userID spec.UserID, roomID, rootEventID string, ignored map[string]bool,
) (*rstypes.HeaderedEvent, error) {
	replies, _, _, err := snapshot.RelationsFor(req.Context(), roomID, rootEventID, "m.thread", "", 0, 0, true, maxLatestCandidates)
	if err != nil {
		return nil, err
	}
	events := make([]*rstypes.HeaderedEvent, 0, len(replies))
	for _, r := range replies {
		events = append(events, r.HeaderedEvent)
	}
	visible, err := visibleEvents(req, snapshot, rsAPI, userID, "threads", events)
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if visible[ev.EventID()] && !ignored[string(ev.SenderID())] {
			return ev, nil
		}
	}
	return nil, nil
}
