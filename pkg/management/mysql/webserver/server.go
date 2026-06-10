/*
Copyright 2026 The CNMySQL Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package webserver exposes the instance control and observability API the
// operator calls over mutually-authenticated TLS: probes, status, and the
// promote/demote/restart lifecycle commands.
package webserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// InstanceController is the behaviour the HTTP layer drives. It is implemented
// by the real instance manager (backed by the pool and the replication
// package) and faked in tests, keeping the HTTP handlers free of MySQL
// specifics.
type InstanceController interface {
	// Healthz reports liveness: the managed process is up.
	Healthz(ctx context.Context) error
	// Readyz reports readiness: the instance can serve its role.
	Readyz(ctx context.Context) error
	// Status returns the full instance status.
	Status(ctx context.Context) (*Status, error)
	// Promote transitions a replica to primary.
	Promote(ctx context.Context) error
	// Demote transitions a primary to replica (read-only).
	Demote(ctx context.Context) error
	// Restart restarts the managed mysqld process.
	Restart(ctx context.Context) error
}

// Handler builds the http.Handler serving the instance control API. Exposing
// the handler (rather than only a server) lets it be tested with httptest and
// wrapped by the caller for TLS.
func Handler(controller InstanceController) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(controller.Healthz))
	mux.HandleFunc("GET /readyz", healthHandler(controller.Readyz))
	mux.HandleFunc("GET /status", statusHandler(controller))
	mux.HandleFunc("POST /promote", actionHandler(controller.Promote))
	mux.HandleFunc("POST /demote", actionHandler(controller.Demote))
	mux.HandleFunc("POST /restart", actionHandler(controller.Restart))
	return mux
}

// healthHandler maps a probe func to 200 OK / 503 Service Unavailable.
func healthHandler(probe func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := probe(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// statusHandler serves the instance status as JSON.
func statusHandler(controller InstanceController) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := controller.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(status); err != nil {
			writeError(w, http.StatusInternalServerError, err)
		}
	}
}

// actionHandler maps a lifecycle command to 200 OK / 500 on error.
func actionHandler(action func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := action(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func writeError(w http.ResponseWriter, code int, err error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
