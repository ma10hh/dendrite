// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package perform

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"

	"github.com/element-hq/dendrite/roomserver/api"
	"github.com/element-hq/dendrite/roomserver/storage"
	"github.com/element-hq/dendrite/roomserver/types"
)

const (
	partialStateResyncInterval = 2 * time.Second
	partialStateMaxBackoff     = 10 * time.Minute
)

var (
	partialStateRooms = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "dendrite",
		Subsystem: "roomserver",
		Name:      "partial_state_rooms",
		Help:      "Rooms that were joined with partial state and have not got their full state yet",
	})
	partialStateResyncs = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dendrite",
		Subsystem: "roomserver",
		Name:      "partial_state_resyncs_total",
		Help:      "Attempts to fetch the full state of a room, by result: success or failure",
	}, []string{"result"})
)

// PartialStateResyncer makes the state of the rooms that were joined with partial state whole: it fetches the
// state of the room, with the memberships that were left out, from the servers in the room, and stores it as the
// state of the room. The records of the rooms are in the database, so it carries on after a restart.
type PartialStateResyncer struct {
	DB    storage.Database
	Admin *Admin
	// Interval is how often the rooms are looked at. It defaults to two seconds.
	Interval time.Duration

	mu       sync.Mutex
	failures map[types.RoomNID]int
	retryAt  map[types.RoomNID]time.Time
}

