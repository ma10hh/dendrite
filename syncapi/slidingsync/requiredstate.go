// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"sort"

	"github.com/matrix-org/gomatrixserverlib/spec"
)

const (
	wildcard  = "*"
	meToken   = "$ME"
	lazyToken = "$LAZY"
)

// stateMatcher decides which current state events a room's required_state asks for.
//
// It follows Synapse's interpretation of `[type, state_key]` pairs:
//   - `*` as the type or the state key matches anything, and `["*","*"]` means all state,
//     including every membership event;
//   - `$ME` as a state key is the requesting user's ID;
//   - `["m.room.member", "$LAZY"]` lazily loads memberships (see lazyMembers) and is not a state key
//     match. `$LAZY` for any other type is just an ordinary, practically unmatchable, state key.
type stateMatcher struct {
	all bool
	// byType maps an event type to the state keys requested for it; `*` matches every state key.
	byType map[string]map[string]struct{}
	// anyType holds the state keys requested for every event type, from pairs like `["*", ""]`.
	anyType map[string]struct{}
	// lazy is set when memberships are to be loaded lazily.
	lazy bool
}

func newStateMatcher(pairs []StateKeyPair, userID string) *stateMatcher {
	m := &stateMatcher{byType: map[string]map[string]struct{}{}, anyType: map[string]struct{}{}}
	for _, p := range pairs {
		evType, stateKey := p[0], p[1]
		if stateKey == meToken {
			stateKey = userID
		}
		switch {
		case evType == wildcard && stateKey == wildcard:
			m.all = true
		case evType == wildcard:
			m.anyType[stateKey] = struct{}{}
		case evType == spec.MRoomMember && stateKey == lazyToken:
			m.lazy = true
		default:
			if m.byType[evType] == nil {
				m.byType[evType] = map[string]struct{}{}
			}
			m.byType[evType][stateKey] = struct{}{}
		}
	}
	return m
}

// matches reports whether a state event is requested explicitly. It does not account for lazy members.
func (m *stateMatcher) matches(evType, stateKey string) bool {
	if m.all {
		return true
	}
	if _, ok := m.anyType[stateKey]; ok {
		return true
	}
	keys := m.byType[evType]
	if _, ok := keys[wildcard]; ok {
		return true
	}
	_, ok := keys[stateKey]
	return ok
}

// needsAllState reports whether the room's whole state has to be read to find the matches.
func (m *stateMatcher) needsAllState() bool {
	return m.all || len(m.anyType) > 0
}

// wildcardKeyTypes lists the event types requested with any state key, for which all of the type's events are needed.
func (m *stateMatcher) wildcardKeyTypes() []string {
	var types []string
	for evType, keys := range m.byType {
		if _, ok := keys[wildcard]; ok {
			types = append(types, evType)
		}
	}
	sort.Strings(types)
	return types
}

// exactPairs lists the `(type, state key)` pairs that can be looked up directly, in a stable order.
func (m *stateMatcher) exactPairs() []StateKeyPair {
	var pairs []StateKeyPair
	for evType, keys := range m.byType {
		for stateKey := range keys {
			if stateKey != wildcard {
				pairs = append(pairs, StateKeyPair{evType, stateKey})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	return pairs
}

// lazyMembers returns the users whose membership events a lazy-loading room needs, given the timeline:
// every sender, and the target of every membership event. Pass the user IDs, not sender IDs.
func lazyMembers(senders, targets []string) []string {
	seen := make(map[string]struct{}, len(senders)+len(targets))
	var users []string
	for _, group := range [][]string{senders, targets} {
		for _, u := range group {
			if _, dup := seen[u]; !dup {
				seen[u] = struct{}{}
				users = append(users, u)
			}
		}
	}
	sort.Strings(users)
	return users
}

// coversAllMembers reports whether every membership event is requested outright, which makes lazy loading redundant.
func (m *stateMatcher) coversAllMembers() bool {
	if m.all {
		return true
	}
	_, ok := m.byType[spec.MRoomMember][wildcard]
	return ok
}
