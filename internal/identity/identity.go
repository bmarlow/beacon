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

// Package identity resolves a stable, cross-cluster identity for the cluster
// Beacon is running on. This is groundwork for multi-cluster fleets where a hub
// cluster aggregates status from many Beacon instances (e.g. one per cluster
// managed by Red Hat Advanced Cluster Management): the hub needs a way to
// correlate a spoke's self-reported identity with the hub's own view of that
// cluster (RHACM's ManagedCluster/ClusterClaim objects) without bespoke mapping
// configuration on either side.
package identity

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
)

// Resolve determines the identity of the cluster Beacon is running on.
//
// ID is chosen with the following precedence:
//
//  1. The OpenShift ClusterVersion UUID (config.openshift.io/v1 ClusterVersion
//     "version", spec.clusterID). This is the identifier OpenShift Cluster
//     Manager/telemetry use, and the one Red Hat Advanced Cluster Management
//     surfaces as the "id.openshift.io" ClusterClaim — so a hub can correlate
//     Beacon's report with RHACM's ManagedCluster with no extra configuration.
//  2. The kube-system Namespace UID, stable on any Kubernetes cluster
//     (OpenShift or not) and the identifier behind RHACM's "id.k8s.io" claim.
//     Used when the ClusterVersion resource is unavailable.
//
// Name is the human-readable cluster name: nameOverride (typically
// spec.clusterName) if non-empty, else the OpenShift Infrastructure
// infrastructureName, else empty.
//
// All lookups tolerate the resource being absent (non-OpenShift clusters, or a
// test environment without these CRDs/objects) and simply leave the
// corresponding field empty rather than failing the caller.
func Resolve(ctx context.Context, c client.Client, nameOverride string) beaconv1alpha1.ClusterIdentity {
	out := beaconv1alpha1.ClusterIdentity{Source: beaconv1alpha1.ClusterIdentitySourceManual}

	if id, ok := clusterVersionID(ctx, c); ok {
		out.ID = id
		out.Source = beaconv1alpha1.ClusterIdentitySourceOpenShift
	} else if uid, ok := kubeSystemUID(ctx, c); ok {
		out.ID = uid
		out.Source = beaconv1alpha1.ClusterIdentitySourceKubeSystem
	}

	if nameOverride != "" {
		out.Name = nameOverride
	} else if name, ok := infrastructureName(ctx, c); ok {
		out.Name = name
	}

	return out
}

// clusterVersionID reads the OpenShift ClusterVersion UUID.
func clusterVersionID(ctx context.Context, c client.Client) (string, bool) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "ClusterVersion"})
	if err := c.Get(ctx, types.NamespacedName{Name: "version"}, obj); err != nil {
		return "", false
	}
	id, found, _ := unstructured.NestedString(obj.Object, "spec", "clusterID")
	return id, found && id != ""
}

// infrastructureName reads the OpenShift Infrastructure infrastructureName.
func infrastructureName(ctx context.Context, c client.Client) (string, bool) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure"})
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, obj); err != nil {
		return "", false
	}
	name, found, _ := unstructured.NestedString(obj.Object, "status", "infrastructureName")
	return name, found && name != ""
}

// Label returns a single best-effort display value for a ClusterIdentity,
// suitable for use as a metric label or log field: Name if set, else ID, else
// "" (nothing could be determined). Prefer this over reading Name/ID directly
// so all call sites apply the same fallback consistently.
func Label(ci beaconv1alpha1.ClusterIdentity) string {
	if ci.Name != "" {
		return ci.Name
	}
	return ci.ID
}

// kubeSystemUID reads the kube-system Namespace UID.
func kubeSystemUID(ctx context.Context, c client.Client) (string, bool) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: "kube-system"}, ns); err != nil {
		return "", false
	}
	if ns.UID == "" {
		return "", false
	}
	return string(ns.UID), true
}
