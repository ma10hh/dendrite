// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage

import (
	"context"

	"github.com/element-hq/dendrite/mediaapi/types"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type Database interface {
	MediaRepository
	Thumbnails
	MediaAdmin
}

// MediaAdmin is what the admin API does with media.
type MediaAdmin interface {
	SetMediaQuarantined(ctx context.Context, mediaID types.MediaID, origin spec.ServerName, quarantined bool) (bool, error)
	SetUserMediaQuarantined(ctx context.Context, userID string, quarantined bool) (int64, error)
	ListUserMedia(ctx context.Context, userID string, from, limit int, orderBy string, backwards bool) ([]types.MediaMetadata, int, error)
	MediaToDelete(ctx context.Context, origin spec.ServerName, beforeTS spec.Timestamp, largerThan int64) ([]types.MediaMetadata, error)
	DeleteMedia(ctx context.Context, mediaID types.MediaID, origin spec.ServerName) (meta *types.MediaMetadata, fileInUse bool, err error)
}

type MediaRepository interface {
	StoreMediaMetadata(ctx context.Context, mediaMetadata *types.MediaMetadata) error
	GetMediaMetadata(ctx context.Context, mediaID types.MediaID, mediaOrigin spec.ServerName) (*types.MediaMetadata, error)
	GetMediaMetadataByHash(ctx context.Context, mediaHash types.Base64Hash, mediaOrigin spec.ServerName) (*types.MediaMetadata, error)
}

type Thumbnails interface {
	StoreThumbnail(ctx context.Context, thumbnailMetadata *types.ThumbnailMetadata) error
	GetThumbnail(ctx context.Context, mediaID types.MediaID, mediaOrigin spec.ServerName, width, height int, resizeMethod string) (*types.ThumbnailMetadata, error)
	GetThumbnails(ctx context.Context, mediaID types.MediaID, mediaOrigin spec.ServerName) ([]*types.ThumbnailMetadata, error)
}
