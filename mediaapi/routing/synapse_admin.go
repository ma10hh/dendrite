// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	log "github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/mediaapi/fileutils"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// The media part of the Synapse admin API:
// https://element-hq.github.io/synapse/latest/admin_api/media_admin_api.html

// setupSynapseAdmin registers the admin routes for media.
func setupSynapseAdmin(
	routers httputil.Routers, cfg *config.Dendrite, db storage.Database, userAPI userapi.MediaUserAPI,
) {
	admin := routers.SynapseAdmin
	handle := func(path, name string, f func(*http.Request) util.JSONResponse, methods ...string) {
		admin.Handle(path, httputil.MakeAdminAPI(name, userAPI, func(req *http.Request, _ *userapi.Device) util.JSONResponse {
			return f(req)
		})).Methods(append(methods, http.MethodOptions)...)
	}
	media := &mediaAdmin{cfg: &cfg.MediaAPI, db: db}
	handle("/admin/v1/media/quarantine/{server}/{mediaId}", "synapse_admin_quarantine", func(r *http.Request) util.JSONResponse { return media.quarantine(r, true) }, http.MethodPost)
	handle("/admin/v1/media/unquarantine/{server}/{mediaId}", "synapse_admin_unquarantine", func(r *http.Request) util.JSONResponse { return media.quarantine(r, false) }, http.MethodPost)
	handle("/admin/v1/user/{userId}/media/quarantine", "synapse_admin_quarantine_user", media.quarantineUser, http.MethodPost)
	handle("/admin/v1/media/{server}/{mediaId}", "synapse_admin_delete_media", media.deleteOne, http.MethodDelete)
	handle("/admin/v1/media/{server}/delete", "synapse_admin_delete_old_media", media.deleteOld, http.MethodPost)
	handle("/admin/v1/users/{userId}/media", "synapse_admin_user_media", func(r *http.Request) util.JSONResponse {
		if r.Method == http.MethodDelete {
			return media.deleteUser(r)
		}
		return media.listUser(r)
	}, http.MethodGet, http.MethodDelete)
}

type mediaAdmin struct {
	cfg *config.MediaAPI
	db  storage.Database
}

func (a *mediaAdmin) vars(req *http.Request) (map[string]string, *util.JSONResponse) {
	vars, err := httputil.URLDecodeMapValues(mux.Vars(req))
	if err != nil {
		r := util.ErrorResponse(err)
		return nil, &r
	}
	return vars, nil
}

// local checks that the media is of this server: the others are copies of media of other servers, which are theirs.
func (a *mediaAdmin) local(server string) *util.JSONResponse {
	if spec.ServerName(server) != a.cfg.Matrix.ServerName {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Can only use local media")}
	}
	return nil
}

func (a *mediaAdmin) quarantine(req *http.Request, quarantined bool) util.JSONResponse {
	vars, errResp := a.vars(req)
	if errResp != nil {
		return *errResp
	}
	found, err := a.db.SetMediaQuarantined(req.Context(), types.MediaID(vars["mediaId"]), spec.ServerName(vars["server"]), quarantined)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("SetMediaQuarantined failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !found {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Media not found")}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

