// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/clientapi/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	rsapi "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/syncapi/notifier"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/types"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// ErrorUnknownPos is returned when the client's pos is not (or no longer) known.
const ErrorUnknownPos spec.MatrixErrorCode = "M_UNKNOWN_POS"

const (
	// maxListsAndSubscriptions follows MSC4186 and bounds per-request work.
	maxListsAndSubscriptions = 100
	// maxConnIDLength bounds the size of connection keys held in memory.
	maxConnIDLength = 64
	// maxRangesPerList and maxRequiredState bound the work one list or subscription can ask for.
	maxRangesPerList = 100
	maxRequiredState = 500
	// maxFilterEntries bounds the spaces and tags one filter can name.
	maxFilterEntries = 100
	// maxFilterEntryLength bounds the length of a space ID or a tag in a filter (a room ID is at most 255 bytes).
	maxFilterEntryLength = 255
	// maxExtensionScope bounds the lists and rooms an extension can name.
	maxExtensionScope = 500
	// maxTimeout caps how long a request may wait for changes. It stays below the server's write timeout.
	maxTimeout = 3 * time.Minute
	// lastSeenInterval is how often a connection refreshes the device's last-seen details.
	lastSeenInterval = 5 * time.Minute
)

// Handler serves the sliding sync endpoint.
type Handler struct {
	store    *ConnectionStore
	db       storage.Database
	userAPI  userapi.SyncUserAPI
	rsAPI    rsapi.SyncRoomserverAPI
	notifier *notifier.Notifier
	cfg      *config.SyncAPI
	typing   typingSource
	// shutdown is cancelled when the server stops, which ends waiting requests.
	shutdown context.Context

	// updatePresence applies the `set_presence` parameter of a request. It is nil when presence is not wired up.
	updatePresence func(userID, presence string)

	// fill computes one response for a request. It must not change the connection: it returns a function that
	// does, to be called once the response is going to be sent. It is a field so that session handling can be
	// tested without a database.
	fill func(ctx context.Context, device *userapi.Device, conn *Connection, body *Request, resp *Response, now types.StreamingToken) (commit func(), err error)
}

func NewHandler(
	store *ConnectionStore, db storage.Database, userAPI userapi.SyncUserAPI, rsAPI rsapi.SyncRoomserverAPI,
	notifier *notifier.Notifier, typing typingSource, cfg *config.SyncAPI,
	shutdown context.Context, enableMetrics bool,
) *Handler {
	if shutdown == nil {
		shutdown = context.Background()
	}
	h := &Handler{store: store, db: db, userAPI: userAPI, rsAPI: rsAPI, notifier: notifier, typing: typing, cfg: cfg, shutdown: shutdown}
	if enableMetrics {
		registerMetrics(store)
	}
	h.fill = h.fillResponse
	return h
}

// SetPresenceUpdater makes requests mark the user as online, idle or offline as `set_presence` asks, the way /sync does.
func (h *Handler) SetPresenceUpdater(update func(userID, presence string)) {
	h.updatePresence = update
}

// parseSetPresence reads the `set_presence` query parameter. Like Synapse, it only accepts the values of /sync.
func parseSetPresence(req *http.Request) (string, *util.JSONResponse) {
	switch v := req.URL.Query().Get("set_presence"); v {
	case "", "online", "offline", "unavailable":
		return v, nil
	default:
		res := invalidParam("set_presence must be one of online, offline and unavailable")
		return "", &res
	}
}

func unknownPos(msg string) util.JSONResponse {
	return util.JSONResponse{
		Code: http.StatusBadRequest,
		JSON: spec.MatrixError{ErrCode: ErrorUnknownPos, Err: msg},
	}
}

func invalidParam(msg string) util.JSONResponse {
	return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(msg)}
}

func validate(body *Request) *util.JSONResponse {
	var msg string
	switch {
	case len(body.ConnID) > maxConnIDLength:
		msg = fmt.Sprintf("conn_id must be at most %d bytes", maxConnIDLength)
	case len(body.Lists) > maxListsAndSubscriptions:
		msg = fmt.Sprintf("at most %d lists are allowed", maxListsAndSubscriptions)
	case len(body.RoomSubscriptions) > maxListsAndSubscriptions:
		msg = fmt.Sprintf("at most %d room subscriptions are allowed", maxListsAndSubscriptions)
	case tooLarge(body) != "":
		msg = tooLarge(body)
	default:
		ext, err := parseExtensions(body.Extensions)
		if err != nil {
			res := invalidParam(err.Error())
			return &res
		}
		body.ext = ext
		return nil
	}
	res := invalidParam(msg)
	return &res
}

