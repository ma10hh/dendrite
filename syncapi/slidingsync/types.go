// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package slidingsync implements Simplified Sliding Sync (MSC4186) in the dialect
// served at /_matrix/client/unstable/org.matrix.simplified_msc3575/sync, which is
// what Element X (matrix-rust-sdk / Ruma's `sync_events::msc4186`) speaks.
//
// The `pos`, `timeout` and `set_presence` parameters travel in the query string.
// The request is not sticky: every request carries the full set of lists and
// subscriptions.
package slidingsync

import (
	"encoding/json"

	"github.com/element-hq/dendrite/syncapi/synctypes"
)

// Request is the JSON body of a sliding sync request.
type Request struct {
	ConnID            string                     `json:"conn_id,omitempty"`
	TxnID             string                     `json:"txn_id,omitempty"`
	Lists             map[string]ListRequest     `json:"lists,omitempty"`
	RoomSubscriptions map[string]RoomConfig      `json:"room_subscriptions,omitempty"`
	Extensions        map[string]json.RawMessage `json:"extensions,omitempty"`

	// ext is Extensions, parsed and validated.
	ext extensionConfig
}

// RoomConfig describes what to return for each room matched by a list or subscription.
type RoomConfig struct {
	RequiredState []StateKeyPair `json:"required_state,omitempty"`
	TimelineLimit int            `json:"timeline_limit"`
}

// ListRequest is a set of sliding windows over the user's rooms.
type ListRequest struct {
	RoomConfig
	Ranges  [][2]int64   `json:"ranges"`
	Filters *ListFilters `json:"filters,omitempty"`
}

// ListFilters restricts which rooms can appear in a list. Set fields are ANDed.
// A null entry in room_types or not_room_types matches rooms without a type.
type ListFilters struct {
	IsDM         *bool     `json:"is_dm,omitempty"`
	IsEncrypted  *bool     `json:"is_encrypted,omitempty"`
	IsInvite     *bool     `json:"is_invite,omitempty"`
	RoomTypes    []*string `json:"room_types,omitempty"`
	NotRoomTypes []*string `json:"not_room_types,omitempty"`
	// Spaces keeps the rooms that are direct children of any of these spaces the user has joined (subspaces are not followed).
	Spaces []string `json:"spaces,omitempty"`
	// Tags keeps the rooms the user tagged with any of these tags; NotTags drops rooms with any of them and wins.
	Tags    []string `json:"tags,omitempty"`
	NotTags []string `json:"not_tags,omitempty"`
}

// StateKeyPair is a `[event type, state key]` entry of `required_state`.
type StateKeyPair [2]string

// Response is the JSON body of a sliding sync response.
type Response struct {
	TxnID      string                  `json:"txn_id,omitempty"`
	Pos        string                  `json:"pos"`
	Lists      map[string]ListResponse `json:"lists,omitempty"`
	Rooms      map[string]RoomResponse `json:"rooms,omitempty"`
	Extensions *ExtensionsResponse     `json:"extensions,omitempty"`
}

// ListResponse carries the number of rooms matching a list's filters, ignoring its ranges.
type ListResponse struct {
	Count int `json:"count"`
}

// RoomResponse describes one room. Absent fields mean "unchanged" unless Initial is set,
// in which case they mean "not set".
type RoomResponse struct {
	Name *string `json:"name,omitempty"`
	// Avatar is a JSON string, or the literal null when the avatar was removed.
	Avatar  json.RawMessage `json:"avatar,omitempty"`
	Initial bool            `json:"initial,omitempty"`
	IsDM    *bool           `json:"is_dm,omitempty"`
	Heroes  []Hero          `json:"heroes,omitempty"`

	// InviteState is set for rooms the user has an unaccepted invite to.
	InviteState []json.RawMessage `json:"invite_state,omitempty"`

	NotificationCount *int `json:"notification_count,omitempty"`
	HighlightCount    *int `json:"highlight_count,omitempty"`

	RequiredState []synctypes.ClientEvent `json:"required_state,omitempty"`
	Timeline      []synctypes.ClientEvent `json:"timeline,omitempty"`
	PrevBatch     string                  `json:"prev_batch,omitempty"`
	Limited       bool                    `json:"limited,omitempty"`
	NumLive       *int                    `json:"num_live,omitempty"`
	// ExpandedTimeline is set when the timeline holds older events than were sent before, because the client asked for more.
	ExpandedTimeline bool   `json:"unstable_expanded_timeline,omitempty"`
	BumpStamp        *int64 `json:"bump_stamp,omitempty"`

	JoinedCount  *int `json:"joined_count,omitempty"`
	InvitedCount *int `json:"invited_count,omitempty"`
}

// Hero is a user that clients can use to compute a room name.
type Hero struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"displayname,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}
