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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GatewayHealthPolicySpec defines the desired state of GatewayHealthPolicy.
//
// GatewayHealthPolicy is a cluster-scoped, singleton configuration resource that
// controls how Beacon reconciles Gateway API Gateways against MetalLB BGP
// advertisements. Global timers, MetalLB integration settings, and exemptions
// are all defined here. Per-Gateway annotations may override selected fields.
type GatewayHealthPolicySpec struct {
	// WithdrawAfter is the duration a Gateway's backing workload must be
	// continuously unhealthy before its IP advertisement is withdrawn from
	// MetalLB. This acts as a dampening timer to avoid flapping.
	//
	// +kubebuilder:default="5s"
	// +optional
	WithdrawAfter metav1.Duration `json:"withdrawAfter,omitempty"`

	// ReadvertiseAfter is the duration a Gateway's backing workload must be
	// continuously healthy again before its IP advertisement is restored in
	// MetalLB. This dampening timer prevents route flap on intermittently
	// healthy workloads.
	//
	// +kubebuilder:default="30s"
	// +optional
	ReadvertiseAfter metav1.Duration `json:"readvertiseAfter,omitempty"`

	// ResyncInterval is the maximum period between full reconciliations of a
	// Gateway even when no watch event has fired. It bounds how quickly probe
	// state transitions are detected in the worst case.
	//
	// +kubebuilder:default="10s"
	// +optional
	ResyncInterval metav1.Duration `json:"resyncInterval,omitempty"`

	// MinHealthyBackendPercent is the minimum percentage of a Gateway's counted
	// backend services that must be healthy for the Gateway to remain
	// advertised. It is evaluated inclusively: the Gateway stays up while
	//
	//	(healthy backends / counted backends) * 100 >= MinHealthyBackendPercent
	//
	// and is withdrawn when it drops below. "Counted" backends are those with a
	// health signal — services with probed pods, or Skupper-linked services;
	// probe-less/exempt services are excluded from the ratio.
	//
	// The default is 100, meaning any single counted backend going down
	// withdraws the Gateway. Lower it to tolerate partial backend outages —
	// e.g. 50 keeps a 4-backend Gateway up until 3 are down (2 up = 50% is
	// still >= 50). May be overridden per-Gateway with the
	// "beacon.io/min-healthy-percent" annotation.
	//
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MinHealthyBackendPercent *int32 `json:"minHealthyBackendPercent,omitempty"`

	// MetalLB configures how Beacon interacts with MetalLB advertisements.
	// +optional
	MetalLB MetalLBConfig `json:"metallb,omitempty"`

	// Exemptions lists Gateways that should be excluded from health checking.
	// A Gateway is also exempt if it carries the
	// "beacon.io/exempt: \"true\"" annotation.
	// +optional
	Exemptions []GatewayReference `json:"exemptions,omitempty"`

	// GatewayClassNames optionally restricts Beacon to only manage Gateways
	// whose spec.gatewayClassName is in this list. When empty, all Gateways
	// are considered (subject to exemptions).
	// +optional
	GatewayClassNames []string `json:"gatewayClassNames,omitempty"`

	// Paused, when true, disables all withdrawal/re-advertisement actions.
	// Beacon continues to observe and report status but takes no mutating
	// action against MetalLB. Useful for maintenance windows.
	//
	// +kubebuilder:default=false
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// MetalLBConfig describes how Beacon locates MetalLB.
//
// Beacon does not create or modify MetalLB CRs. It only reads IPAddressPools
// (to determine which Gateway VIPs are sourced from MetalLB) and withdraws a
// VIP by draining the Gateway's proxy Deployment, which causes MetalLB to
// natively withdraw the route.
type MetalLBConfig struct {
	// Namespace is the namespace in which MetalLB CRs (IPAddressPool) live.
	//
	// +kubebuilder:default="metallb-system"
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// GatewayReference identifies a Gateway by namespace and name.
type GatewayReference struct {
	// Namespace of the Gateway.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name of the Gateway.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// GatewayHealthPolicyStatus defines the observed state of GatewayHealthPolicy.
type GatewayHealthPolicyStatus struct {
	// ObservedGeneration reflects the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ManagedGateways is the number of Gateways currently under management
	// (not exempt and matching the configured class filter).
	// +optional
	ManagedGateways int32 `json:"managedGateways,omitempty"`

	// AdvertisedIPs is the number of Gateway IPs currently advertised via
	// MetalLB by Beacon.
	// +optional
	AdvertisedIPs int32 `json:"advertisedIPs,omitempty"`

	// WithdrawnIPs is the number of Gateway IPs currently withdrawn by Beacon
	// due to failing health probes.
	// +optional
	WithdrawnIPs int32 `json:"withdrawnIPs,omitempty"`

	// Gateways holds per-Gateway health and advertisement observations.
	// +optional
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	Gateways []GatewayStatus `json:"gateways,omitempty"`

	// Conditions represent the latest available observations of the policy's
	// state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GatewayStatus captures the observed state of a single managed Gateway.
type GatewayStatus struct {
	// Namespace of the Gateway.
	Namespace string `json:"namespace"`

	// Name of the Gateway.
	Name string `json:"name"`

	// IPs are the load-balancer IP addresses inferred for this Gateway.
	// +optional
	IPs []string `json:"ips,omitempty"`

	// FromMetalLB indicates whether the IPs are sourced from a MetalLB
	// IPAddressPool.
	// +optional
	FromMetalLB bool `json:"fromMetalLB,omitempty"`

	// Health is the aggregate health of the Gateway's backing workloads.
	//   - "Healthy": all probed pods are ready.
	//   - "Unhealthy": at least one probed, non-exempt pod is failing.
	//   - "Exempt": Gateway (or all its pods) are exempt from probing.
	//   - "Unknown": health could not be determined.
	// +optional
	Health HealthState `json:"health,omitempty"`

	// Advertisement is the current advertisement state Beacon is enforcing.
	//   - "Advertised": IP(s) advertised in MetalLB.
	//   - "Withdrawn": IP(s) withdrawn from MetalLB.
	//   - "PendingWithdrawal": unhealthy, withdraw timer running.
	//   - "PendingReadvertise": healthy again, re-advertise timer running.
	// +optional
	Advertisement AdvertisementState `json:"advertisement,omitempty"`

	// LastTransitionTime is when Health last changed. Used to evaluate the
	// dampening timers.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// Message is a human-readable explanation of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Timer describes a running dampening timer, if any. Published by the
	// active (leader) controller so all replicas' dashboards render it
	// consistently.
	// +optional
	Timer *TimerStatus `json:"timer,omitempty"`
}

// TimerStatus describes a running dampening timer.
type TimerStatus struct {
	// Kind is "backoff" (backends unhealthy; counting down to withdraw) or
	// "recovery" (backends healthy again; counting down to re-advertise).
	// +optional
	Kind string `json:"kind,omitempty"`

	// ThresholdSeconds is the configured duration the condition must persist.
	// +optional
	ThresholdSeconds int64 `json:"thresholdSeconds,omitempty"`

	// ElapsedSeconds is how long the condition has persisted so far.
	// +optional
	ElapsedSeconds int64 `json:"elapsedSeconds,omitempty"`

	// RemainingSeconds is ThresholdSeconds-ElapsedSeconds (never negative).
	// +optional
	RemainingSeconds int64 `json:"remainingSeconds,omitempty"`
}

// HealthState enumerates aggregate Gateway health values.
type HealthState string

const (
	HealthHealthy   HealthState = "Healthy"
	HealthUnhealthy HealthState = "Unhealthy"
	HealthExempt    HealthState = "Exempt"
	HealthUnknown   HealthState = "Unknown"
)

// AdvertisementState enumerates MetalLB advertisement states Beacon enforces.
type AdvertisementState string

const (
	AdvertisementAdvertised         AdvertisementState = "Advertised"
	AdvertisementWithdrawn          AdvertisementState = "Withdrawn"
	AdvertisementPendingWithdrawal  AdvertisementState = "PendingWithdrawal"
	AdvertisementPendingReadvertise AdvertisementState = "PendingReadvertise"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ghp
// +kubebuilder:printcolumn:name="Managed",type="integer",JSONPath=".status.managedGateways"
// +kubebuilder:printcolumn:name="Advertised",type="integer",JSONPath=".status.advertisedIPs"
// +kubebuilder:printcolumn:name="Withdrawn",type="integer",JSONPath=".status.withdrawnIPs"
// +kubebuilder:printcolumn:name="Paused",type="boolean",JSONPath=".spec.paused"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// GatewayHealthPolicy is the Schema for the gatewayhealthpolicies API.
// It is a cluster-scoped singleton (conventionally named "cluster") that
// configures Beacon's reconciliation behavior.
type GatewayHealthPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayHealthPolicySpec   `json:"spec,omitempty"`
	Status GatewayHealthPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GatewayHealthPolicyList contains a list of GatewayHealthPolicy.
type GatewayHealthPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayHealthPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GatewayHealthPolicy{}, &GatewayHealthPolicyList{})
}
