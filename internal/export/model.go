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

// Package export produces the machine-oriented "summary" payload a
// multi-cluster hub polls to aggregate status from many Beacon instances (see
// internal/identity and internal/export.Server). It is deliberately a thin,
// independently-versioned wrapper around GatewayHealthPolicy status — the
// per-Gateway rollup the reconciling leader already computes — rather than the
// full topology.Graph the dashboard serves: cheap to produce and safe to poll
// frequently from many clusters at once. A hub wanting full pod/route/service
// detail for a specific cluster should instead fetch (or deep-link to) that
// cluster's own dashboard.
package export

import "time"

// SchemaVersion versions this payload's JSON contract, independent of the
// dashboard's topology.SchemaVersion (the two may evolve separately since they
// have different consumers/cadences). Bump only for breaking changes
// (renamed/removed fields); prefer additive changes so a hub and many
// independently-upgraded spokes stay compatible.
const SchemaVersion = "v1"

// ClusterInfo identifies the cluster a Summary was generated on. Mirrors
// api/v1alpha1.ClusterIdentity as a plain struct so this package's JSON
// contract doesn't change shape if the CRD's Go type changes.
type ClusterInfo struct {
	// ID is a stable, unique cluster identifier suitable for cross-cluster
	// correlation. Empty when it could not be auto-detected.
	ID string `json:"id,omitempty"`
	// Name is the human-readable cluster name.
	Name string `json:"name,omitempty"`
	// Source records how ID was determined: "OpenShiftClusterVersion",
	// "KubeSystemUID", or "Manual".
	Source string `json:"source,omitempty"`
}

// GatewaySummary is the per-Gateway rollup already computed by the
// reconciling leader (GatewayHealthPolicy.status.gateways).
type GatewaySummary struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// IPs are the load-balancer IP addresses inferred for this Gateway.
	IPs []string `json:"ips,omitempty"`
	// FromMetalLB indicates whether the IPs are sourced from a MetalLB
	// IPAddressPool (and thus advertisement-managed by Beacon).
	FromMetalLB bool `json:"fromMetalLB,omitempty"`
	// Health is "Healthy", "Unhealthy", "Exempt", or "Unknown".
	Health string `json:"health,omitempty"`
	// Advertisement is "Advertised", "Withdrawn", "PendingWithdrawal", or
	// "PendingReadvertise".
	Advertisement string `json:"advertisement,omitempty"`
}

// Summary is the top-level export payload.
type Summary struct {
	// SchemaVersion is the version of this JSON contract; see SchemaVersion.
	SchemaVersion string `json:"schemaVersion"`
	// GeneratedAt is when this snapshot was assembled.
	GeneratedAt time.Time `json:"generatedAt"`
	// OperatorVersion is the Beacon operator build version (git describe/tag).
	OperatorVersion string `json:"operatorVersion"`
	// Cluster identifies the cluster this summary was generated on.
	Cluster ClusterInfo `json:"cluster"`
	// ManagedGateways/AdvertisedIPs/WithdrawnIPs mirror
	// GatewayHealthPolicy.status's aggregate counters.
	ManagedGateways int32 `json:"managedGateways"`
	AdvertisedIPs   int32 `json:"advertisedIPs"`
	WithdrawnIPs    int32 `json:"withdrawnIPs"`
	// Gateways holds the per-Gateway rollup.
	Gateways []GatewaySummary `json:"gateways,omitempty"`
}