// parseTimeout reads the `timeout` query parameter in milliseconds. It defaults to 0, meaning "answer now".
func parseTimeout(req *http.Request) (time.Duration, *util.JSONResponse) {
	raw := req.URL.Query().Get("timeout")
	if raw == "" {
		return 0, nil
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms < 0 {
		res := invalidParam("timeout must be a non-negative number of milliseconds")
		return 0, &res
	}
	return min(time.Duration(ms)*time.Millisecond, maxTimeout), nil
}

// OnIncomingRequest handles POST /sync. A request without `pos` starts a new session on the
// (user, device, conn_id) connection; one with `pos` continues it.
func (h *Handler) OnIncomingRequest(req *http.Request, device *userapi.Device) util.JSONResponse {
	var body Request
	if resErr := httputil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	if resErr := validate(&body); resErr != nil {
		return *resErr
	}
	timeout, resErr := parseTimeout(req)
	if resErr != nil {
		return *resErr
	}
	setPresence, resErr := parseSetPresence(req)
	if resErr != nil {
		return *resErr
	}

	var (
		conn       *Connection
		requestSeq uint64
		hasPos     bool
	)
	if pos := req.URL.Query().Get("pos"); pos == "" {
		var err error
		conn, err = h.store.Create(device.UserID, device.ID, body.ConnID)
		if err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("failed to create sliding sync connection")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		// A request that starts a session is answered immediately, whatever it asks for.
		timeout = 0
	} else {
		sessionID, seq, err := ParsePos(pos)
		if err != nil {
			return invalidParam(err.Error())
		}
		requestSeq, hasPos = seq, true
		conn = h.store.Get(device.UserID, device.ID, body.ConnID, sessionID)
		if conn == nil {
			return unknownPos("unknown pos, start a new session")
		}
	}

	if err := conn.Acquire(req.Context()); err != nil {
		return util.JSONResponse{Code: http.StatusRequestTimeout, JSON: spec.Unknown("request cancelled")}
	}
	defer conn.Release()
	defer h.store.Touch(conn)

	if hasPos && requestSeq != conn.pos {
		// The client may not have seen our last response and is repeating the request
		// it sent before it. The MSC requires this to keep working.
		if requestSeq == conn.lastRequestPos && conn.lastResponse != nil {
			return util.JSONResponse{Code: http.StatusOK, JSON: conn.lastResponse}
		}
		return unknownPos("pos does not match the connection state")
	}

	h.noteDeviceActivity(req, device, conn)
	if h.updatePresence != nil {
		h.updatePresence(device.UserID, setPresence)
	}

	activeRequests.Inc()
	defer activeRequests.Dec()
	resp, err := h.respond(req.Context(), device, conn, &body, timeout)
	if errors.Is(err, errShuttingDown) {
		return util.JSONResponse{Code: http.StatusServiceUnavailable, JSON: spec.Unknown("the server is shutting down")}
	}
	if err != nil {
		if req.Context().Err() != nil {
			// The client went away: nothing was sent, so nothing is remembered.
			return util.JSONResponse{Code: http.StatusRequestTimeout, JSON: spec.Unknown("request cancelled")}
		}
		util.GetLogger(req.Context()).WithError(err).Error("failed to build sliding sync response")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	conn.RecordResponse(requestSeq, resp)
	return util.JSONResponse{Code: http.StatusOK, JSON: resp}
}

// hasData reports whether a response has anything the client is waiting for.
func (r *Response) hasData() bool {
	return len(r.Rooms) > 0 || r.Extensions.hasData()
}

// respond builds the response, waiting up to timeout for something to send if there is nothing yet.
// The connection only changes once the response to return has been chosen.
func (h *Handler) respond(
	ctx context.Context, device *userapi.Device, conn *Connection, body *Request, timeout time.Duration,
) (*Response, error) {
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}

	for {
		// Take the position before looking, so that a change that lands in between is not missed.
		var listener *notifier.UserDeviceStreamListener
		var now types.StreamingToken
		if h.notifier != nil {
			now = h.notifier.CurrentPosition()
			if timeout > 0 {
				l := h.notifier.GetListener(types.SyncRequest{Device: device, Context: ctx})
				listener = &l
			}
		}
		closeListener := func() {
			if listener != nil {
				listener.Close()
			}
		}

		resp := &Response{TxnID: body.TxnID}
		commit, err := h.fill(ctx, device, conn, body, resp, now)
		if err != nil {
			closeListener()
			return nil, err
		}
		if timeout <= 0 || resp.hasData() {
			closeListener()
			commit()
			return resp, nil
		}

		waitingRequests.Inc()
		select {
		case <-listener.GetNotifyChannel(now):
			waitingRequests.Dec()
			closeListener()
		case <-expired:
			waitingRequests.Dec()
			closeListener()
			commit()
			return resp, nil
		case <-ctx.Done():
			waitingRequests.Dec()
			closeListener()
			return nil, ctx.Err()
		case <-h.shutdown.Done():
			waitingRequests.Dec()
			closeListener()
			return nil, errShuttingDown
		}
	}
}

// fillResponse evaluates the request's lists and subscriptions against the user's rooms.
func (h *Handler) fillResponse(
	ctx context.Context, device *userapi.Device, conn *Connection, body *Request, resp *Response, now types.StreamingToken,
) (commit func(), err error) {
	snapshot, err := h.db.NewDatabaseSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening snapshot: %w", err)
	}
	succeeded := false
	defer sqlutil.EndTransactionWithCheck(snapshot, &succeeded, &err)

	cands, err := h.loadCandidates(ctx, snapshot, device.UserID, needsFor(body), conn.sent)
	if err != nil {
		return nil, err
	}
	sortEntries(cands.entries)
	p := planRooms(cands.entries, body)

	if len(p.counts) > 0 {
		resp.Lists = make(map[string]ListResponse, len(p.counts))
		for name, count := range p.counts {
			resp.Lists[name] = ListResponse{Count: count}
		}
	}

	u, err := h.buildUpdate(ctx, snapshot, device, cands, p, conn.sent)
	if err != nil {
		return nil, err
	}
	if len(u.rooms) > 0 {
		resp.Rooms = u.rooms
	}

	extensionsCommit, err := h.buildExtensions(&extensionRun{
		ctx: ctx, snapshot: snapshot, device: device, conn: conn, cfg: body.ext, plan: p, update: u, now: now, resp: resp,
	})
	if err != nil {
		return nil, err
	}
	succeeded = true
	candidateIDs := make(map[string]struct{}, len(cands.entries))
	for _, e := range cands.entries {
		candidateIDs[e.roomID] = struct{}{}
	}
	return func() {
		for roomID, state := range u.sent {
			conn.sent[roomID] = state
		}
		// Rooms that are gone altogether, such as purged ones, are not worth remembering.
		for roomID := range conn.sent {
			if _, ok := candidateIDs[roomID]; !ok {
				delete(conn.sent, roomID)
			}
		}
		extensionsCommit()
	}, nil
}

