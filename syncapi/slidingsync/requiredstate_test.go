// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"reflect"
	"testing"
)

func TestStateMatcher(t *testing.T) {
	const me = "@me:test"
	for name, tc := range map[string]struct {
		pairs   []StateKeyPair
		matches []StateKeyPair
		misses  []StateKeyPair
	}{
		"exact pair": {
			pairs:   []StateKeyPair{{"m.room.name", ""}},
			matches: []StateKeyPair{{"m.room.name", ""}},
			misses:  []StateKeyPair{{"m.room.name", "x"}, {"m.room.topic", ""}},
		},
		"nothing requested matches nothing": {
			pairs:  nil,
			misses: []StateKeyPair{{"m.room.name", ""}, {"m.room.member", me}},
		},
		"wildcard state key": {
			pairs:   []StateKeyPair{{"m.space.child", "*"}},
			matches: []StateKeyPair{{"m.space.child", "!a:test"}, {"m.space.child", ""}},
			misses:  []StateKeyPair{{"m.space.parent", "!a:test"}},
		},
		"wildcard type": {
			pairs:   []StateKeyPair{{"*", ""}},
			matches: []StateKeyPair{{"m.room.name", ""}, {"m.room.topic", ""}},
			misses:  []StateKeyPair{{"m.room.member", me}},
		},
		"everything": {
			pairs:   []StateKeyPair{{"*", "*"}},
			matches: []StateKeyPair{{"m.room.name", ""}, {"m.room.member", me}, {"custom", "key"}},
		},
		"everything wins over the rest, lazy loading included": {
			pairs:   []StateKeyPair{{"m.room.name", ""}, {"m.room.member", "$LAZY"}, {"*", "*"}},
			matches: []StateKeyPair{{"m.room.member", "@anyone:test"}},
		},
		"$ME is the requesting user": {
			pairs:   []StateKeyPair{{"m.room.member", "$ME"}},
			matches: []StateKeyPair{{"m.room.member", me}},
			misses:  []StateKeyPair{{"m.room.member", "@other:test"}, {"m.room.member", "$ME"}},
		},
		"$LAZY is not a state key match": {
			pairs:  []StateKeyPair{{"m.room.member", "$LAZY"}},
			misses: []StateKeyPair{{"m.room.member", me}, {"m.room.member", "$LAZY"}},
		},
		"$LAZY with every member": {
			pairs:   []StateKeyPair{{"m.room.member", "$LAZY"}, {"m.room.member", "*"}},
			matches: []StateKeyPair{{"m.room.member", "@anyone:test"}},
		},
		"$LAZY means nothing for other types": {
			pairs:   []StateKeyPair{{"m.room.topic", "$LAZY"}},
			matches: []StateKeyPair{{"m.room.topic", "$LAZY"}},
			misses:  []StateKeyPair{{"m.room.topic", ""}},
		},
	} {
		m := newStateMatcher(tc.pairs, me)
		for _, p := range tc.matches {
			if !m.matches(p[0], p[1]) {
				t.Errorf("%s: %v should match", name, p)
			}
		}
		for _, p := range tc.misses {
			if m.matches(p[0], p[1]) {
				t.Errorf("%s: %v should not match", name, p)
			}
		}
	}
}

func TestStateMatcherLazyFlag(t *testing.T) {
	if !newStateMatcher([]StateKeyPair{{"m.room.member", "$LAZY"}}, "@me:test").lazy {
		t.Error("[m.room.member, $LAZY] must enable lazy loading")
	}
	if newStateMatcher([]StateKeyPair{{"m.room.topic", "$LAZY"}}, "@me:test").lazy {
		t.Error("$LAZY only applies to m.room.member")
	}
	if newStateMatcher([]StateKeyPair{{"m.room.member", "$ME"}}, "@me:test").lazy {
		t.Error("$ME is not lazy loading")
	}
}

func TestStateMatcherLookupPlan(t *testing.T) {
	m := newStateMatcher([]StateKeyPair{
		{"m.room.name", ""}, {"m.room.member", "$ME"}, {"m.room.member", "$LAZY"},
		{"m.space.child", "*"}, {"m.call.member", "*"}, {"m.room.name", ""},
	}, "@me:test")

	if m.needsAllState() {
		t.Error("exact pairs and wildcard state keys do not need all state")
	}
	if got, want := m.exactPairs(), []StateKeyPair{{"m.room.member", "@me:test"}, {"m.room.name", ""}}; !reflect.DeepEqual(got, want) {
		t.Errorf("exactPairs = %v, want %v (deduplicated, $ME resolved, wildcard keys and $LAZY left out)", got, want)
	}
	if got, want := m.wildcardKeyTypes(), []string{"m.call.member", "m.space.child"}; !reflect.DeepEqual(got, want) {
		t.Errorf("wildcardKeyTypes = %v, want %v", got, want)
	}

	if !newStateMatcher([]StateKeyPair{{"*", ""}}, "@me:test").needsAllState() {
		t.Error("a wildcard type needs all state")
	}
	if !newStateMatcher([]StateKeyPair{{"*", "*"}}, "@me:test").needsAllState() {
		t.Error("all state needs all state")
	}
}

func TestLazyMembers(t *testing.T) {
	got := lazyMembers([]string{"@b:test", "@a:test", "@b:test"}, []string{"@c:test", "@a:test"})
	if want := []string{"@a:test", "@b:test", "@c:test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lazyMembers = %v, want the sorted union %v", got, want)
	}
	if got := lazyMembers(nil, nil); len(got) != 0 {
		t.Errorf("no timeline means no members, got %v", got)
	}
}
