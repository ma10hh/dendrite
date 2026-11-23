// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	log "github.com/sirupsen/logrus"

	clienthttputil "github.com/element-hq/dendrite/clientapi/httputil"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/transactions"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// delayParam is the query parameter that turns a send into a delayed event, as MSC4140 names it while it is unstable.
const delayParam = "org.matrix.msc4140.delay"

// delayedEvents keeps the events that users asked to be sent later (MSC4140). They are stored by the user API, so that
// they survive a restart, and each waits for its own timer. A timer that runs out sends the event as the user, with the
// checks of a normal send. An event is removed before it is sent: if the server stops in between it is lost rather than
// sent twice.
type delayedEvents struct {
	cfg     *config.ClientAPI
	userAPI userapi.ClientUserAPI
	rsAPI   roomserverAPI.ClientRoomserverAPI

	mu     sync.Mutex
	timers map[string]*time.Timer
}

// NewDelayedEvents makes the scheduler of delayed events. Call Start to load the events that are waiting.
func NewDelayedEvents(cfg *config.ClientAPI, userAPI userapi.ClientUserAPI, rsAPI roomserverAPI.ClientRoomserverAPI) *delayedEvents {
	return &delayedEvents{cfg: cfg, userAPI: userAPI, rsAPI: rsAPI, timers: map[string]*time.Timer{}}
}

// Start sets the timers of the events that were waiting when the server stopped. Those that are overdue are sent now.
func (d *delayedEvents) Start(ctx context.Context) error {
	events, err := d.userAPI.AllDelayedEvents(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range events {
		d.arm(e)
	}
	return nil
}

// arm sets the timer of the event. The caller holds the lock.
func (d *delayedEvents) arm(e userapi.DelayedEvent) {
	if old, ok := d.timers[e.DelayID]; ok {
		old.Stop()
	}
	wait := time.Until(time.UnixMilli(e.RunningSinceMS + e.DelayMS))
	id := e.DelayID
	d.timers[id] = time.AfterFunc(max(wait, 0), func() {
		if res := d.send(context.Background(), id); res.Code != http.StatusOK {
			log.WithField("delay_id", id).Warnf("A delayed event could not be sent: %d %v", res.Code, res.JSON)
		}
	})
}

func (d *delayedEvents) disarm(delayID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[delayID]; ok {
		t.Stop()
		delete(d.timers, delayID)
	}
}

// send takes the event out and sends it as its user. It answers 404 if the event is not there any more.
func (d *delayedEvents) send(ctx context.Context, delayID string) util.JSONResponse {
	d.disarm(delayID)
	e, err := d.userAPI.DelayedEvent(ctx, delayID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if e == nil {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown delayed event")}
	}
	found, err := d.userAPI.DeleteDelayedEvent(ctx, delayID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !found {
		// Another request took it between the read and the delete.
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown delayed event")}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "/", bytes.NewReader(e.Content))
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	device := &userapi.Device{UserID: e.UserID, ID: e.DeviceID}
	res := SendEvent(req, device, e.RoomID, e.EventType, nil, e.StateKey, d.cfg, d.rsAPI, nil)
	if res.Code == http.StatusOK && e.StateKey != nil {
		d.cancelState(ctx, e.RoomID, e.EventType, *e.StateKey)
	}
	return res
}

// cancelState drops the delayed state events for the same type and state key of a room: a state event has just been
// sent, and one that was waiting from before would overwrite it later.
func (d *delayedEvents) cancelState(ctx context.Context, roomID, eventType, stateKey string) {
	ids, err := d.userAPI.DeleteDelayedStateEvents(ctx, roomID, eventType, stateKey)
	if err != nil {
		log.WithError(err).Warn("Cancelling delayed state events failed")
		return
	}
	for _, id := range ids {
		d.disarm(id)
	}
}

func newDelayID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func maxDelayExceeded(max time.Duration) util.JSONResponse {
	return util.JSONResponse{Code: http.StatusBadRequest, JSON: struct {
		ErrCode  string `json:"errcode"`
		Err      string `json:"error"`
		Unstable string `json:"org.matrix.msc4140.errcode"`
		MaxDelay int64  `json:"max_delay"`
	}{"M_UNKNOWN", "The requested delay exceeds the allowed maximum.", "M_MAX_DELAY_EXCEEDED", max.Milliseconds()}}
}

// sendOrDelay sends the event, or, if the request asks for a delay, keeps it to be sent later. A state event that is
// sent now also cancels the delayed state events of the same type and state key. d is nil if delayed events are off.
func (d *delayedEvents) sendOrDelay(
	req *http.Request, device *userapi.Device, roomID, eventType string, txnID, stateKey *string,
	cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI, txnCache *transactions.Cache,
) util.JSONResponse {
	if raw := req.URL.Query().Get(delayParam); raw != "" {
		if d == nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.Unrecognized("Delayed events are not enabled on this server")}
		}
		return d.schedule(req, device, roomID, eventType, stateKey, raw)
	}
	res := SendEvent(req, device, roomID, eventType, txnID, stateKey, cfg, rsAPI, txnCache)
	if d != nil && stateKey != nil && res.Code == http.StatusOK {
		d.cancelState(req.Context(), roomID, eventType, *stateKey)
	}
	return res
}

func (d *delayedEvents) schedule(req *http.Request, device *userapi.Device, roomID, eventType string, stateKey *string, rawDelay string) util.JSONResponse {
	ctx := req.Context()
	delay, err := strconv.ParseInt(rawDelay, 10, 64)
	if err != nil || delay <= 0 {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("delay must be a positive number of milliseconds")}
	}
	if time.Duration(delay)*time.Millisecond > d.cfg.DelayedEvents.MaxDelay || delay > int64(d.cfg.DelayedEvents.MaxDelay/time.Millisecond) {
		return maxDelayExceeded(d.cfg.DelayedEvents.MaxDelay)
	}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Bad userID")}
	}
	if resErr := checkMemberInRoom(ctx, d.rsAPI, *userID, roomID); resErr != nil {
		return *resErr
	}
	var content map[string]any
	if resErr := clienthttputil.UnmarshalJSONRequest(req, &content); resErr != nil {
		return *resErr
	}
	id, err := newDelayID()
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if device.ShadowBanned {
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"delay_id": id}}
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Cannot read the content.")}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	count, err := d.userAPI.CountDelayedEvents(ctx, device.UserID)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("CountDelayedEvents failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if count >= d.cfg.DelayedEvents.MaxPerUser {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MatrixError{ErrCode: spec.ErrorLimitExceeded, Err: "Too many delayed events are waiting"}}
	}
	e := userapi.DelayedEvent{
		DelayID: id, UserID: device.UserID, DeviceID: device.ID, RoomID: roomID, EventType: eventType, StateKey: stateKey,
		Content: raw, DelayMS: delay, RunningSinceMS: time.Now().UnixMilli(),
	}
	if err = d.userAPI.InsertDelayedEvent(ctx, e); err != nil {
		util.GetLogger(ctx).WithError(err).Error("InsertDelayedEvent failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	d.arm(e)
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"delay_id": id}}
}

