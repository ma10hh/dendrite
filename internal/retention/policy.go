// Copyright 2026 Lukas Matt
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package retention holds the rules for message retention policies: what an m.room.retention event may say and how
// it is combined with the limits of the server.
package retention

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/element-hq/dendrite/setup/config"
)

// EventType is the state event type of the retention policy of a room.
const EventType = "m.room.retention"

// Policy is the lifetime of the messages of a room. A lifetime of 0 means that there is none.
type Policy struct {
	MinLifetime time.Duration
	MaxLifetime time.Duration
}

type content struct {
	MinLifetime any `json:"min_lifetime"`
	MaxLifetime any `json:"max_lifetime"`
}

func (c content) millis() (min, max int64, err error) {
	read := func(raw any, name string) (int64, error) {
		if raw == nil {
			return 0, nil
		}
		n, ok := raw.(json.Number)
		if !ok {
			return 0, fmt.Errorf("%s must be an integer number of milliseconds", name)
		}
		v, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer number of milliseconds", name)
		}
		if v < 0 {
			return 0, fmt.Errorf("%s must not be negative", name)
		}
		return v, nil
	}
	if min, err = read(c.MinLifetime, "min_lifetime"); err != nil {
		return
	}
	max, err = read(c.MaxLifetime, "max_lifetime")
	return
}

func decode(raw []byte) (content, error) {
	var c content
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return content{}, errors.New("content of m.room.retention is not valid")
	}
	return c, nil
}

// Validate says what is wrong with the content of an m.room.retention event that a user wants to send.
func Validate(raw []byte) error {
	c, err := decode(raw)
	if err != nil {
		return err
	}
	min, max, err := c.millis()
	if err != nil {
		return err
	}
	if min > 0 && max > 0 && min > max {
		return errors.New("min_lifetime must not be above max_lifetime")
	}
	return nil
}

// FromContent reads the policy of an m.room.retention event. Content that is not valid gives no limits.
func FromContent(raw []byte) Policy {
	c, err := decode(raw)
	if err != nil {
		return Policy{}
	}
	min, max, err := c.millis()
	if err != nil {
		return Policy{}
	}
	const maxMillis = int64(1<<63-1) / int64(time.Millisecond)
	if min > maxMillis {
		min = maxMillis
	}
	if max > maxMillis {
		max = maxMillis
	}
	return Policy{
		MinLifetime: time.Duration(min) * time.Millisecond,
		MaxLifetime: time.Duration(max) * time.Millisecond,
	}
}

// Effective is the policy that applies to a room: the one of its m.room.retention event (nil if it has none) or the
// default of the server, with the lifetimes moved into the range the server allows.
func Effective(cfg *config.MessageRetention, room *Policy) Policy {
	p := Policy{
		MinLifetime: cfg.DefaultPolicy.MinLifetime,
		MaxLifetime: cfg.DefaultPolicy.MaxLifetime,
	}
	if room != nil {
		p = *room
	}
	clamp := func(d time.Duration) time.Duration {
		if d <= 0 {
			return 0
		}
		if cfg.AllowedLifetimeMin > 0 && d < cfg.AllowedLifetimeMin {
			d = cfg.AllowedLifetimeMin
		}
		if cfg.AllowedLifetimeMax > 0 && d > cfg.AllowedLifetimeMax {
			d = cfg.AllowedLifetimeMax
		}
		return d
	}
	return Policy{MinLifetime: clamp(p.MinLifetime), MaxLifetime: clamp(p.MaxLifetime)}
}
