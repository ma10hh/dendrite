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
	mathrand "math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"

	clienthttputil "github.com/element-hq/dendrite/clientapi/httputil"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/transactions"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

var (
	delayedEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dendrite",
		Subsystem: "clientapi",
		Name:      "delayed_events_total",
		Help:      "Delayed events by what happened: scheduled, sent, cancelled, retried (a send failed because of the server and is tried again) or failed (dropped)",
	}, []string{"result"})
	delayedWaiting = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "dendrite",
		Subsystem: "clientapi",
		Name:      "delayed_events_waiting",
		Help:      "Delayed events with a timer, waiting to be sent",
	})
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

	mu       sync.Mutex
	timers   map[string]*time.Timer
	inflight map[string]struct{} // events that are being sent
	attempts map[string]int      // failed sends of an event, by the server
}

// NewDelayedEvents makes the scheduler of delayed events. Call Start to load the events that are waiting.
func NewDelayedEvents(cfg *config.ClientAPI, userAPI userapi.ClientUserAPI, rsAPI roomserverAPI.ClientRoomserverAPI) *delayedEvents {
	return &delayedEvents{cfg: cfg, userAPI: userAPI, rsAPI: rsAPI, timers: map[string]*time.Timer{}, inflight: map[string]struct{}{}, attempts: map[string]int{}}
}

// Start sets the timers of the events that were waiting when the server stopped. Those that are overdue are sent
// within a few seconds, spread out so that a long outage does not make them all go at once.
func (d *delayedEvents) Start(ctx context.Context) error {
	events, err := d.userAPI.AllDelayedEvents(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range events {
		wait := time.Until(time.UnixMilli(e.RunningSinceMS + e.DelayMS))
		if wait <= 0 {
			wait = time.Duration(mathrand.Int63n(int64(overdueSpread)))
		}
		d.armAfter(e.DelayID, wait)
	}
	return nil
}

// overdueSpread is the window that events which were due while the server was down are spread over.
const overdueSpread = 3 * time.Second

// Retries of an event whose send failed because of the server: the delay before the n-th is retryBase * 2^(n-1).
const (
	maxSendAttempts = 5
	retryBase       = time.Second
)

// arm sets the timer of the event from its own times. The caller holds the lock.
func (d *delayedEvents) arm(e userapi.DelayedEvent) {
	d.armAfter(e.DelayID, time.Until(time.UnixMilli(e.RunningSinceMS+e.DelayMS)))
}

// armAfter sets the timer of the event. The caller holds the lock.
func (d *delayedEvents) armAfter(id string, wait time.Duration) {
	if old, ok := d.timers[id]; ok {
		old.Stop()
	}
	d.timers[id] = time.AfterFunc(max(wait, 0), func() {
		if res := d.send(context.Background(), id); res.Code != http.StatusOK {
			log.WithField("delay_id", id).Warnf("A delayed event could not be sent: %d %v", res.Code, res.JSON)
		}
	})
	delayedWaiting.Set(float64(len(d.timers)))
}

func (d *delayedEvents) disarm(delayID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disarmLocked(delayID)
}

func (d *delayedEvents) disarmLocked(delayID string) {
	if t, ok := d.timers[delayID]; ok {
		t.Stop()
		delete(d.timers, delayID)
		delayedWaiting.Set(float64(len(d.timers)))
	}
}

// send sends the event as its user and removes it. It answers 404 if the event is not there any more, or is being sent
// by another call. **A failure of the server keeps the event** and tries again a few times with growing waits, since
// losing the "leave" of a call leaves a stale member behind; a refusal (the user may not send it any more) drops it.
// The price is that a crash between the send and the removal sends the event again on the next start: that is harmless
// for state events (the same content is not sent twice) but can repeat a message.
func (d *delayedEvents) send(ctx context.Context, delayID string) util.JSONResponse {
	d.mu.Lock()
	d.disarmLocked(delayID)
	if _, busy := d.inflight[delayID]; busy {
		d.mu.Unlock()
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown delayed event")}
	}
	d.inflight[delayID] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.inflight, delayID)
		d.mu.Unlock()
	}()

	e, err := d.userAPI.DelayedEvent(ctx, delayID)
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if e == nil {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown delayed event")}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "/", bytes.NewReader(e.Content))
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	device := &userapi.Device{UserID: e.UserID, ID: e.DeviceID}
	res := SendEvent(req, device, e.RoomID, e.EventType, nil, e.StateKey, d.cfg, d.rsAPI, nil)

	if res.Code >= http.StatusInternalServerError {
		d.mu.Lock()
		d.attempts[delayID]++
		n := d.attempts[delayID]
		if n < maxSendAttempts {
			d.armAfter(delayID, retryBase<<(n-1))
			d.mu.Unlock()
			delayedEventsTotal.WithLabelValues("retried").Inc()
			return res
		}
		delete(d.attempts, delayID)
		d.mu.Unlock()
	}
	d.mu.Lock()
	delete(d.attempts, delayID)
	d.mu.Unlock()
	if _, err = d.userAPI.DeleteDelayedEvent(ctx, delayID); err != nil {
		log.WithError(err).WithField("delay_id", delayID).Error("Removing a delayed event that was dealt with failed")
	}
	if res.Code != http.StatusOK {
		delayedEventsTotal.WithLabelValues("failed").Inc()
		return res
	}
	delayedEventsTotal.WithLabelValues("sent").Inc()
	if e.StateKey != nil {
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
	delayedEventsTotal.WithLabelValues("cancelled").Add(float64(len(ids)))
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
	delayedEventsTotal.WithLabelValues("scheduled").Inc()
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
		delayedEventsTotal.WithLabelValues("cancelled").Inc()
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
			if r := rateLimits.LimitBucket(req, device, "calls"); r != nil {
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
		if r := rateLimits.LimitBucket(req, device, "calls"); r != nil {
			return *r
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"rtc_transports": rtcTransports(cfg)}}
	})).Methods(http.MethodGet, http.MethodOptions)
}