// noteDeviceActivity refreshes the device's last-seen address and user agent, at most once per interval.
func (h *Handler) noteDeviceActivity(req *http.Request, device *userapi.Device, conn *Connection) {
	if time.Since(conn.lastSeen) < lastSeenInterval {
		return
	}
	conn.lastSeen = time.Now()

	remoteAddr := req.RemoteAddr
	if h.cfg != nil && h.cfg.RealIPHeader != "" {
		if header := req.Header.Get(h.cfg.RealIPHeader); header != "" {
			// The header can be a list, as X-Forwarded-For is, where the client is the first address.
			if first := strings.TrimSpace(strings.Split(header, ",")[0]); net.ParseIP(first) != nil {
				remoteAddr = first
			}
		}
	}
	update := &userapi.PerformLastSeenUpdateRequest{
		UserID:     device.UserID,
		DeviceID:   device.ID,
		RemoteAddr: remoteAddr,
		UserAgent:  req.UserAgent(),
	}
	go h.userAPI.PerformLastSeenUpdate(context.Background(), update, &userapi.PerformLastSeenUpdateResponse{}) // nolint:errcheck
}

// tooLarge describes the first part of the request that asks for unreasonable amounts of work, or returns "".
func tooLarge(body *Request) string {
	// The spaces are looked up for all the lists together, so their number is bounded for the whole request, and
	// the length of what they name is bounded, because every comparison costs as much as the string is long.
	spaces := map[string]struct{}{}
	for name, list := range body.Lists {
		if len(list.Ranges) > maxRangesPerList {
			return fmt.Sprintf("list %q has more than %d ranges", name, maxRangesPerList)
		}
		if len(list.RequiredState) > maxRequiredState {
			return fmt.Sprintf("list %q has more than %d required_state entries", name, maxRequiredState)
		}
		if f := list.Filters; f != nil && (len(f.Spaces) > maxFilterEntries || len(f.Tags) > maxFilterEntries || len(f.NotTags) > maxFilterEntries) {
			return fmt.Sprintf("list %q has more than %d spaces or tags in its filters", name, maxFilterEntries)
		}
		if f := list.Filters; f != nil {
			for _, entries := range [][]string{f.Spaces, f.Tags, f.NotTags} {
				for _, e := range entries {
					if len(e) > maxFilterEntryLength {
						return fmt.Sprintf("list %q has a space or tag of more than %d bytes in its filters", name, maxFilterEntryLength)
					}
				}
			}
			for _, s := range f.Spaces {
				spaces[s] = struct{}{}
			}
		}
	}
	if len(spaces) > maxFilterEntries {
		return fmt.Sprintf("the lists name more than %d different spaces in all", maxFilterEntries)
	}
	for roomID, sub := range body.RoomSubscriptions {
		if len(sub.RequiredState) > maxRequiredState {
			return fmt.Sprintf("subscription %q has more than %d required_state entries", roomID, maxRequiredState)
		}
	}
	return ""
}
