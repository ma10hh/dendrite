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
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	log "github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/internal/httputil"
	roomserverAPI "github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// purge_history and redact: administrator jobs that run in the background. Their state is in memory: it is gone after a
// restart, and a job that was running then is not continued (it can be asked for again, since both are repeatable).

// adminJob is the state of one background job.
type adminJob struct {
	Status string
	Error  string
	// Failed lists the events that a redaction could not be sent for, with the reason.
	Failed  map[string]string
	Started time.Time
}

type adminJobs struct {
	mu   sync.Mutex
	jobs map[string]*adminJob
}

const adminJobTTL = 24 * time.Hour

func newAdminJobs() *adminJobs { return &adminJobs{jobs: map[string]*adminJob{}} }

// start makes a job with a new ID and drops the ones that are over a day old.
func (a *adminJobs) start(status string) (string, *adminJob, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	job := &adminJob{Status: status, Failed: map[string]string{}, Started: time.Now()}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, j := range a.jobs {
		if time.Since(j.Started) > adminJobTTL && j.Status != "active" {
			delete(a.jobs, k)
		}
	}
	a.jobs[id] = job
	return id, job, nil
}

func (a *adminJobs) update(job *adminJob, f func(*adminJob)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(job)
}

func (a *adminJobs) get(id string) (adminJob, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, ok := a.jobs[id]
	if !ok {
		return adminJob{}, false
	}
	cp := *j
	cp.Failed = make(map[string]string, len(j.Failed))
	for k, v := range j.Failed {
		cp.Failed[k] = v
	}
	return cp, true
}

// SynapseAdminPurgeHistory implements POST /_synapse/admin/v1/purge_history/{roomId}[/{eventId}]: the non-state events
// of the room before a time or an event are deleted, in the background. The events of users of this server are kept
// unless delete_local_events is set, as in Synapse.
func SynapseAdminPurgeHistory(req *http.Request, rsAPI roomserverAPI.ClientRoomserverAPI, jobs *adminJobs) util.JSONResponse {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		return util.ErrorResponse(err)
	}
	roomID := vars["roomId"]
	if _, err = spec.NewRoomID(roomID); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid room ID")}
	}
	var body struct {
		DeleteLocal *bool  `json:"delete_local_events"`
		UpToEventID string `json:"purge_up_to_event_id"`
		UpToTS      *int64 `json:"purge_up_to_ts"`
	}
	if req.Body != nil {
		if err = json.NewDecoder(req.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
		}
	}
	if id := vars["eventId"]; id != "" {
		body.UpToEventID = id
	}
	if (body.UpToEventID == "") == (body.UpToTS == nil) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Exactly one of purge_up_to_event_id and purge_up_to_ts is needed")}
	}
	if body.UpToTS != nil && *body.UpToTS < 0 {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("purge_up_to_ts must be a time in milliseconds")}
	}
	if _, err = rsAPI.QueryRoomVersionForRoom(req.Context(), roomID); err != nil {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room not found")}
	}
	sync := rsAPI.SyncAdmin()
	if sync == nil {
		return util.JSONResponse{Code: http.StatusServiceUnavailable, JSON: spec.Unknown("The sync API is not running")}
	}
	before := int64(0)
	if body.UpToTS != nil {
		before = *body.UpToTS
	} else if before, err = sync.EventTimestamp(req.Context(), roomID, body.UpToEventID); errors.Is(err, roomserverAPI.ErrSyncAdminNotFound) {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("The event is not in the timeline of the room")}
	} else if err != nil {
		return adminServerError(req, "EventTimestamp", err)
	}
	deleteLocal := body.DeleteLocal != nil && *body.DeleteLocal

	id, job, err := jobs.start("active")
	if err != nil {
		return adminServerError(req, "starting the purge", err)
	}
	go func() {
		n, err := sync.PurgeHistory(context.Background(), roomID, before, deleteLocal)
		jobs.update(job, func(j *adminJob) {
			if err != nil {
				j.Status, j.Error = "failed", err.Error()
				return
			}
			j.Status = "complete"
		})
		log.WithFields(log.Fields{"room_id": roomID, "deleted": n, "error": err, "purge_id": id}).Info("History purge finished")
	}()
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"purge_id": id}}
}

