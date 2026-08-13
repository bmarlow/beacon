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

package export

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	certutil "k8s.io/client-go/util/cert"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
)

// summaryPath is the export endpoint's URL path. It is also the
// nonResourceURLs value a caller's RBAC must grant (see Server doc).
const summaryPath = "/api/v1/export/summary"

// Server serves the machine-oriented summary export endpoint for multi-cluster
// fleets: GET /api/v1/export/summary returns a Summary (see model.go) as JSON.
//
// Unlike the human dashboard (an OpenShift oauth-proxy front-end plus per-user
// SubjectAccessReview filtering), this endpoint authenticates the caller's
// bearer token via TokenReview and authorizes it via a SubjectAccessReview
// against the request's non-resource URL and verb, evaluated by the
// kube-apiserver exactly like RBAC for any other request. This is a small,
// self-contained middleware (deliberately not
// sigs.k8s.io/controller-runtime/pkg/metrics/filters.
// WithAuthenticationAndAuthorization, which pulls in k8s.io/apiserver and a
// large transitive dependency tree for what is, at this scale, ~50 lines of
// logic) built on the SAME controller-runtime client.Client Create pattern
// already used for the dashboard's per-user checks (see webui/authz.go). It
// requires no new RBAC on Beacon's own ServiceAccount beyond what the
// dashboard already needs: create on tokenreviews/subjectaccessreviews.
//
// The CALLER (e.g. a hub cluster's ServiceAccount) needs its own ClusterRole
// granting it this path, e.g.:
//
//	rules:
//	- nonResourceURLs: ["/api/v1/export/summary"]
//	  verbs: ["get"]
//
// TLS is mandatory — bearer tokens must never be sent in the clear. When
// CertDir is unset, Server generates an ephemeral self-signed certificate at
// startup (fine behind an OpenShift Route with edge/reencrypt termination;
// supply CertDir — e.g. a mounted OpenShift service-serving cert, as used for
// the metrics endpoint — for a cluster-verifiable certificate instead).
type Server struct {
	// Addr is the bind address, e.g. ":8083". The server does not start when
	// empty (opt-in; mirrors the dashboard's --dashboard-bind-address convention).
	Addr string
	// Client authenticates/authorizes callers (TokenReview/SubjectAccessReview)
	// and reads the GatewayHealthPolicy singleton.
	Client client.Client
	// PolicyName is the singleton GatewayHealthPolicy name.
	PolicyName string
	// CertDir, when set, is a directory containing tls.crt/tls.key. When
	// empty, an ephemeral self-signed certificate is generated at startup.
	CertDir string
}

// AddToManager registers the Server as a manager Runnable.
func (s *Server) AddToManager(mgr manager.Manager) error {
	return mgr.Add(s)
}

// NeedLeaderElection makes the export endpoint run on every replica (it only
// reads the already-computed, shared GatewayHealthPolicy status).
func (s *Server) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable. Does nothing when Addr is empty.
func (s *Server) Start(ctx context.Context) error {
	if s.Addr == "" {
		return nil
	}
	logger := log.FromContext(ctx).WithName("export")

	mux := http.NewServeMux()
	mux.Handle(summaryPath, s.authenticate(logger, http.HandlerFunc(s.handleSummary)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	tlsConfig, err := s.tlsConfig()
	if err != nil {
		return fmt.Errorf("preparing export TLS certificate: %w", err)
	}

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConfig,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("starting multi-cluster export endpoint", "addr", s.Addr, "path", summaryPath)
	// Certificates are already loaded into srv.TLSConfig; passing empty paths
	// tells net/http to use them instead of reading from disk.
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// tlsConfig loads CertDir's tls.crt/tls.key, or generates an ephemeral
// self-signed certificate when CertDir is empty.
func (s *Server) tlsConfig() (*tls.Config, error) {
	var cert tls.Certificate
	var err error
	if s.CertDir != "" {
		cert, err = tls.LoadX509KeyPair(filepath.Join(s.CertDir, "tls.crt"), filepath.Join(s.CertDir, "tls.key"))
		if err != nil {
			return nil, fmt.Errorf("loading certificate from %s: %w", s.CertDir, err)
		}
	} else {
		certPEM, keyPEM, genErr := certutil.GenerateSelfSignedCertKey("beacon-export", nil, nil)
		if genErr != nil {
			return nil, fmt.Errorf("generating self-signed certificate: %w", genErr)
		}
		cert, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// authenticate wraps next with bearer-token authentication (TokenReview) and
// non-resource-URL authorization (SubjectAccessReview), both delegated to the
// kube-apiserver — the same trust boundary as any other RBAC-governed request.
func (s *Server) authenticate(logger logr.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		token, ok := bearerToken(r)
		if !ok {
			http.Error(w, "unauthorized: missing bearer token", http.StatusUnauthorized)
			return
		}

		tr := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}
		if err := s.Client.Create(ctx, tr); err != nil {
			logger.Error(err, "TokenReview request failed")
			http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if !tr.Status.Authenticated {
			http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
		user := tr.Status.User

		sar := &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{
				User:   user.Username,
				Groups: user.Groups,
				UID:    user.UID,
				NonResourceAttributes: &authzv1.NonResourceAttributes{
					Path: r.URL.Path,
					Verb: strings.ToLower(r.Method),
				},
			},
		}
		if err := s.Client.Create(ctx, sar); err != nil {
			logger.Error(err, "SubjectAccessReview request failed")
			http.Error(w, "authorization unavailable", http.StatusServiceUnavailable)
			return
		}
		if !sar.Status.Allowed {
			http.Error(w, fmt.Sprintf("forbidden: %s is not allowed to %s %s", user.Username, r.Method, r.URL.Path),
				http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token from a "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}

// handleSummary serves the current Summary as JSON.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pol := &beaconv1alpha1.GatewayHealthPolicy{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: s.PolicyName}, pol); err != nil {
		if !apierrors.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// No policy configured yet: return an empty-but-valid summary rather
		// than erroring, matching the dashboard's tolerant behavior.
		pol = &beaconv1alpha1.GatewayHealthPolicy{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(Build(pol)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
