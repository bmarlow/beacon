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

// Package metallb provides a minimal typed client surface for the subset of
// MetalLB CRDs Beacon needs to read and manipulate:
//
//   - IPAddressPool (metallb.io/v1beta1): to determine whether a Gateway IP is
//     sourced from MetalLB, by matching the IP against the pool's CIDRs/ranges.
//   - BGPAdvertisement (metallb.io/v1beta1): the advertisement CR Beacon
//     manages to withdraw/re-advertise routes without touching BGP sessions.
//
// We intentionally define our own lightweight types (rather than importing the
// full MetalLB Go module) to keep Beacon's dependency surface small and stable
// across MetalLB releases. Only the fields Beacon reads or writes are modeled.
package metallb

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the MetalLB API group.
	GroupName = "metallb.io"
	// Version is the MetalLB API version Beacon targets.
	Version = "v1beta1"
)

var (
	// GroupVersion is the MetalLB group/version.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

	// IPAddressPoolGVK identifies the IPAddressPool kind.
	IPAddressPoolGVK = GroupVersion.WithKind("IPAddressPool")
	// IPAddressPoolListGVK identifies the IPAddressPoolList kind.
	IPAddressPoolListGVK = GroupVersion.WithKind("IPAddressPoolList")
	// BGPAdvertisementGVK identifies the BGPAdvertisement kind.
	BGPAdvertisementGVK = GroupVersion.WithKind("BGPAdvertisement")
	// BGPAdvertisementListGVK identifies the BGPAdvertisementList kind.
	BGPAdvertisementListGVK = GroupVersion.WithKind("BGPAdvertisementList")
)

// +kubebuilder:object:generate=false

// IPAddressPool is a minimal representation of metallb.io/v1beta1 IPAddressPool.
type IPAddressPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IPAddressPoolSpec `json:"spec,omitempty"`
}

// IPAddressPoolSpec is the subset of the pool spec Beacon reads.
type IPAddressPoolSpec struct {
	// Addresses is the list of CIDRs / ranges the pool manages, e.g.
	// "192.168.10.0/24" or "192.168.9.1-192.168.9.5".
	Addresses []string `json:"addresses"`

	// AutoAssign indicates whether MetalLB may auto-assign from this pool.
	// +optional
	AutoAssign *bool `json:"autoAssign,omitempty"`

	// AvoidBuggyIPs is passed through; unused by Beacon but modeled for round-trip.
	// +optional
	AvoidBuggyIPs bool `json:"avoidBuggyIPs,omitempty"`
}

// +kubebuilder:object:generate=false

// IPAddressPoolList is a list of IPAddressPool.
type IPAddressPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAddressPool `json:"items"`
}

// +kubebuilder:object:generate=false

// BGPAdvertisement is a minimal representation of metallb.io/v1beta1
// BGPAdvertisement. Beacon manages one such CR to control which Services'
// IPs are advertised via BGP.
type BGPAdvertisement struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BGPAdvertisementSpec `json:"spec,omitempty"`
}

// BGPAdvertisementSpec is the subset of the advertisement spec Beacon writes.
type BGPAdvertisementSpec struct {
	// IPAddressPools restricts the advertisement to the named pools.
	// +optional
	IPAddressPools []string `json:"ipAddressPools,omitempty"`

	// IPAddressPoolSelectors selects pools by label.
	// +optional
	IPAddressPoolSelectors []metav1.LabelSelector `json:"ipAddressPoolSelectors,omitempty"`

	// ServiceSelectors restricts the advertisement to Services matching these
	// label selectors. Beacon uses this together with a managed Service label
	// to advertise/withdraw individual routes without touching BGP sessions.
	// +optional
	ServiceSelectors []metav1.LabelSelector `json:"serviceSelectors,omitempty"`

	// AggregationLength controls IPv4 route aggregation (default /32).
	// +optional
	AggregationLength *int32 `json:"aggregationLength,omitempty"`

	// Communities to attach to advertised routes.
	// +optional
	Communities []string `json:"communities,omitempty"`
}

// +kubebuilder:object:generate=false

// BGPAdvertisementList is a list of BGPAdvertisement.
type BGPAdvertisementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPAdvertisement `json:"items"`
}
