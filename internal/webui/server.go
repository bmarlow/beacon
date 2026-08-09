/*
Copyright 2026.

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

// Package webui serves the Beacon topology dashboard: a hierarchical view of
// MetalLB IP -> Gateway -> Route -> Service -> Pod with live status. It exposes:
//
//	GET /            -> the HTML dashboard
//	GET /api/topology -> the topology Graph as JSON
//	GET /healthz     -> liveness
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/bmarlow/beacon/internal/state"
	"github.com/bmarlow/beacon/internal/topology"
)

//go:embed static/*
var staticFS embed.FS

// Server serves the dashboard and topology API.
type Server struct {
	Addr       string
	Client     client.Client
	States     *state.Store
	PolicyName string
	// RequireAuth, when true, expects an authenticating reverse proxy
	// (OpenShift oauth-proxy) in front and enforces per-user RBAC filtering via
	// SubjectAccessReviews using the forwarded user identity. When false, the
	// dashboard is unauthenticated and shows everything (local/dev mode).
	RequireAuth bool
}

// NewServer constructs a Server.
func NewServer(addr string, c client.Client, states *state.Store, policyName string, requireAuth bool) *Server {
	return &Server{Addr: addr, Client: c, States: states, PolicyName: policyName, RequireAuth: requireAuth}
}

// Start implements manager.Runnable so the manager owns the HTTP server
// lifecycle (started after leader election is not required; the UI is read-only
// and safe to serve on all replicas).
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("webui")

	mux := http.NewServeMux()

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/topology", s.handleTopology)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("starting topology dashboard", "addr", s.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// NeedLeaderElection makes the UI run on every replica (read-only).
func (s *Server) NeedLeaderElection() bool { return false }

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	builder := &topology.Builder{
		Client:     s.Client,
		States:     s.States,
		PolicyName: s.PolicyName,
	}

	// When auth is required, derive the user from the oauth-proxy forwarded
	// headers and filter the graph to what that user can read.
	if s.RequireAuth {
		user, ok := userFromRequest(r)
		if !ok {
			http.Error(w, "unauthorized: missing forwarded user identity", http.StatusUnauthorized)
			return
		}
		builder.Authz = NewAccessChecker(s.Client, user)
	}

	graph, err := builder.Build(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(graph); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// AddToManager registers the Server as a manager Runnable.
func (s *Server) AddToManager(mgr manager.Manager) error {
	return mgr.Add(s)
}
