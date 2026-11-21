// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/mediaapi/fileutils"
	"github.com/element-hq/dendrite/mediaapi/storage"
	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/element-hq/dendrite/setup/config"
)

var (
	retentionDeleted = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dendrite",
		Subsystem: "mediaapi",
		Name:      "retention_deleted_total",
		Help:      "Media deleted because it was not used for the configured lifetime, by kind: local or remote",
	}, []string{"kind"})
	uploadsOverQuota = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "dendrite",
		Subsystem: "mediaapi",
		Name:      "uploads_over_quota_total",
		Help:      "Uploads that were refused because the user is out of storage",
	})
)

// touchInterval is how often the time of the last download of one media is written: downloads are far more frequent
// than the lifetimes of media are short, and each write goes through the single writer.
const touchInterval = time.Hour

// retentionBatch is how much media one query of the removal returns.
const retentionBatch = 500

// accessTouches remembers when the last download of a media was written, to write at most one per interval.
type accessTouches struct {
	mu      sync.Mutex
	touched map[string]time.Time
}

var touches = &accessTouches{touched: map[string]time.Time{}}

// shouldTouch says whether the download at the time is to be written, and notes it if so.
func (t *accessTouches) shouldTouch(id types.MediaID, origin spec.ServerName, now time.Time) bool {
	key := string(origin) + "/" + string(id)
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.touched[key]; ok && now.Sub(last) < touchInterval {
		return false
	}
	// Forget everything now and then: the media that are still downloaded are written again within the interval.
	if len(t.touched) >= 10000 {
		t.touched = map[string]time.Time{}
	}
	t.touched[key] = now
	return true
}

// touchMedia records a download of the media for the removal of unused media.
func touchMedia(ctx context.Context, db storage.Database, id types.MediaID, origin spec.ServerName) {
	now := time.Now()
	if !touches.shouldTouch(id, origin, now) {
		return
	}
	if err := db.TouchMedia(ctx, id, origin, spec.AsTimestamp(now)); err != nil {
		log.WithError(err).WithField("media_id", id).Warn("Failed to record the download of media")
	}
}

// mediaFileMu keeps the removal of a media from running between the look-up of a file by its hash and the metadata of an
// upload that refers to the same file.
var mediaFileMu sync.Mutex

// removeMedia deletes the media, and its file and thumbnails if no other media uses them.
func removeMedia(ctx context.Context, cfg *config.MediaAPI, db storage.Database, id types.MediaID, origin spec.ServerName) (bool, error) {
	mediaFileMu.Lock()
	defer mediaFileMu.Unlock()
	meta, inUse, err := db.DeleteMedia(ctx, id, origin)
	if err != nil || meta == nil {
		return false, err
	}
	if !inUse {
		if path, perr := fileutils.GetPathFromBase64Hash(meta.Base64Hash, cfg.AbsBasePath); perr == nil {
			fileutils.RemoveDir(types.Path(filepath.Dir(path)), log.WithField("media_id", id))
		}
	}
	return true, nil
}

// RunRetention deletes the media that was last used longer ago than the configured lifetimes, and says how much it
// deleted. It does nothing for a lifetime of 0.
func RunRetention(ctx context.Context, cfg *config.MediaAPI, db storage.Database, now time.Time) (local, remote int, err error) {
	for _, pass := range []struct {
		local    bool
		lifetime time.Duration
		count    *int
		kind     string
	}{
		{true, cfg.Retention.LocalMediaLifetime, &local, "local"},
		{false, cfg.Retention.RemoteMediaLifetime, &remote, "remote"},
	} {
		if pass.lifetime <= 0 {
			continue
		}
		before := spec.AsTimestamp(now.Add(-pass.lifetime))
		for ctx.Err() == nil {
			batch, qerr := db.UnusedMedia(ctx, pass.local, cfg.Matrix.ServerName, before, retentionBatch)
			if qerr != nil {
				return local, remote, qerr
			}
			deleted := 0
			for _, m := range batch {
				ok, derr := removeMedia(ctx, cfg, db, m.MediaID, m.Origin)
				if derr != nil {
					return local, remote, derr
				}
				if ok {
					deleted++
				}
			}
			*pass.count += deleted
			retentionDeleted.WithLabelValues(pass.kind).Add(float64(deleted))
			// A batch that deleted nothing would come back for the same media for ever.
			if len(batch) < retentionBatch || deleted == 0 {
				break
			}
		}
	}
	return local, remote, ctx.Err()
}

// StartRetention runs the removal of unused media every interval until the context is done. It does not start if no
// lifetime is set.
func StartRetention(ctx context.Context, cfg *config.MediaAPI, db storage.Database) {
	if cfg.Retention.LocalMediaLifetime <= 0 && cfg.Retention.RemoteMediaLifetime <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(cfg.Retention.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				local, remote, err := RunRetention(ctx, cfg, db, time.Now())
				switch {
				case err != nil && ctx.Err() == nil:
					log.WithError(err).Error("Removing unused media failed")
				case local+remote > 0:
					log.WithFields(log.Fields{"local": local, "remote": remote}).Info("Removed unused media")
				}
			}
		}
	}()
}

// checkUserQuota refuses an upload that would take the user above the storage limit.
func checkUserQuota(ctx context.Context, cfg *config.MediaAPI, db storage.Database, userID types.MatrixUserID, size types.FileSizeBytes) *util.JSONResponse {
	if cfg.MaxUserStorageBytes <= 0 {
		return nil
	}
	used, err := db.UserMediaBytes(ctx, string(userID))
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("UserMediaBytes failed")
		return &util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if used+int64(size) > int64(cfg.MaxUserStorageBytes) {
		uploadsOverQuota.Inc()
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.MatrixError{
				ErrCode: "M_RESOURCE_LIMIT_EXCEEDED",
				Err:     "You have used up your storage for media: delete media or ask an administrator",
			},
		}
	}
	return nil
}
