// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"database/sql"
	"errors"
	"maps"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/element-hq/dendrite/roomserver/api"
	rstypes "github.com/element-hq/dendrite/roomserver/types"
	"github.com/element-hq/dendrite/syncapi/storage"
	"github.com/element-hq/dendrite/syncapi/synctypes"
	"github.com/element-hq/dendrite/syncapi/types"
)

// maxBundledReferences is the most references that are bundled with an event.
const maxBundledReferences = 100

// BundleRelations returns the events with the aggregations of the events that relate to them in
// unsigned["m.relations"]: the thread summary, the latest valid replacement and the references. The events passed in
// are left alone, the ones that get something are copies. State events and redacted events get nothing.
//
// Children the user may not see by the history visibility of the room, and children sent by users the user ignores,
// are left out. If something cannot be read the events are returned as they are: a missing summary is better than
// a failed request.
func BundleRelations(
	ctx context.Context, snapshot storage.DatabaseTransaction, rsAPI api.SyncRoomserverAPI,
	userID spec.UserID, events []*rstypes.HeaderedEvent,
) []*rstypes.HeaderedEvent {
	out, err := bundleRelations(ctx, snapshot, rsAPI, userID, events)
	if err != nil {
		logrus.WithError(err).Warn("failed to bundle relations, sending the events without them")
		return events
	}
	return out
}

