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

	"github.com/beacon-operator/beacon/internal/rcache"
	"github.com/beacon-operator/beacon/internal/state"
	"github.com/beacon-operator/beacon/internal/topology"
)

// topologyCacheTTL bounds how long the dashboard's topology.Builder reuses a
// live Pod/Service/Deployment/EndpointSlice read across requests. The
// dashboard rebuilds the full graph from scratch on every request (including
// every auto-refresh poll, per open browser tab); without this, an unchanged
// cluster still pays the full live-read cost every single poll.
//
// This must be comfortably longer than the actual gap between polls, or it
// buys nothing: the client schedules its next poll 5s AFTER the previous one
// *finishes* (see app.js's scheduleAuto), so the true gap is 5s + however
// long the last build took — if the TTL is shorter than that gap, every
// "auto-refresh" ends up being a full cache-cold rebuild anyway. 10s covers
// that gap with room to spare, so steady-state auto-refresh polls hit a warm
// cache (fast) while a real change still surfaces within one or two polls —
// well within the existing withdraw/readvertise dampening timers' own
// multi-second timescales, which this cache doesn't affect at all (those are
// driven by the reconciler's own, separate, uncached reads).
const topologyCacheTTL = 10 * time.Second

// maxConcurrentBuilds bounds how many topology.Builder.Build() calls (each
// itself fanning out to gatewayBuildConcurrency concurrent per-Gateway
// live API calls) may run at once across ALL requests to this dashboard
// instance — every open browser tab (auto-refreshing every 5s), every
// manual refresh click, and every concurrent user. Without this, a burst of
// concurrent requests (e.g. several tabs, or a client re-requesting before a
// slow response returns) each start their own full-graph rebuild; those
// rebuilds compete for the same rate-limited API client and get slower,
// which invites still more overlapping requests — the same thundering-herd
// pileup that a single tab's auto-refresh loop is separately guarded
// against client-side (see app.js's refreshInFlight). This is the
// server-side backstop for every OTHER way overlapping requests can occur.
const maxConcurrentBuilds = 2

// authzCacheTTL bounds how long a SubjectAccessReview result is reused across
// requests (see AccessChecker's doc comment for why this exists — the same
// motivation as topologyCacheTTL, but for the per-object authorization check
// rather than the object data itself). RBAC changes far less frequently than
// pod/service health, so this is intentionally longer than topologyCacheTTL;
// the trade-off is that a just-revoked permission can remain visible in the
// dashboard for up to this long (the underlying Kubernetes API itself is
// unaffected — this only controls what Beacon's own dashboard chooses to
// display).
const authzCacheTTL = 20 * time.Second

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
	// cache short-circuits repeated live Pod/Service/Deployment/EndpointSlice
	// reads across topology builds; see topologyCacheTTL.
	cache *rcache.Cache
	// authzCache short-circuits repeated SubjectAccessReview checks across
	// requests; see authzCacheTTL. Separate from cache because it needs a
	// different (longer) TTL.
	authzCache *rcache.Cache
	// buildSem bounds concurrent topology builds; see maxConcurrentBuilds.
	buildSem chan struct{}
}

// NewServer constructs a Server.
func NewServer(addr string, c client.Client, states *state.Store, policyName string, requireAuth bool) *Server {
	return &Server{
		Addr:        addr,
		Client:      c,
		States:      states,
		PolicyName:  policyName,
		RequireAuth: requireAuth,
		cache:       rcache.New(topologyCacheTTL),
		authzCache:  rcache.New(authzCacheTTL),
		buildSem:    make(chan struct{}, maxConcurrentBuilds),
	}
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
		Cache:      s.cache,
	}

	// When auth is required, derive the user from the oauth-proxy forwarded
	// headers and filter the graph to what that user can read.
	var authedUser string
	if s.RequireAuth {
		user, ok := userFromRequest(r)
		if !ok {
			http.Error(w, "unauthorized: missing forwarded user identity", http.StatusUnauthorized)
			return
		}
		authedUser = user.Name
		builder.Authz = NewAccessChecker(s.Client, user, s.authzCache)
	}

	// Wait for a build slot, but don't wait past the request's own deadline —
	// see maxConcurrentBuilds.
	select {
	case s.buildSem <- struct{}{}:
		defer func() { <-s.buildSem }()
	case <-ctx.Done():
		http.Error(w, "timed out waiting for an available topology-build slot", http.StatusServiceUnavailable)
		return
	}

	graph, err := builder.Build(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	graph.User = authedUser

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
