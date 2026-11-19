// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/mediaapi/urlpreview"
	"github.com/element-hq/dendrite/setup/config"
	userapi "github.com/element-hq/dendrite/userapi/api"
)

// newURLPreviewer returns the previewer of the configuration, or nil if URL previews are off or misconfigured.
func newURLPreviewer(cfg *config.MediaAPI) (*urlpreview.Previewer, error) {
	if !cfg.URLPreviews.Enabled {
		return nil, nil
	}
	deny, err := urlpreview.ParsePrefixes(cfg.URLPreviews.IPRangeDenylist)
	if err != nil {
		return nil, fmt.Errorf("media_api.url_previews.ip_range_denylist: %w", err)
	}
	allow, err := urlpreview.ParsePrefixes(cfg.URLPreviews.IPRangeAllowlist)
	if err != nil {
		return nil, fmt.Errorf("media_api.url_previews.ip_range_allowlist: %w", err)
	}
	maxImage := int64(cfg.MaxFileSizeBytes)
	if maxImage <= 0 {
		maxImage = int64(config.DefaultMaxFileSizeBytes)
	}
	return urlpreview.New(urlpreview.Config{
		MaxPageBytes:  int64(cfg.URLPreviews.MaxPageSizeBytes),
		MaxImageBytes: maxImage,
		Timeout:       cfg.URLPreviews.Timeout,
		CacheTTL:      cfg.URLPreviews.CacheLifetime,
		MaxConcurrent: cfg.URLPreviews.MaxConcurrent,
		MaxCached:     1000,
		UserAgent:     "Dendrite (URL preview bot; +https://github.com/element-hq/dendrite)",
		Guard:         urlpreview.Guard{Deny: deny, Allow: allow},
	}), nil
}

// PreviewURL implements GET /preview_url: the Open Graph data of a page, with its image kept as media of the user.
func PreviewURL(
	req *http.Request, dev *userapi.Device, cfg *config.MediaAPI, previewer *urlpreview.Previewer,
	db storage.Database, activeThumbnailGeneration *types.ActiveThumbnailGeneration,
) util.JSONResponse {
	rawURL := req.URL.Query().Get("url")
	if rawURL == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("url")}
	}
	if _, err := urlpreview.ValidateURL(rawURL); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(err.Error())}
	}

	store := func(ctx context.Context, img *urlpreview.Image) (string, error) {
		r := &uploadRequest{
			MediaMetadata: &types.MediaMetadata{
				Origin:        cfg.Matrix.ServerName,
				FileSizeBytes: types.FileSizeBytes(len(img.Data)),
				ContentType:   types.ContentType(img.ContentType),
				UserID:        types.MatrixUserID(dev.UserID),
			},
			Logger: util.GetLogger(ctx).WithField("preview", true),
		}
		if resErr := r.doUpload(ctx, bytes.NewReader(img.Data), cfg, db, activeThumbnailGeneration); resErr != nil {
			return "", fmt.Errorf("storing the image failed with %d", resErr.Code)
		}
		return fmt.Sprintf("mxc://%s/%s", cfg.Matrix.ServerName, r.MediaMetadata.MediaID), nil
	}

	preview, err := previewer.Preview(req.Context(), rawURL, req.Header.Get("Accept-Language"), store)
	switch {
	case err == nil:
		return util.JSONResponse{Code: http.StatusOK, JSON: preview}
	case errors.Is(err, urlpreview.ErrBusy):
		return util.JSONResponse{Code: http.StatusTooManyRequests, JSON: spec.LimitExceeded("Too many previews are being made", int64(time.Second/time.Millisecond)*5)}
	case errors.Is(err, urlpreview.ErrFetch):
		// What went wrong is for the log: the answer must not tell a user what the server can reach.
		util.GetLogger(req.Context()).WithError(err).WithField("url", rawURL).Info("URL preview failed")
		return util.JSONResponse{Code: http.StatusBadGateway, JSON: spec.Unknown("Failed to fetch the URL")}
	default:
		util.GetLogger(req.Context()).WithError(err).Error("URL preview failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
}

func makePreviewURLHandler(
	cfg *config.MediaAPI, previewer *urlpreview.Previewer, userAPI userapi.MediaUserAPI, rateLimits *httputil.RateLimits,
	db storage.Database, activeThumbnailGeneration *types.ActiveThumbnailGeneration,
) http.Handler {
	return httputil.MakeAuthAPI("preview_url", userAPI, func(req *http.Request, dev *userapi.Device) util.JSONResponse {
		if r := rateLimits.Limit(req, dev); r != nil {
			return *r
		}
		return PreviewURL(req, dev, cfg, previewer, db, activeThumbnailGeneration)
	})
}
