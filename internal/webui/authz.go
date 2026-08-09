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

package webui

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// userFromRequest extracts the authenticated identity from the headers set by
// the OpenShift oauth-proxy (running with --pass-user-headers). Returns false
// when no user header is present (request did not traverse the proxy).
func userFromRequest(r *http.Request) (userInfo, bool) {
	name := r.Header.Get("X-Forwarded-User")
	if name == "" {
		// oauth-proxy may also set X-Forwarded-Preferred-Username.
		name = r.Header.Get("X-Forwarded-Preferred-Username")
	}
	if name == "" {
		return userInfo{}, false
	}
	var groups []string
	if g := r.Header.Get("X-Forwarded-Groups"); g != "" {
		for _, part := range strings.Split(g, ",") {
			if p := strings.TrimSpace(part); p != "" {
				groups = append(groups, p)
			}
		}
	}
	return userInfo{Name: name, Groups: groups}, true
}

// AccessChecker answers "can user U perform verb V on resource R?" by issuing
// SubjectAccessReviews against the API server (as the operator's ServiceAccount,
// which is granted create on subjectaccessreviews). It caches results per
// request-scoped user for the lifetime of a single graph build.
//
// Because SARs are evaluated by the API server using the same RBAC the API uses,
// a cluster-admin passes every check (sees everything), and ordinary users only
// pass for resources they can actually read.
type AccessChecker struct {
	client client.Client
	user   userInfo

	mu    sync.Mutex
	cache map[string]bool
}

// userInfo is the authenticated identity extracted from the oauth-proxy headers.
type userInfo struct {
	Name   string
	Groups []string
	// Admin short-circuits all checks to allow (set when the user is known to
	// be a cluster-admin). Left false by default; SARs still yield the correct
	// answer for admins, this is only an optimization when we can detect it.
	Admin bool
}

// NewAccessChecker builds a checker for a specific user.
func NewAccessChecker(c client.Client, user userInfo) *AccessChecker {
	return &AccessChecker{client: c, user: user, cache: map[string]bool{}}
}

// Allowed reports whether the user may perform verb on the given resource.
// group is the API group ("" for core). namespace is "" for cluster-scoped.
func (a *AccessChecker) Allowed(ctx context.Context, verb, group, resource, namespace, name string) bool {
	if a == nil {
		return true // no checker configured (auth disabled) -> allow
	}
	if a.user.Admin {
		return true
	}
	key := verb + "|" + group + "|" + resource + "|" + namespace + "|" + name
	a.mu.Lock()
	if v, ok := a.cache[key]; ok {
		a.mu.Unlock()
		return v
	}
	a.mu.Unlock()

	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   a.user.Name,
			Groups: a.user.Groups,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb:      verb,
				Group:     group,
				Resource:  resource,
				Namespace: namespace,
				Name:      name,
			},
		},
	}
	allowed := false
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := a.client.Create(cctx, sar); err == nil {
		allowed = sar.Status.Allowed
	}

	a.mu.Lock()
	a.cache[key] = allowed
	a.mu.Unlock()
	return allowed
}
