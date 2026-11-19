// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultConnectionTTL is how long an idle connection is kept before it is forgotten.
	DefaultConnectionTTL = 30 * time.Minute
	// DefaultMaxConnectionsPerDevice bounds the memory a single device can pin.
	DefaultMaxConnectionsPerDevice = 10

	sweepInterval = time.Minute
)

type connectionKey struct {
	userID   string
	deviceID string
	connID   string
}

// Connection is the server-side state of one sliding sync session. All fields
// except those documented otherwise are guarded by holding the connection (see Acquire).
type Connection struct {
	key       connectionKey
	sessionID string
	lock      chan struct{}

	// pos is the sequence number of the last response sent. Zero means no response yet.
	pos uint64
	// lastRequestPos and lastResponse allow replaying the previous response when
	// the client retries a request because it never received the answer.
	lastRequestPos uint64
	lastResponse   *Response

	// sent is what the connection was last told about each room it has received.
	sent map[string]*roomSent

	// ext is what the extensions remember between responses.
	ext extensionState

	// lastSeen is when the device's last-seen details were last refreshed from this connection.
	lastSeen time.Time

	// lastUsed is guarded by ConnectionStore.mu, not by the connection lock.
	lastUsed time.Time
}

// Acquire serialises requests on one connection. It gives up when ctx is done.
func (c *Connection) Acquire(ctx context.Context) error {
	select {
	case c.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Connection) Release() {
	<-c.lock
}

// Pos returns the position token to hand to the client for the current sequence number.
func (c *Connection) Pos() string {
	return c.sessionID + "/" + strconv.FormatUint(c.pos, 10)
}

// ParsePos splits a position token into session ID and sequence number.
func ParsePos(pos string) (sessionID string, seq uint64, err error) {
	sessionID, seqStr, found := strings.Cut(pos, "/")
	if !found || sessionID == "" {
		return "", 0, fmt.Errorf("malformed pos")
	}
	seq, err = strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("malformed pos: %w", err)
	}
	return sessionID, seq, nil
}

// RecordResponse advances the connection to the next position and remembers
// the response for retries. requestSeq is the position the client sent.
func (c *Connection) RecordResponse(requestSeq uint64, resp *Response) {
	c.lastRequestPos = requestSeq
	c.pos++
	resp.Pos = c.Pos()
	c.lastResponse = resp
}

// ConnectionStore keeps all live connections in memory. Nothing is persisted:
// after a restart or expiry clients get M_UNKNOWN_POS and start a new session.
type ConnectionStore struct {
	mu        sync.Mutex
	conns     map[connectionKey]*Connection
	ttl       time.Duration
	maxPerDev int
	now       func() time.Time
	lastSweep time.Time
}

func NewConnectionStore(ttl time.Duration, maxPerDevice int) *ConnectionStore {
	return newConnectionStore(ttl, maxPerDevice, time.Now)
}

func newConnectionStore(ttl time.Duration, maxPerDevice int, now func() time.Time) *ConnectionStore {
	if maxPerDevice < 1 {
		maxPerDevice = 1
	}
	return &ConnectionStore{
		conns:     make(map[connectionKey]*Connection),
		ttl:       ttl,
		maxPerDev: maxPerDevice,
		now:       now,
		lastSweep: now(),
	}
}

// Create starts a new session for the key, replacing any existing one.
func (s *ConnectionStore) Create(userID, deviceID, connID string) (*Connection, error) {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	key := connectionKey{userID, deviceID, connID}
	now := s.now()
	conn := &Connection{
		key:       key,
		sessionID: hex.EncodeToString(id),
		lock:      make(chan struct{}, 1),
		sent:      make(map[string]*roomSent),
		lastUsed:  now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	delete(s.conns, key)
	s.evictOldestLocked(userID, deviceID)
	s.conns[key] = conn
	return conn, nil
}

// Get returns the live connection for the key if its session ID matches.
func (s *ConnectionStore) Get(userID, deviceID, connID, sessionID string) *Connection {
	key := connectionKey{userID, deviceID, connID}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	conn, ok := s.conns[key]
	if !ok || conn.sessionID != sessionID {
		return nil
	}
	conn.lastUsed = now
	return conn
}

// Touch marks the connection as recently used, e.g. when a long poll finishes.
func (s *ConnectionStore) Touch(conn *Connection) {
	s.mu.Lock()
	conn.lastUsed = s.now()
	s.mu.Unlock()
}

// Len returns the number of live connections.
func (s *ConnectionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// sweepLocked drops expired connections at most once per sweepInterval, so the
// store needs no background goroutine.
func (s *ConnectionStore) sweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) < sweepInterval {
		return
	}
	s.lastSweep = now
	for key, conn := range s.conns {
		if now.Sub(conn.lastUsed) > s.ttl {
			delete(s.conns, key)
		}
	}
}

// evictOldestLocked makes room for one more connection on the device.
func (s *ConnectionStore) evictOldestLocked(userID, deviceID string) {
	for {
		var oldest *Connection
		count := 0
		for key, conn := range s.conns {
			if key.userID != userID || key.deviceID != deviceID {
				continue
			}
			count++
			if oldest == nil || conn.lastUsed.Before(oldest.lastUsed) {
				oldest = conn
			}
		}
		if count < s.maxPerDev {
			return
		}
		delete(s.conns, oldest.key)
	}
}