// own finds the delayed event of the user. Other users' events are not found, so that their IDs are not confirmed.
func (d *delayedEvents) own(ctx context.Context, device *userapi.Device, delayID string) (*userapi.DelayedEvent, *util.JSONResponse) {
	e, err := d.userAPI.DelayedEvent(ctx, delayID)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("DelayedEvent failed")
		return nil, &util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if e == nil || e.UserID != device.UserID {
		return nil, &util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown delayed event")}
	}
	return e, nil
}

// update implements POST /delayed_events/{delay_id} with the action "send", "cancel" or "restart".
func (d *delayedEvents) update(req *http.Request, device *userapi.Device, delayID, action string) util.JSONResponse {
	ctx := req.Context()
	if action == "" {
		var body struct {
			Action string `json:"action"`
		}
		if resErr := clienthttputil.UnmarshalJSONRequest(req, &body); resErr != nil {
			return *resErr
		}
		if body.Action == "" {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("action is missing")}
		}
		action = body.Action
	}
	if action != "send" && action != "cancel" && action != "restart" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("action must be send, cancel or restart")}
	}
	e, errRes := d.own(ctx, device, delayID)
	if errRes != nil {
		return *errRes
	}
	switch action {
	case "send":
		if res := d.send(ctx, e.DelayID); res.Code != http.StatusOK {
			return res
		}
	case "cancel":
		d.disarm(e.DelayID)
		if _, err := d.userAPI.DeleteDelayedEvent(ctx, e.DelayID); err != nil {
			util.GetLogger(ctx).WithError(err).Error("DeleteDelayedEvent failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	case "restart":
		d.mu.Lock()
		defer d.mu.Unlock()
		now := time.Now().UnixMilli()
		found, err := d.userAPI.RestartDelayedEvent(ctx, e.DelayID, now)
		if err != nil {
			util.GetLogger(ctx).WithError(err).Error("RestartDelayedEvent failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		if !found {
			return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown delayed event")}
		}
		e.RunningSinceMS = now
		d.arm(*e)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

type delayedEventEntry struct {
	DelayID      string          `json:"delay_id"`
	RoomID       string          `json:"room_id"`
	Type         string          `json:"type"`
	StateKey     *string         `json:"state_key,omitempty"`
	Delay        int64           `json:"delay"`
	RunningSince int64           `json:"running_since"`
	Content      json.RawMessage `json:"content"`
}

// list implements GET /delayed_events: the delayed events of the user, the next to be sent first.
func (d *delayedEvents) list(req *http.Request, device *userapi.Device) util.JSONResponse {
	events, err := d.userAPI.DelayedEventsForUser(req.Context(), device.UserID)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("DelayedEventsForUser failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	out := make([]delayedEventEntry, 0, len(events))
	for _, e := range events {
		out = append(out, delayedEventEntry{
			DelayID: e.DelayID, RoomID: e.RoomID, Type: e.EventType, StateKey: e.StateKey,
			Delay: e.DelayMS, RunningSince: e.RunningSinceMS, Content: e.Content,
		})
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"delayed_events": out}}
}

// setupDelayedEventsRoutes registers the endpoints for delayed events under the prefix, if they are enabled.
func setupDelayedEventsRoutes(
	router *mux.Router, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI, rateLimits *httputil.RateLimits,
	d *delayedEvents, prefix string,
) {
	if d == nil {
		return
	}
	handle := func(path, name string, f func(req *http.Request, device *userapi.Device, vars map[string]string) util.JSONResponse, method string) {
		router.Handle(prefix+path, httputil.MakeAuthAPI(name, userAPI, func(req *http.Request, device *userapi.Device) util.JSONResponse {
			if r := rateLimits.Limit(req, device); r != nil {
				return *r
			}
			vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
			if err != nil {
				return util.ErrorResponse(err)
			}
			return f(req, device, vars)
		})).Methods(method, http.MethodOptions)
	}
	handle("/delayed_events", "delayed_events_list", func(req *http.Request, device *userapi.Device, _ map[string]string) util.JSONResponse {
		return d.list(req, device)
	}, http.MethodGet)
	handle("/delayed_events/{delayID}", "delayed_events_update", func(req *http.Request, device *userapi.Device, vars map[string]string) util.JSONResponse {
		return d.update(req, device, vars["delayID"], "")
	}, http.MethodPost)
	// The form that clients used before the action moved into the body.
	handle("/delayed_events/{delayID}/{action}", "delayed_events_action", func(req *http.Request, device *userapi.Device, vars map[string]string) util.JSONResponse {
		return d.update(req, device, vars["delayID"], vars["action"])
	}, http.MethodPost)
}

type rtcTransport struct {
	Type              string `json:"type"`
	LiveKitServiceURL string `json:"livekit_service_url,omitempty"`
}

func rtcTransports(cfg *config.ClientAPI) []rtcTransport {
	out := make([]rtcTransport, 0, len(cfg.MatrixRTC.Transports))
	for _, t := range cfg.MatrixRTC.Transports {
		out = append(out, rtcTransport{Type: t.Type, LiveKitServiceURL: t.LiveKitServiceURL})
	}
	return out
}

// setupRTCRoutes registers GET /rtc/transports (MSC4143) under the prefix, if media servers for calls are configured.
func setupRTCRoutes(router *mux.Router, cfg *config.ClientAPI, userAPI userapi.ClientUserAPI, rateLimits *httputil.RateLimits, prefix string) {
	if len(cfg.MatrixRTC.Transports) == 0 {
		return
	}
	router.Handle(prefix+"/rtc/transports", httputil.MakeAuthAPI("rtc_transports", userAPI, func(req *http.Request, device *userapi.Device) util.JSONResponse {
		if r := rateLimits.Limit(req, device); r != nil {
			return *r
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"rtc_transports": rtcTransports(cfg)}}
	})).Methods(http.MethodGet, http.MethodOptions)
}