// Run resyncs the rooms until the context ends.
func (r *PartialStateResyncer) Run(ctx context.Context) {
	interval := r.Interval
	if interval == 0 {
		interval = partialStateResyncInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		r.ResyncAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ResyncAll goes through the rooms that have partial state once, the ones joined first first. A room that failed is
// tried again later, after longer each time.
func (r *PartialStateResyncer) ResyncAll(ctx context.Context) {
	rooms, err := r.DB.PartialStateRooms(ctx)
	if err != nil {
		logrus.WithError(err).Error("failed to list the rooms with partial state")
		return
	}
	partialStateRooms.Set(float64(len(rooms)))
	for _, room := range rooms {
		if ctx.Err() != nil {
			return
		}
		if !r.due(room.RoomNID) {
			continue
		}
		logger := logrus.WithField("room_nid", room.RoomNID)
		if err := r.Resync(ctx, room); err != nil {
			partialStateResyncs.WithLabelValues("failure").Inc()
			delay := r.failed(room.RoomNID)
			logger.WithError(err).Warnf("failed to fetch the full state of a room, trying again in %s", delay)
			continue
		}
		partialStateResyncs.WithLabelValues("success").Inc()
		r.succeeded(room.RoomNID)
	}
}

// Resync fetches the state of one room and clears its record.
func (r *PartialStateResyncer) Resync(ctx context.Context, partial types.PartialState) error {
	info, err := r.DB.RoomInfoByNID(ctx, partial.RoomNID)
	if err != nil {
		return fmt.Errorf("r.DB.RoomInfoByNID: %w", err)
	}
	if info == nil {
		// The room was purged.
		return r.DB.ClearRoomPartialState(ctx, partial.RoomNID)
	}

	// The state is stored by an event of a user of this server in the room, the one that joined.
	events, err := r.DB.EventsFromIDs(ctx, info, []string{partial.JoinEventID})
	if err != nil || len(events) != 1 || events[0].PDU == nil {
		return fmt.Errorf("the join event %s is not known: %v", partial.JoinEventID, err)
	}
	roomIDString := events[0].RoomID().String()
	roomID, err := spec.NewRoomID(roomIDString)
	if err != nil {
		return err
	}
	userID, err := r.Admin.Queryer.QueryUserIDForSender(ctx, *roomID, events[0].SenderID())
	if err != nil || userID == nil {
		return fmt.Errorf("the user of the join event %s is not known: %v", partial.JoinEventID, err)
	}
	if left, lerr := r.userLeft(ctx, info, events[0].SenderID()); lerr != nil {
		return lerr
	} else if left {
		// Nobody of this server is in the room any more, so there is no state to have.
		logrus.WithField("room_id", roomIDString).Info("the user of a room with partial state left it, dropping the record")
		return r.DB.ClearRoomPartialState(ctx, partial.RoomNID)
	}

	var lastErr error = fmt.Errorf("no server to ask")
	for _, server := range partial.Servers {
		if r.Admin.Cfg.Matrix.IsLocalServerName(spec.ServerName(server)) {
			continue
		}
		if lastErr = r.fetchMembers(ctx, info, events[0].PDU, spec.ServerName(server)); lastErr == nil {
			logrus.WithFields(logrus.Fields{"room_id": roomIDString, "server": server}).Info("the room has its full state now")
			return r.DB.ClearRoomPartialState(ctx, partial.RoomNID)
		}
		logrus.WithError(lastErr).WithFields(logrus.Fields{"room_id": roomIDString, "server": server}).Warn("failed to fetch the state of a room")
	}
	return lastErr
}

// fetchMembers asks the server for the state of the room at the join, checks the signatures of the memberships in
// it, stores them as outliers (which checks them against their auth events) and has the roomserver add them to the
// state of the room. No event is created, and nothing is sent to other servers.
func (r *PartialStateResyncer) fetchMembers(
	ctx context.Context, info *types.RoomInfo, join gomatrixserverlib.PDU, server spec.ServerName,
) error {
	in := r.Admin.Inputer
	resp, err := in.FSAPI.LookupState(ctx, in.ServerName, server, join.RoomID().String(), join.EventID(), info.RoomVersion)
	if err != nil {
		return fmt.Errorf("LookupState: %w", err)
	}
	var inputs []api.InputRoomEvent
	var memberIDs []string
	for _, ev := range resp.GetStateEvents().UntrustedEvents(info.RoomVersion) {
		if ev.Type() != spec.MRoomMember || ev.StateKey() == nil || ev.RoomID().String() != join.RoomID().String() {
			continue
		}
		if verr := gomatrixserverlib.VerifyEventSignatures(ctx, ev, in.KeyRing, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
			return r.Admin.Queryer.QueryUserIDForSender(ctx, roomID, senderID)
		}); verr != nil {
			continue
		}
		inputs = append(inputs, api.InputRoomEvent{Kind: api.KindOutlier, Event: &types.HeaderedEvent{PDU: ev}, Origin: server})
		memberIDs = append(memberIDs, ev.EventID())
	}

	// An event the roomserver rejects is not an error here: it is left out of the state.
	if len(inputs) > 0 {
		var res api.InputRoomEventsResponse
		in.InputRoomEvents(ctx, &api.InputRoomEventsRequest{InputRoomEvents: inputs}, &res)
		if res.ErrMsg != "" && res.ErrMsg != api.InputWasRejected {
			return fmt.Errorf("storing the memberships: %s", res.ErrMsg)
		}
	}
	var res api.InputRoomEventsResponse
	in.InputRoomEvents(ctx, &api.InputRoomEventsRequest{InputRoomEvents: []api.InputRoomEvent{{
		Kind:           api.KindAddMembers,
		Event:          &types.HeaderedEvent{PDU: join},
		Origin:         server,
		MemberEventIDs: memberIDs,
	}}}, &res)
	if res.ErrMsg != "" {
		return fmt.Errorf("adding the memberships: %s", res.ErrMsg)
	}
	return nil
}

// userLeft reports whether the user is no longer joined to the room: the state could not be stored by an event of
// theirs.
func (r *PartialStateResyncer) userLeft(ctx context.Context, info *types.RoomInfo, sender spec.SenderID) (bool, error) {
	_, stillInRoom, _, err := r.DB.GetMembership(ctx, info.RoomNID, sender)
	if err != nil {
		return false, err
	}
	return !stillInRoom, nil
}

func (r *PartialStateResyncer) due(room types.RoomNID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !time.Now().Before(r.retryAt[room])
}

func (r *PartialStateResyncer) failed(room types.RoomNID) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == nil {
		r.failures, r.retryAt = map[types.RoomNID]int{}, map[types.RoomNID]time.Time{}
	}
	r.failures[room]++
	delay := min(time.Duration(1<<min(r.failures[room], 10))*time.Second, partialStateMaxBackoff)
	r.retryAt[room] = time.Now().Add(delay)
	return delay
}

func (r *PartialStateResyncer) succeeded(room types.RoomNID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, room)
	delete(r.retryAt, room)
}
