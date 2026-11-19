// Copyright 2025 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package slidingsync

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// errShuttingDown ends requests that were waiting when the server stopped.
var errShuttingDown = errors.New("server is shutting down")

var (
	activeRequests = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dendrite", Subsystem: "syncapi", Name: "sliding_sync_active_requests",
		Help: "The number of sliding sync requests that are being handled right now, including those waiting for changes",
	})
	waitingRequests = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dendrite", Subsystem: "syncapi", Name: "sliding_sync_waiting_requests",
		Help: "The number of sliding sync requests that are waiting for something to return",
	})
	registerOnce sync.Once
)

// registerMetrics exposes the sliding sync metrics. It is safe to call more than once; only the first store is reported.
func registerMetrics(store *ConnectionStore) {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			activeRequests, waitingRequests,
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Namespace: "dendrite", Subsystem: "syncapi", Name: "sliding_sync_connections",
				Help: "The number of sliding sync connections held in memory",
			}, func() float64 { return float64(store.Len()) }),
		)
	})
}