type adminDelayedEvent struct {
	delayedEventEntry
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id"`
}

// adminList implements GET /_synapse/admin/v1/delayed_events[?user_id=]: the delayed events that are waiting, of all
// users or of one.
func (d *delayedEvents) adminList(req *http.Request) util.JSONResponse {
	var events []userapi.DelayedEvent
	var err error
	if user := req.URL.Query().Get("user_id"); user != "" {
		events, err = d.userAPI.DelayedEventsForUser(req.Context(), user)
	} else {
		events, err = d.userAPI.AllDelayedEvents(req.Context())
	}
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("listing delayed events failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	out := make([]adminDelayedEvent, 0, len(events))
	for _, e := range events {
		out = append(out, adminDelayedEvent{
			delayedEventEntry: delayedEventEntry{
				DelayID: e.DelayID, RoomID: e.RoomID, Type: e.EventType, StateKey: e.StateKey,
				Delay: e.DelayMS, RunningSince: e.RunningSinceMS, Content: e.Content,
			},
			UserID: e.UserID, DeviceID: e.DeviceID,
		})
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"delayed_events": out, "total": len(out)}}
}

// adminCancel implements DELETE /_synapse/admin/v1/delayed_events/{delay_id}: the event is dropped, not sent.
func (d *delayedEvents) adminCancel(req *http.Request, delayID string) util.JSONResponse {
	d.disarm(delayID)
	found, err := d.userAPI.DeleteDelayedEvent(req.Context(), delayID)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("DeleteDelayedEvent failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !found {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown delayed event")}
	}
	delayedEventsTotal.WithLabelValues("cancelled").Inc()
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

// setupDelayedEventsAdminRoutes registers the administrator's view of the delayed events, if they are enabled.
func setupDelayedEventsAdminRoutes(router *mux.Router, userAPI userapi.ClientUserAPI, d *delayedEvents) {
	if d == nil {
		return
	}
	router.Handle("/admin/v1/delayed_events", httputil.MakeAdminAPI("synapse_admin_delayed_events", userAPI, func(req *http.Request, _ *userapi.Device) util.JSONResponse {
		return d.adminList(req)
	})).Methods(http.MethodGet, http.MethodOptions)
	router.Handle("/admin/v1/delayed_events/{delayID}", httputil.MakeAdminAPI("synapse_admin_delayed_event", userAPI, func(req *http.Request, _ *userapi.Device) util.JSONResponse {
		vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
		if err != nil {
			return util.ErrorResponse(err)
		}
		return d.adminCancel(req, vars["delayID"])
	})).Methods(http.MethodDelete, http.MethodOptions)
}
