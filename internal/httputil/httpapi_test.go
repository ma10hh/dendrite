// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package httputil

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matrix-org/util"
	"github.com/prometheus/client_golang/prometheus"
)

func TestWrapHandlerInBasicAuth(t *testing.T) {
	type args struct {
		h http.Handler
		b BasicAuth
	}

	dummyHandler := http.HandlerFunc(func(h http.ResponseWriter, r *http.Request) {
		h.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name    string
		args    args
		want    int
		reqAuth bool
	}{
		{
			name:    "no user or password setup",
			args:    args{h: dummyHandler},
			want:    http.StatusOK,
			reqAuth: false,
		},
		{
			name: "only user set",
			args: args{
				h: dummyHandler,
				b: BasicAuth{Username: "test"}, // no basic auth
			},
			want:    http.StatusOK,
			reqAuth: false,
		},
		{
			name: "only pass set",
			args: args{
				h: dummyHandler,
				b: BasicAuth{Password: "test"}, // no basic auth
			},
			want:    http.StatusOK,
			reqAuth: false,
		},
		{
			name: "credentials correct",
			args: args{
				h: dummyHandler,
				b: BasicAuth{Username: "test", Password: "test"}, // basic auth enabled
			},
			want:    http.StatusOK,
			reqAuth: true,
		},
		{
			name: "credentials wrong",
			args: args{
				h: dummyHandler,
				b: BasicAuth{Username: "test1", Password: "test"}, // basic auth enabled
			},
			want:    http.StatusForbidden,
			reqAuth: true,
		},
		{
			name: "no basic auth in request",
			args: args{
				h: dummyHandler,
				b: BasicAuth{Username: "test", Password: "test"}, // basic auth enabled
			},
			want:    http.StatusForbidden,
			reqAuth: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baHandler := WrapHandlerInBasicAuth(tt.args.h, tt.args.b)

			req := httptest.NewRequest("GET", "http://localhost/metrics", nil)
			if tt.reqAuth {
				req.SetBasicAuth("test", "test")
			}

			w := httptest.NewRecorder()
			baHandler(w, req)
			resp := w.Result()

			if resp.StatusCode != tt.want {
				t.Errorf("Expected status code %d, got %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestRequestDurationMetric(t *testing.T) {
	h := MakeExternalAPI("metric_test_handler", func(*http.Request) util.JSONResponse {
		return util.JSONResponse{Code: http.StatusTeapot, JSON: struct{}{}}
	})
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusTeapot {
			t.Fatalf("code %d", rec.Code)
		}
	}
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var samples uint64
	for _, mf := range mfs {
		if mf.GetName() != "dendrite_http_request_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["handler"] == "metric_test_handler" && labels["code"] == "418" {
				samples = m.GetHistogram().GetSampleCount()
			}
		}
	}
	if samples != 2 {
		t.Errorf("observed %d requests of the handler with code 418, want 2", samples)
	}
}
