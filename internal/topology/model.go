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

// Package topology assembles a hierarchical, status-annotated view of the
// MetalLB -> Gateway API -> workload relationships that Beacon manages, for
// display in the operator's web UI.
//
// Hierarchy (root to leaf):
//
//	IPAddressPool (MetalLB)
//	  └─ IP / VIP
//	       └─ Gateway (gateway.networking.k8s.io)
//	            └─ Route (HTTP/GRPC/TCP/TLS)
//	                 └─ backend Service
//	                      └─ Pod (with probe health)
package topology

import "time"

// Status is a normalized status enum shared across all node kinds so the UI can
// render consistent colors.
type Status string

const (
	// StatusHealthy: everything below/at this node is nominal.
	StatusHealthy Status = "Healthy"
	// StatusDegraded: partially unhealthy (some children failing).
	StatusDegraded Status = "Degraded"
	// StatusUnhealthy: failing.
	StatusUnhealthy Status = "Unhealthy"
	// StatusWithdrawn: MetalLB advertisement withdrawn by Beacon.
	StatusWithdrawn Status = "Withdrawn"
	// StatusPending: a dampening timer is running (withdraw/re-advertise).
	StatusPending Status = "Pending"
	// StatusExempt: excluded from health checking (annotation/config/no probes).
	StatusExempt Status = "Exempt"
	// StatusUnknown: state could not be determined.
	StatusUnknown Status = "Unknown"
)

// Graph is the top-level payload returned to the UI.
type Graph struct {
	// GeneratedAt is when this snapshot was assembled.
	GeneratedAt time.Time `json:"generatedAt"`

	// OperatorVersion is the Beacon operator build version (git describe / tag).
	OperatorVersion string `json:"operatorVersion"`

	// ConsoleBaseURL is the OpenShift web console base URL (e.g.
	// https://console-openshift-console.apps.example.com), used by the UI to
	// build per-resource console links. Empty when not on OpenShift / unknown.
	ConsoleBaseURL string `json:"consoleBaseURL,omitempty"`

	// User is the authenticated username the graph was rendered for (empty when
	// auth is disabled). Displayed in the header.
	User string `json:"user,omitempty"`

	// MetalLBNamespace is the namespace the pools were read from.
	MetalLBNamespace string `json:"metallbNamespace"`

	// Pools are the MetalLB IP address pools that back one or more Gateways.
	Pools []PoolNode `json:"pools"`

	// UnpooledGateways are managed Gateways whose IPs are not sourced from any
	// MetalLB pool (observed but not advertisement-managed by Beacon).
	UnpooledGateways []GatewayNode `json:"unpooledGateways,omitempty"`

	// Summary holds aggregate counters for the header.
	Summary Summary `json:"summary"`
}

// Summary holds aggregate counts for the UI header/badges.
type Summary struct {
	Pools            int `json:"pools"`
	Gateways         int `json:"gateways"`
	Routes           int `json:"routes"`
	Services         int `json:"services"`
	Pods             int `json:"pods"`
	AdvertisedIPs    int `json:"advertisedIPs"`
	WithdrawnIPs     int `json:"withdrawnIPs"`
	UnhealthyGateway int `json:"unhealthyGateways"`
}

