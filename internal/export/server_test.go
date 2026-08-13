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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
)

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{header: "Bearer abc123", wantToken: "abc123", wantOK: true},
		{header: "bearer abc123", wantToken: "abc123", wantOK: true}, // case-insensitive scheme
		{header: "", wantOK: false},
		{header: "Bearer ", wantOK: false},
		{header: "Basic abc123", wantOK: false},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, summaryPath, nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		token, ok := bearerToken(r)
		if ok != tc.wantOK || token != tc.wantToken {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, token, ok, tc.wantToken, tc.wantOK)
		}
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := beaconv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeAuthClient simulates the kube-apiserver's TokenReview/SubjectAccessReview
// webhook evaluation on top of the fake client, since the fake client is a
// naive object tracker and would otherwise leave Status zero-valued.
func fakeAuthClient(t *testing.T, validToken string, allowedPaths map[string]bool, extra ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(extra...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				switch o := obj.(type) {
				case *authnv1.TokenReview:
					if o.Spec.Token == validToken {
						o.Status = authnv1.TokenReviewStatus{
							Authenticated: true,
							User:          authnv1.UserInfo{Username: "hub-sa", Groups: []string{"system:serviceaccounts"}},
						}
					} else {
						o.Status = authnv1.TokenReviewStatus{Authenticated: false}
					}
					return nil
				case *authzv1.SubjectAccessReview:
					path := ""
					if o.Spec.NonResourceAttributes != nil {
						path = o.Spec.NonResourceAttributes.Path
					}
					o.Status = authzv1.SubjectAccessReviewStatus{Allowed: allowedPaths[path]}
					return nil
				default:
					return c.Create(ctx, obj, opts...)
				}
			},
		}).
		Build()
}

func TestServer_Authenticate(t *testing.T) {
	logger := log.Log.WithName("test")

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})

	t.Run("missing token -> 401", func(t *testing.T) {
		s := &Server{Client: fakeAuthClient(t, "good-token", map[string]bool{summaryPath: true})}
		r := httptest.NewRequest(http.MethodGet, summaryPath, nil)
		w := httptest.NewRecorder()
		s.authenticate(logger, next).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})

	t.Run("invalid token -> 401", func(t *testing.T) {
		s := &Server{Client: fakeAuthClient(t, "good-token", map[string]bool{summaryPath: true})}
		r := httptest.NewRequest(http.MethodGet, summaryPath, nil)
		r.Header.Set("Authorization", "Bearer wrong-token")
		w := httptest.NewRecorder()
		s.authenticate(logger, next).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})

	t.Run("valid token but not authorized -> 403", func(t *testing.T) {
		s := &Server{Client: fakeAuthClient(t, "good-token", map[string]bool{})} // nothing allowed
		r := httptest.NewRequest(http.MethodGet, summaryPath, nil)
		r.Header.Set("Authorization", "Bearer good-token")
		w := httptest.NewRecorder()
		s.authenticate(logger, next).ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("valid token and authorized -> reaches handler", func(t *testing.T) {
		s := &Server{Client: fakeAuthClient(t, "good-token", map[string]bool{summaryPath: true})}
		r := httptest.NewRequest(http.MethodGet, summaryPath, nil)
		r.Header.Set("Authorization", "Bearer good-token")
		w := httptest.NewRecorder()
		s.authenticate(logger, next).ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.String() != "reached" {
			t.Fatalf("expected request to reach the handler, got status=%d body=%q", w.Code, w.Body.String())
		}
	})
}

func TestServer_HandleSummary(t *testing.T) {
	pol := &beaconv1alpha1.GatewayHealthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: beaconv1alpha1.GatewayHealthPolicyStatus{
			ManagedGateways: 1,
			Cluster:         beaconv1alpha1.ClusterIdentity{Name: "prod-east"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pol).Build()
	s := &Server{Client: cl, PolicyName: "cluster"}

	r := httptest.NewRequest(http.MethodGet, summaryPath, nil)
	w := httptest.NewRecorder()
	s.handleSummary(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got Summary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.ManagedGateways != 1 || got.Cluster.Name != "prod-east" {
		t.Fatalf("unexpected summary: %+v", got)
	}
}

func TestServer_HandleSummary_NoPolicyIsTolerant(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := &Server{Client: cl, PolicyName: "cluster"}

	r := httptest.NewRequest(http.MethodGet, summaryPath, nil)
	w := httptest.NewRecorder()
	s.handleSummary(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no policy configured; body=%s", w.Code, w.Body.String())
	}
}

func TestServer_HandleSummary_MethodNotAllowed(t *testing.T) {
	s := &Server{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), PolicyName: "cluster"}
	r := httptest.NewRequest(http.MethodPost, summaryPath, nil)
	w := httptest.NewRecorder()
	s.handleSummary(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}
