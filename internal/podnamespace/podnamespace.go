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

// Package podnamespace resolves the namespace the current process (the
// operator's own pod) is running in. Used both to scope the operator's own
// resources (dashboard Secret/ServiceAccount, provisioned Service/Route) to
// its own namespace, and to scope the manager's cache for those namespaced
// types so the operator's RBAC can be a namespaced Role instead of a
// cluster-wide ClusterRole (see cmd/main.go's cache.Options.ByObject wiring).
package podnamespace

import (
	"fmt"
	"os"
	"strings"
)

const (
	// EnvVar is the standard downward-API env var name for the pod's own
	// namespace (see config/manager/manager.yaml / the CSV's Deployment spec).
	EnvVar = "POD_NAMESPACE"
	// ServiceAccountFile is the projected ServiceAccount token's namespace
	// file, used as a fallback when EnvVar isn't set.
	ServiceAccountFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// Get resolves the namespace the current process is running in: EnvVar if
// set, else ServiceAccountFile. Returns an error if neither is available
// (e.g. running outside a cluster without POD_NAMESPACE set), which callers
// should treat as "namespace-scoped features unavailable" rather than fatal.
func Get() (string, error) {
	if v := os.Getenv(EnvVar); v != "" {
		return v, nil
	}
	data, err := os.ReadFile(ServiceAccountFile)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", ServiceAccountFile, err)
	}
	ns := strings.TrimSpace(string(data))
	if ns == "" {
		return "", fmt.Errorf("empty namespace in %s", ServiceAccountFile)
	}
	return ns, nil
}
