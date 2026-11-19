// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestParsePos(t *testing.T) {
	sessionID, seq, err := ParsePos("abc/42")
	if err != nil || sessionID != "abc" || seq != 42 {
		t.Fatalf("got (%q, %d, %v)", sessionID, seq, err)
	}
	for _, bad := range []string{"", "abc", "/1", "abc/", "abc/-1", "abc/x"} {
		if _, _, err := ParsePos(bad); err == nil {
			t.Errorf("ParsePos(%q) should fail", bad)
		}
	}
}

func TestStoreCreateReplacesExisting(t *testing.T) {
	s := NewConnectionStore(time.Hour, 5)
	first, _ := s.Create("@a:x", "D", "c")
	second, _ := s.Create("@a:x", "D", "c")
	if first.sessionID == second.sessionID {
		t.Fatal("new session must get a fresh session ID")
	}
	if got := s.Get("@a:x", "D", "c", first.sessionID); got != nil {
		t.Error("old session must no longer resolve")
	}
	if got := s.Get("@a:x", "D", "c", second.sessionID); got != second {
		t.Error("new session must resolve")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestStoreKeysAreIsolated(t *testing.T) {
	s := NewConnectionStore(time.Hour, 5)
	a, _ := s.Create("@a:x", "D", "c")
	if s.Get("@b:x", "D", "c", a.sessionID) != nil {
		t.Error("other user must not see the connection")
	}
	if s.Get("@a:x", "OTHER", "c", a.sessionID) != nil {
		t.Error("other device must not see the connection")
	}
	if s.Get("@a:x", "D", "other-conn", a.sessionID) != nil {
		t.Error("other conn_id must not see the connection")
	}
}

func TestStoreExpiresIdleConnections(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	s := newConnectionStore(10*time.Minute, 5, clock.now)
	idle, _ := s.Create("@a:x", "D", "idle")
	busy, _ := s.Create("@a:x", "D", "busy")

	clock.advance(8 * time.Minute)
	s.Touch(busy)
	clock.advance(5 * time.Minute) // idle is now 13m old, busy 5m

	if s.Get("@a:x", "D", "idle", idle.sessionID) != nil {
		t.Error("idle connection should have expired")
	}
	if s.Get("@a:x", "D", "busy", busy.sessionID) == nil {
		t.Error("touched connection should survive")
	}
}

func TestStoreCapsConnectionsPerDevice(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	s := newConnectionStore(time.Hour, 2, clock.now)
	oldest, _ := s.Create("@a:x", "D", "1")
	clock.advance(time.Second)
	middle, _ := s.Create("@a:x", "D", "2")
	clock.advance(time.Second)
	other, _ := s.Create("@a:x", "OTHER", "1")
	clock.advance(time.Second)
	newest, _ := s.Create("@a:x", "D", "3")

	if s.Get("@a:x", "D", "1", oldest.sessionID) != nil {
		t.Error("least recently used connection of the device should be evicted")
	}
	if s.Get("@a:x", "D", "2", middle.sessionID) == nil || s.Get("@a:x", "D", "3", newest.sessionID) == nil {
		t.Error("newer connections must be kept")
	}
	if s.Get("@a:x", "OTHER", "1", other.sessionID) == nil {
		t.Error("other devices must not be affected by the cap")
	}
}

func TestStoreNonPositiveCapStillWorks(t *testing.T) {
	s := NewConnectionStore(time.Hour, 0)
	if _, err := s.Create("@a:x", "D", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("@a:x", "D", "2"); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestConnectionAcquireHonoursContext(t *testing.T) {
	conn, _ := NewConnectionStore(time.Hour, 5).Create("@a:x", "D", "c")
	if err := conn.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := conn.Acquire(ctx); err == nil {
		t.Fatal("second Acquire must block until the context is done")
	}
	conn.Release()
	if err := conn.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	conn.Release()
}
