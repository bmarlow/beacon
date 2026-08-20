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

// Package advertiser withdraws or restores a Gateway's MetalLB-advertised VIP by
// draining the Gateway's data-plane (proxy) Service of ready endpoints, which is
// MetalLB's native, BGP-safe withdrawal trigger.
//
// # Why this mechanism (and not MetalLB CR manipulation)
//
// MetalLB advertises a Service's LoadBalancer IP over a persistent BGP session
// as long as the Service has at least one ready endpoint. It is deliberately
// designed to KEEP advertising: removing/altering IPAddressPools or
// BGPAdvertisements that back an assigned IP is treated as a "stale" config and
// MetalLB retains the last-good advertisement to preserve connectivity. There is
// also no per-Service selector on BGPAdvertisement, and pools may not overlap.
// Consequently, an external controller cannot reliably yank one advertised /32
// by editing MetalLB CRs.
//
// What MetalLB DOES support, natively and gracefully, is withdrawing a Service's
// route when that Service has zero ready endpoints. It sends a single BGP
// withdraw for that prefix over the EXISTING session — the adjacency never
// flaps, and other prefixes are unaffected. (Verified on-cluster with
// externalTrafficPolicy: Cluster gateways.)
//
// # How Beacon uses it
//
// The Gateway's proxy is implemented by a Deployment (e.g. the Istio-managed
// ingress gateway) selected by the label
// gateway.networking.k8s.io/gateway-name=<gateway>. To WITHDRAW, Beacon scales
// that Deployment to 0 replicas (recording the previous replica count in an
// annotation). The proxy Service then has no ready endpoints and MetalLB
// withdraws the VIP. To RE-ADVERTISE, Beacon restores the saved replica count;
// the proxy comes back, endpoints become ready, and MetalLB re-announces.
//
// Note this is whole-Gateway granularity: all VIPs/routes fronted by the Gateway
// are withdrawn together. For the common one-Gateway-per-VIP topology this is
// exactly the desired behavior.
package advertiser

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	beaconv1alpha1 "github.com/beacon-operator/beacon/api/v1alpha1"
)

const (
	// SavedReplicasAnnotation stores the replica count Beacon scaled down from,
	// so it can be restored on re-advertisement.
	SavedReplicasAnnotation = "beacon.io/saved-replicas"
	// WithdrawnAnnotation marks a proxy Deployment as currently withdrawn by
	// Beacon.
	WithdrawnAnnotation = "beacon.io/withdrawn"

	// ManagedByLabel / ManagedByValue mark resources Beacon manages.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "beacon"

	// gatewayNameLabel correlates proxy workloads/Services to their Gateway.
	gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"
)

// Advertiser withdraws/restores a Gateway VIP via proxy endpoint draining.
type Advertiser struct {
	Client client.Client
	Config beaconv1alpha1.MetalLBConfig
}

// Withdraw scales the Gateway's proxy Deployment(s) to 0 so MetalLB withdraws
// the VIP. The prior replica count is saved for later restoration. Idempotent.
func (a *Advertiser) Withdraw(ctx context.Context, gatewayNamespace, gatewayName string) error {
	deploys, err := a.proxyDeployments(ctx, gatewayNamespace, gatewayName)
	if err != nil {
		return err
	}
	for i := range deploys {
		d := &deploys[i]
		current := int32(1)
		if d.Spec.Replicas != nil {
			current = *d.Spec.Replicas
		}
		if current == 0 {
			continue // already withdrawn/scaled to zero
		}
		patch := client.MergeFrom(d.DeepCopy())
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[SavedReplicasAnnotation] = strconv.Itoa(int(current))
		d.Annotations[WithdrawnAnnotation] = "true"
		zero := int32(0)
		d.Spec.Replicas = &zero
		if err := a.Client.Patch(ctx, d, patch); err != nil {
			return fmt.Errorf("scaling proxy deployment %s/%s to 0: %w", d.Namespace, d.Name, err)
		}
	}
	return nil
}

// Advertise restores the Gateway's proxy Deployment(s) to their saved replica
// count (default 1) so MetalLB re-advertises the VIP. Idempotent.
func (a *Advertiser) Advertise(ctx context.Context, gatewayNamespace, gatewayName string) error {
	deploys, err := a.proxyDeployments(ctx, gatewayNamespace, gatewayName)
	if err != nil {
		return err
	}
	for i := range deploys {
		d := &deploys[i]
		// Only act if Beacon previously withdrew it, or it is currently at 0.
		withdrawn := d.Annotations[WithdrawnAnnotation] == "true"
		atZero := d.Spec.Replicas != nil && *d.Spec.Replicas == 0
		if !withdrawn && !atZero {
			continue // already advertised; leave user-managed scale alone
		}
		restore := int32(1)
		if s, ok := d.Annotations[SavedReplicasAnnotation]; ok {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				restore = int32(n)
			}
		}
		patch := client.MergeFrom(d.DeepCopy())
		d.Spec.Replicas = &restore
		delete(d.Annotations, WithdrawnAnnotation)
		delete(d.Annotations, SavedReplicasAnnotation)
		if err := a.Client.Patch(ctx, d, patch); err != nil {
			return fmt.Errorf("restoring proxy deployment %s/%s to %d: %w", d.Namespace, d.Name, restore, err)
		}
	}
	return nil
}

// IsWithdrawn reports whether all proxy Deployments for the Gateway are scaled
// to zero (the ground-truth "withdrawn" signal used by the topology UI).
func (a *Advertiser) IsWithdrawn(ctx context.Context, gatewayNamespace, gatewayName string) (bool, error) {
	deploys, err := a.proxyDeployments(ctx, gatewayNamespace, gatewayName)
	if err != nil {
		return false, err
	}
	if len(deploys) == 0 {
		return false, nil
	}
	for i := range deploys {
		r := int32(1)
		if deploys[i].Spec.Replicas != nil {
			r = *deploys[i].Spec.Replicas
		}
		if r != 0 {
			return false, nil
		}
	}
	return true, nil
}

// proxyDeployments returns the Deployment(s) implementing the Gateway's proxy,
// located in the Gateway's namespace by the gateway-name label.
func (a *Advertiser) proxyDeployments(ctx context.Context, gatewayNamespace, gatewayName string) ([]appsv1.Deployment, error) {
	list := &appsv1.DeploymentList{}
	if err := a.Client.List(ctx, list,
		client.InNamespace(gatewayNamespace),
		client.MatchingLabels{gatewayNameLabel: gatewayName},
	); err != nil {
		return nil, fmt.Errorf("listing proxy deployments for gateway %s/%s: %w", gatewayNamespace, gatewayName, err)
	}
	return list.Items, nil
}

// ProxyServiceKeys is retained for callers that need the proxy Service refs.
func ProxyServiceKeys(svcs []corev1.Service) []types.NamespacedName {
	keys := make([]types.NamespacedName, 0, len(svcs))
	for i := range svcs {
		keys = append(keys, types.NamespacedName{Namespace: svcs[i].Namespace, Name: svcs[i].Name})
	}
	return keys
}