// Ref identifies the Kubernetes object behind a node so the UI can build an
// OpenShift console link. For core resources Group is "" and the console uses
// the plural (e.g. "pods"); for CRD-backed resources the console uses the
// group~version~Kind form.
type Ref struct {
	Group         string `json:"group,omitempty"`
	Version       string `json:"version,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Plural        string `json:"plural,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name,omitempty"`
	ClusterScoped bool   `json:"clusterScoped,omitempty"`
}

// StatusTiming records how long a component has been in its current status.
// Embedded into each node so the UI can render "for 3m12s".
type StatusTiming struct {
	// StatusSince is when the component entered its current status (RFC3339).
	// Omitted when unknown.
	StatusSince *time.Time `json:"statusSince,omitempty"`
	// StatusForSeconds is the number of seconds the component has been in its
	// current status. 0 when unknown.
	StatusForSeconds int64 `json:"statusForSeconds,omitempty"`
}

// PoolNode is a MetalLB IPAddressPool and the VIPs allocated from it.
type PoolNode struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	Addresses  []string `json:"addresses"`
	AutoAssign *bool    `json:"autoAssign,omitempty"`
	// Restricted is true when the pool is shown for context (it backs a Gateway
	// the user can see) but the user cannot directly read the IPAddressPool.
	// The UI renders such pools without a clickable console link.
	Restricted   bool   `json:"restricted,omitempty"`
	Status       Status `json:"status"`
	StatusTiming `json:",inline"`
	Ref          *Ref     `json:"ref,omitempty"`
	IPs          []IPNode `json:"ips"`
}

// IPNode is a single VIP and the Gateway that owns it.
type IPNode struct {
	IP            string `json:"ip"`
	Advertisement string `json:"advertisement"` // Advertised/Withdrawn/Pending*
	Status        Status `json:"status"`
	StatusTiming  `json:",inline"`
	Gateways      []GatewayNode `json:"gateways"`
	// Timer mirrors the running dampening timer of the owning Gateway, if any.
	Timer *Timer `json:"timer,omitempty"`
}

// GatewayNode is a Gateway API Gateway.
type GatewayNode struct {
	Name          string      `json:"name"`
	Namespace     string      `json:"namespace"`
	ClassName     string      `json:"className"`
	IPs           []string    `json:"ips"`
	FromMetalLB   bool        `json:"fromMetalLB"`
	Exempt        bool        `json:"exempt"`
	Managed       bool        `json:"managed"`
	Health        Status      `json:"health"`
	Advertisement string      `json:"advertisement"`
	Message       string      `json:"message,omitempty"`
	ProxyService  *ServiceRef `json:"proxyService,omitempty"`
	// ReplicasReady / ReplicasDesired report the Gateway's data-plane (proxy)
	// Deployment replica counts (summed across proxy Deployments).
	ReplicasReady   int32       `json:"replicasReady"`
	ReplicasDesired int32       `json:"replicasDesired"`
	Routes          []RouteNode `json:"routes"`
	Status          Status      `json:"status"`
	StatusTiming    `json:",inline"`
	Ref             *Ref `json:"ref,omitempty"`
	// Timer describes a running dampening timer (backoff/recovery), if any.
	Timer *Timer `json:"timer,omitempty"`
}

// Timer surfaces a running dampening timer to the UI.
//
//   - Kind "backoff": backends unhealthy; counting down to scaling the Gateway
//     proxy to zero (withdraw). Threshold = spec.withdrawAfter.
//   - Kind "recovery": backends healthy again; counting down to scaling the
//     Gateway proxy back up (re-advertise). Threshold = spec.readvertiseAfter.
type Timer struct {
	Kind         string `json:"kind"`
	ThresholdSec int64  `json:"thresholdSeconds"`
	ElapsedSec   int64  `json:"elapsedSeconds"`
	RemainingSec int64  `json:"remainingSeconds"`
}

// RouteNode is an xRoute attached to the Gateway.
type RouteNode struct {
	Kind         string   `json:"kind"` // HTTPRoute/GRPCRoute/TCPRoute/TLSRoute
	Name         string   `json:"name"`
	Namespace    string   `json:"namespace"`
	Hostnames    []string `json:"hostnames,omitempty"`
	Status       Status   `json:"status"`
	StatusTiming `json:",inline"`
	Ref          *Ref          `json:"ref,omitempty"`
	Services     []ServiceNode `json:"services"`
}

// ServiceRef is a lightweight reference (used for the proxy Service).
type ServiceRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Type      string `json:"type"`
}

// ServiceNode is a backend Service referenced by a route.
type ServiceNode struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Type         string `json:"type"`
	Port         string `json:"port,omitempty"`
	Status       Status `json:"status"`
	StatusTiming `json:",inline"`
	Ref          *Ref      `json:"ref,omitempty"`
	Pods         []PodNode `json:"pods"`
}

// PodNode is a backend workload Pod with its probe-derived health.
type PodNode struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Node      string `json:"node,omitempty"`
	Phase     string `json:"phase"`
	// Probed indicates the pod declares at least one health probe.
	Probed       bool   `json:"probed"`
	Ready        bool   `json:"ready"`
	Status       Status `json:"status"`
	StatusTiming `json:",inline"`
	Ref          *Ref   `json:"ref,omitempty"`
	Reason       string `json:"reason,omitempty"`
}