func (a *mediaAdmin) quarantineUser(req *http.Request) util.JSONResponse {
	vars, errResp := a.vars(req)
	if errResp != nil {
		return *errResp
	}
	n, err := a.db.SetUserMediaQuarantined(req.Context(), vars["userId"], true)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("SetUserMediaQuarantined failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]int64{"num_quarantined": n}}
}

// remove deletes the media, and its file and thumbnails if no other media uses them.
func (a *mediaAdmin) remove(req *http.Request, id types.MediaID) (bool, error) {
	meta, inUse, err := a.db.DeleteMedia(req.Context(), id, a.cfg.Matrix.ServerName)
	if err != nil || meta == nil {
		return false, err
	}
	if !inUse {
		if path, perr := fileutils.GetPathFromBase64Hash(meta.Base64Hash, a.cfg.AbsBasePath); perr == nil {
			fileutils.RemoveDir(types.Path(filepath.Dir(path)), log.WithField("media_id", id))
		}
	}
	return true, nil
}

func (a *mediaAdmin) removeAll(req *http.Request, ids []types.MediaID) util.JSONResponse {
	deleted := make([]string, 0, len(ids))
	for _, id := range ids {
		ok, err := a.remove(req, id)
		if err != nil {
			util.GetLogger(req.Context()).WithError(err).WithField("media_id", id).Error("deleting media failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		if ok {
			deleted = append(deleted, string(id))
		}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]any{"deleted_media": deleted, "total": len(deleted)}}
}

func (a *mediaAdmin) deleteOne(req *http.Request) util.JSONResponse {
	vars, errResp := a.vars(req)
	if errResp != nil {
		return *errResp
	}
	if errResp = a.local(vars["server"]); errResp != nil {
		return *errResp
	}
	return a.removeAll(req, []types.MediaID{types.MediaID(vars["mediaId"])})
}

func (a *mediaAdmin) deleteOld(req *http.Request) util.JSONResponse {
	vars, errResp := a.vars(req)
	if errResp != nil {
		return *errResp
	}
	if errResp = a.local(vars["server"]); errResp != nil {
		return *errResp
	}
	q := req.URL.Query()
	before, err := strconv.ParseInt(q.Get("before_ts"), 10, 64)
	if err != nil || before < 0 {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("before_ts must be a time in milliseconds")}
	}
	var larger int64
	if v := q.Get("size_gt"); v != "" {
		if larger, err = strconv.ParseInt(v, 10, 64); err != nil || larger < 0 {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("size_gt must be a number of bytes")}
		}
	}
	old, err := a.db.MediaToDelete(req.Context(), a.cfg.Matrix.ServerName, spec.Timestamp(before), larger)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("MediaToDelete failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	ids := make([]types.MediaID, len(old))
	for i := range old {
		ids[i] = old[i].MediaID
	}
	return a.removeAll(req, ids)
}

func (a *mediaAdmin) deleteUser(req *http.Request) util.JSONResponse {
	vars, errResp := a.vars(req)
	if errResp != nil {
		return *errResp
	}
	media, _, err := a.db.ListUserMedia(req.Context(), vars["userId"], 0, 1<<30, "", false)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("ListUserMedia failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	ids := make([]types.MediaID, 0, len(media))
	for _, m := range media {
		if m.Origin == a.cfg.Matrix.ServerName {
			ids = append(ids, m.MediaID)
		}
	}
	return a.removeAll(req, ids)
}

func (a *mediaAdmin) listUser(req *http.Request) util.JSONResponse {
	vars, errResp := a.vars(req)
	if errResp != nil {
		return *errResp
	}
	q := req.URL.Query()
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
	backwards := false
	switch q.Get("dir") {
	case "", "f":
	case "b":
		backwards = true
	default:
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("dir must be f or b")}
	}
	media, total, err := a.db.ListUserMedia(req.Context(), vars["userId"], from, limit, q.Get("order_by"), backwards)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("ListUserMedia failed")
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(err.Error())}
	}
	type entry struct {
		MediaID            string  `json:"media_id"`
		MediaType          string  `json:"media_type"`
		MediaLength        int64   `json:"media_length"`
		UploadName         string  `json:"upload_name"`
		CreatedTS          int64   `json:"created_ts"`
		LastAccessTS       *int64  `json:"last_access_ts"`
		QuarantinedBy      *string `json:"quarantined_by"`
		SafeFromQuarantine bool    `json:"safe_from_quarantine"`
	}
	out := make([]entry, 0, len(media))
	for _, m := range media {
		e := entry{MediaID: string(m.MediaID), MediaType: string(m.ContentType), MediaLength: int64(m.FileSizeBytes), UploadName: string(m.UploadName), CreatedTS: int64(m.CreationTimestamp)}
		if m.Quarantined {
			admin := "admin"
			e.QuarantinedBy = &admin
		}
		out = append(out, e)
	}
	body := map[string]any{"media": out, "total": total}
	if next := from + len(media); next < total {
		body["next_token"] = next
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: body}
}