// SynapseAdminPurgeHistoryStatus implements GET /_synapse/admin/v1/purge_history_status/{purgeId}.
func SynapseAdminPurgeHistoryStatus(req *http.Request, jobs *adminJobs) util.JSONResponse {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		return util.ErrorResponse(err)
	}
	job, ok := jobs.get(vars["purgeId"])
	if !ok {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown purge_id")}
	}
	body := map[string]string{"status": job.Status}
	if job.Error != "" {
		body["error"] = job.Error
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}

const redactPage = 200

// SynapseAdminRedactUser implements POST /_synapse/admin/v1/user/{userId}/redact: the non-state events of a local user
// are redacted, in the background, in all rooms or the ones listed. A redaction is sent as the user; if that is not
// possible (the user has left) as the administrator, who then needs the power to redact. What cannot be redacted is
// listed in the status. Only users of this server: the timeline lists what they sent.
func SynapseAdminRedactUser(
	req *http.Request, admin *userapi.Device, cfg *config.ClientAPI,
	rsAPI roomserverAPI.ClientRoomserverAPI, jobs *adminJobs,
) util.JSONResponse {
	userID, _, _, errResp := localAdminUser(req, cfg)
	if errResp != nil {
		return *errResp
	}
	var body struct {
		Rooms  []string `json:"rooms"`
		Reason string   `json:"reason"`
		Limit  *int     `json:"limit"`
	}
	if req.Body != nil {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body could not be decoded: " + err.Error())}
		}
	}
	if body.Limit != nil && *body.Limit <= 0 {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("limit must be above 0")}
	}
	sync := rsAPI.SyncAdmin()
	if sync == nil {
		return util.JSONResponse{Code: http.StatusServiceUnavailable, JSON: spec.Unknown("The sync API is not running")}
	}
	only := map[string]bool{}
	for _, r := range body.Rooms {
		only[r] = true
	}
	id, job, err := jobs.start("scheduled")
	if err != nil {
		return adminServerError(req, "starting the redaction", err)
	}
	limit := -1
	if body.Limit != nil {
		limit = *body.Limit
	}
	go redactUser(context.Background(), cfg, rsAPI, sync, jobs, job, userID, admin.UserID, body.Reason, only, limit)
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"redact_id": id}}
}

func redactUser(
	ctx context.Context, cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI, sync roomserverAPI.SyncAdmin,
	jobs *adminJobs, job *adminJob, userID, adminID, reason string, only map[string]bool, limit int,
) {
	jobs.update(job, func(j *adminJob) { j.Status = "active" })
	fail := func(err error) {
		jobs.update(job, func(j *adminJob) { j.Status, j.Error = "failed", err.Error() })
	}
	content, _ := json.Marshal(map[string]string{"reason": reason})
	send := func(device *userapi.Device, ev roomserverAPI.SyncAdminEvent) util.JSONResponse {
		r, err := http.NewRequestWithContext(ctx, http.MethodPut, "/", bytes.NewReader(content))
		if err != nil {
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		return SendRedaction(r, device, ev.RoomID, ev.EventID, cfg, rsAPI, nil, nil)
	}
	var after int64
	for done := 0; limit < 0 || done < limit; {
		events, err := sync.EventsBySender(ctx, userID, after, redactPage)
		if err != nil {
			fail(err)
			return
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			after = ev.Position
			if len(only) > 0 && !only[ev.RoomID] {
				continue
			}
			if limit >= 0 && done >= limit {
				break
			}
			done++
			res := send(&userapi.Device{UserID: userID, ID: "admin-redact"}, ev)
			if res.Code == http.StatusForbidden && adminID != "" {
				res = send(&userapi.Device{UserID: adminID, ID: "admin-redact"}, ev)
			}
			if res.Code != http.StatusOK {
				b, _ := json.Marshal(res.JSON)
				jobs.update(job, func(j *adminJob) { j.Failed[ev.EventID] = string(b) })
			}
		}
	}
	jobs.update(job, func(j *adminJob) { j.Status = "completed" })
}

// SynapseAdminRedactStatus implements GET /_synapse/admin/v1/user/redact_status/{redactId}.
func SynapseAdminRedactStatus(req *http.Request, jobs *adminJobs) util.JSONResponse {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		return util.ErrorResponse(err)
	}
	job, ok := jobs.get(vars["redactId"])
	if !ok {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Unknown redact_id")}
	}
	body := map[string]any{"status": job.Status, "failed_redactions": job.Failed}
	if job.Error != "" {
		body["error"] = job.Error
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}