// nolint:gocyclo
func bundleRelations(
	ctx context.Context, snapshot storage.DatabaseTransaction, rsAPI api.SyncRoomserverAPI,
	userID spec.UserID, events []*rstypes.HeaderedEvent,
) ([]*rstypes.HeaderedEvent, error) {
	seen := map[string]bool{}
	ids := make([]string, 0, len(events))
	idsByRoom := map[string][]string{}
	for _, ev := range events {
		if ev == nil || ev.StateKey() != nil || gjson.GetBytes(ev.Unsigned(), "redacted_because").Exists() || seen[ev.EventID()] {
			continue
		}
		seen[ev.EventID()] = true
		ids = append(ids, ev.EventID())
		idsByRoom[ev.RoomID().String()] = append(idsByRoom[ev.RoomID().String()], ev.EventID())
	}
	if len(ids) == 0 {
		return events, nil
	}

	ignored := map[string]bool{}
	if list, err := snapshot.IgnoresForUser(ctx, userID.String()); err == nil {
		for user := range list.List {
			ignored[user] = true
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// The tables are indexed by room, so they are asked one room at a time.
	bundles := &types.RelationBundles{
		Threads:        map[string]types.ThreadEntry{},
		ThreadChildren: map[string][]types.RelationChild{},
		Edits:          map[string][]string{},
		References:     map[string][]types.RelationChild{},
	}
	for roomID, roomEventIDs := range idsByRoom {
		part, perr := snapshot.RelationBundles(ctx, roomID, roomEventIDs, userID.String(), len(ignored) > 0)
		if perr != nil {
			return nil, perr
		}
		maps.Copy(bundles.Threads, part.Threads)
		maps.Copy(bundles.ThreadChildren, part.ThreadChildren)
		maps.Copy(bundles.Edits, part.Edits)
		maps.Copy(bundles.References, part.References)
	}
	if len(bundles.Threads) == 0 && len(bundles.Edits) == 0 && len(bundles.References) == 0 {
		return events, nil
	}

	// The replies of a thread, without the ones of ignored users.
	threads := map[string]types.ThreadEntry{}
	latestReply := map[string]string{}
	for id, t := range bundles.Threads {
		if len(ignored) > 0 {
			t.Count, t.LatestEventID = 0, ""
			for _, child := range bundles.ThreadChildren[id] {
				if ignored[child.Sender] {
					continue
				}
				t.Count++
				t.LatestEventID = child.EventID
			}
		}
		if t.Count > 0 && t.LatestEventID != "" {
			threads[id] = t
			latestReply[id] = t.LatestEventID
		}
	}

	wanted := make([]string, 0, len(latestReply))
	for _, id := range latestReply {
		wanted = append(wanted, id)
	}
	for _, candidates := range bundles.Edits {
		wanted = append(wanted, candidates...)
	}
	children := map[string]*rstypes.HeaderedEvent{}
	if len(wanted) > 0 {
		loaded, lerr := snapshot.Events(ctx, wanted)
		if lerr != nil {
			return nil, lerr
		}
		visible, verr := ApplyHistoryVisibilityFilter(ctx, snapshot, rsAPI, loaded, nil, userID, "bundle")
		if verr != nil {
			return nil, verr
		}
		for _, ev := range visible {
			children[ev.EventID()] = ev
		}
	}

	toClient := func(ev *rstypes.HeaderedEvent) (*synctypes.ClientEvent, error) {
		return synctypes.ToClientEvent(ev.PDU, synctypes.FormatAll, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
			return rsAPI.QueryUserIDForSender(ctx, roomID, senderID)
		})
	}

	out := make([]*rstypes.HeaderedEvent, len(events))
	copy(out, events)
	for i, ev := range events {
		if ev == nil || !seen[ev.EventID()] {
			continue
		}
		relations := map[string]any{}

		if t, ok := threads[ev.EventID()]; ok {
			if latest := children[t.LatestEventID]; latest != nil {
				if client, cerr := toClient(latest); cerr == nil {
					participated := t.Participated
					if len(ignored) > 0 {
						participated = ev.SenderID() == spec.SenderID(userID.String())
						for _, child := range bundles.ThreadChildren[ev.EventID()] {
							participated = participated || child.Sender == userID.String()
						}
					}
					relations["m.thread"] = map[string]any{
						"latest_event":              client,
						"count":                     t.Count,
						"current_user_participated": participated,
					}
				}
			}
		}

		if edit := latestValidEdit(ev, bundles.Edits[ev.EventID()], children); edit != nil {
			if client, cerr := toClient(edit); cerr == nil {
				relations["m.replace"] = client
			}
		}

		var chunk []map[string]string
		for _, ref := range bundles.References[ev.EventID()] {
			if !ignored[ref.Sender] && len(chunk) < maxBundledReferences {
				chunk = append(chunk, map[string]string{"event_id": ref.EventID})
			}
		}
		if len(chunk) > 0 {
			relations["m.reference"] = map[string]any{"chunk": chunk}
		}

		if len(relations) == 0 {
			continue
		}
		bundled, berr := withUnsignedRelations(ev, relations)
		if berr != nil {
			return nil, berr
		}
		out[i] = bundled
	}
	return out, nil
}

// latestValidEdit returns the most recent of the events that replace the event, by origin_server_ts and then by
// the largest event ID, that are valid: the replacement has to have the new content, and the event must not be a
// replacement itself. The sender and the type were checked by the query, and neither may be a state event.
func latestValidEdit(
	original *rstypes.HeaderedEvent, candidates []string, loaded map[string]*rstypes.HeaderedEvent,
) *rstypes.HeaderedEvent {
	if gjson.GetBytes(original.Content(), "m\\.relates_to.rel_type").String() == "m.replace" {
		return nil
	}
	var best *rstypes.HeaderedEvent
	for _, id := range candidates {
		edit := loaded[id]
		if edit == nil || edit.StateKey() != nil || !gjson.GetBytes(edit.Content(), "m\\.new_content").IsObject() {
			continue
		}
		if best == nil || edit.OriginServerTS() > best.OriginServerTS() ||
			(edit.OriginServerTS() == best.OriginServerTS() && edit.EventID() > best.EventID()) {
			best = edit
		}
	}
	return best
}

// withUnsignedRelations returns a copy of the event with the aggregations in its unsigned data.
func withUnsignedRelations(ev *rstypes.HeaderedEvent, relations map[string]any) (*rstypes.HeaderedEvent, error) {
	copied, err := gomatrixserverlib.MustGetRoomVersion(ev.Version()).NewEventFromTrustedJSON(ev.JSON(), false)
	if err != nil {
		return nil, err
	}
	if err = copied.SetUnsignedField(`m\.relations`, relations); err != nil {
		return nil, err
	}
	clone := *ev
	clone.PDU = copied
	return &clone, nil
}
