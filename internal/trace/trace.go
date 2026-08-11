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

// Package trace resolves a Gateway API Gateway down to the concrete Kubernetes
// resources whose health determines whether the Gateway's VIP should be
// advertised.
//
// There are two distinct pod populations in a Gateway API deployment:
//
//  1. The Gateway *data-plane / proxy* pods (envoy, istio-ingressgateway,
//     nginx, ...). These are what the Gateway's own LoadBalancer Service
//     selects and what MetalLB's endpoints point at. MetalLB already reacts to
//     these natively (proxy down -> endpoint removed -> route withdrawn).
//
//  2. The *backend / workload* pods that xRoutes (HTTPRoute, GRPCRoute,
//     TCPRoute, TLSRoute) attached to the Gateway forward traffic to. These are
//     the pods whose application health actually matters for "should this VIP
//     be advertised".
//
// Beacon monitors population (2), the BACKEND pods. It uses the Gateway's
// LoadBalancer Service ONLY to infer the VIP address, never to source the pods
// whose probes are evaluated.
//
// Resolution:
//
//	Gateway
//	  ├─ (VIP)  Gateway.status.addresses  ||  LoadBalancer Service ingress IP
//	  └─ (pods) xRoutes whose parentRefs -> this Gateway
//	              └─ rule.backendRefs -> backend Service(s)
//	                    └─ EndpointSlices -> backend workload Pods
package trace

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
	"github.com/bmarlow/beacon/internal/health"
	"github.com/bmarlow/beacon/internal/policy"
	"github.com/bmarlow/beacon/internal/skupper"
)

// GatewayResolution is the outcome of tracing a Gateway.
type GatewayResolution struct {
	// IPs are the load-balancer IP addresses advertised for the Gateway.
	IPs []string

	// ProxyServices are the Gateway's own LoadBalancer Service(s) (the proxy
	// data plane). Beacon uses these to toggle MetalLB advertisement labels,
	// NOT to evaluate health.
	ProxyServices []corev1.Service

	// BackendServices are the Services referenced by xRoutes attached to this
	// Gateway. Their pods are the ones Beacon health-checks.
	BackendServices []corev1.Service

	// Pods are the deduplicated BACKEND workload Pods whose probes are
	// evaluated for health.
	Pods []corev1.Pod

	// RemoteBackends are Skupper-linked backends (the real workload lives on a
	// remote cluster). Their health comes from the Skupper Listener status, not
	// local pods, and is folded into the Gateway's aggregate health.
	RemoteBackends []RemoteBackend

	// ServiceHealths is the per-backend-Service health used by the
	// minimum-healthy-backend-percentage decision (one entry per backend
	// Service, local or Skupper).
	ServiceHealths []health.ServiceHealth
}

// RemoteBackend is a Skupper Listener-backed backend Service.
type RemoteBackend struct {
	Namespace string
	Name      string
	Listener  string
	Ready     bool
	Reason    string
}

// Services returns the Services on which Beacon toggles the MetalLB
// advertisement label (the proxy/VIP Services).
func (r *GatewayResolution) Services() []corev1.Service {
	return r.ProxyServices
}

// Resolver traces Gateways using a controller-runtime client.
type Resolver struct {
	Client client.Client
}

// gatewayServiceLabel is the well-known label Gateway implementations set on
// the proxy Service they create so it can be correlated back to the Gateway.
const gatewayServiceLabel = "gateway.networking.k8s.io/gateway-name"

// Resolve traces a Gateway to its IPs, its proxy Service(s) (for advertisement
// control), and its BACKEND workload Pods (for health evaluation).
// Resolve traces a Gateway. gatewayPodPercent is the Gateway-level
// minimum-healthy-pod threshold and gatewayZeroPolicy the Gateway-level
// scaled-to-zero policy; each backend Service may override both via annotations.
func (r *Resolver) Resolve(ctx context.Context, gw *gwapiv1.Gateway, gatewayPodPercent int, gatewayZeroPolicy beaconv1alpha1.ZeroReplicasPolicy) (*GatewayResolution, error) {
	res := &GatewayResolution{}
	gatewayCritical := policy.GatewayCritical(gw)

	// 1. Infer the VIP(s) from Gateway status, then fall back to the proxy
	//    Service's ingress IPs.
	res.IPs = extractGatewayIPs(gw)

	proxySvcs, err := r.findProxyServices(ctx, gw)
	if err != nil {
		return nil, fmt.Errorf("finding proxy services for gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	res.ProxyServices = proxySvcs

	if len(res.IPs) == 0 {
		for i := range proxySvcs {
			for _, ing := range proxySvcs[i].Status.LoadBalancer.Ingress {
				if ing.IP != "" {
					res.IPs = appendUnique(res.IPs, ing.IP)
				}
			}
		}
	}

	// 2. Discover the BACKEND Services referenced by xRoutes attached to this
	//    Gateway.
	backendSvcs, routeCrit, err := r.findBackendServices(ctx, gw)
	if err != nil {
		return nil, fmt.Errorf("finding backend services for gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	res.BackendServices = backendSvcs

	// 3. Trace each backend Service to its Pods (the pods we health-check).
	//    Skupper-backed Services are evaluated via their Listener instead.
	//    Also compute per-Service health for the minimum-healthy-percentage rule.
	seen := map[types.NamespacedName]struct{}{}
	critical := func(svc *corev1.Service) bool {
		rc := routeCrit[types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}]
		return policy.BackendCritical(svc.Annotations, rc.Any, rc.Present, gatewayCritical)
	}
	for i := range backendSvcs {
		svc := &backendSvcs[i]
		if lname, ok := skupper.ServiceListenerName(svc.Labels); ok {
			sh := skupper.EvaluateListener(ctx, r.Client, svc.Namespace, lname)
			res.RemoteBackends = append(res.RemoteBackends, RemoteBackend{
				Namespace: svc.Namespace, Name: svc.Name,
				Listener: lname, Ready: sh.Ready, Reason: sh.Reason,
			})
			// A Skupper backend always counts; healthy iff the link is ready.
			res.ServiceHealths = append(res.ServiceHealths, health.ServiceHealth{
				Namespace: svc.Namespace, Name: svc.Name, Counted: true, Healthy: sh.Ready,
				Critical: critical(svc),
			})
			continue
		}
		pods, err := r.podsForService(ctx, &backendSvcs[i])
		if err != nil {
			return nil, fmt.Errorf("finding pods for backend service %s/%s: %w",
				backendSvcs[i].Namespace, backendSvcs[i].Name, err)
		}
		// Per-service health: a Service is "up" while the percentage of its
		// probed pods that are Ready meets the per-Service pod threshold
		// (Service annotation overrides the Gateway-level value; default 1 =
		// any Ready pod). A Service with a selector but zero pods (scaled to
		// zero) is counted as failing unless the effective zeroReplicasPolicy is
		// Exempt. Probe-less / selector-less Services are not counted.
		podPct := policy.ServiceMinHealthyPodPercent(svc.Annotations, int32(gatewayPodPercent))
		zeroPol := policy.ServiceZeroReplicasPolicy(svc.Annotations, gatewayZeroPolicy)
		hasSelector := len(svc.Spec.Selector) > 0
		counted, healthy, _, _ := health.EvaluateService(pods, int(podPct), hasSelector,
			zeroPol == beaconv1alpha1.ZeroReplicasUnhealthy)
		res.ServiceHealths = append(res.ServiceHealths, health.ServiceHealth{
			Namespace: svc.Namespace, Name: svc.Name,
			Counted:  counted,
			Healthy:  healthy,
			Critical: critical(svc),
		})
		for j := range pods {
			key := types.NamespacedName{Namespace: pods[j].Namespace, Name: pods[j].Name}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			res.Pods = append(res.Pods, pods[j])
		}
	}

	return res, nil
}

// extractGatewayIPs pulls IP-type addresses from Gateway.status.addresses.
func extractGatewayIPs(gw *gwapiv1.Gateway) []string {
	var ips []string
	for _, addr := range gw.Status.Addresses {
		if addr.Type == nil || *addr.Type == gwapiv1.IPAddressType {
			if net.ParseIP(addr.Value) != nil {
				ips = appendUnique(ips, addr.Value)
			}
		}
	}
	return ips
}

// findProxyServices locates the Gateway's own LoadBalancer Service(s) (the
// proxy data plane), used for VIP inference and advertisement control.
func (r *Resolver) findProxyServices(ctx context.Context, gw *gwapiv1.Gateway) ([]corev1.Service, error) {
	var out []corev1.Service

	labeled := &corev1.ServiceList{}
	if err := r.Client.List(ctx, labeled,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{gatewayServiceLabel: gw.Name},
	); err != nil {
		return nil, err
	}
	for i := range labeled.Items {
		if labeled.Items[i].Spec.Type == corev1.ServiceTypeLoadBalancer {
			out = append(out, labeled.Items[i])
		}
	}
	if len(out) > 0 {
		return out, nil
	}

	// Fallback: match by ingress IP across LoadBalancer Services in the ns.
	gwIPs := map[string]struct{}{}
	for _, ip := range extractGatewayIPs(gw) {
		gwIPs[ip] = struct{}{}
	}
	if len(gwIPs) == 0 {
		return out, nil
	}
	all := &corev1.ServiceList{}
	if err := r.Client.List(ctx, all, client.InNamespace(gw.Namespace)); err != nil {
		return nil, err
	}
	for i := range all.Items {
		svc := &all.Items[i]
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if _, ok := gwIPs[ing.IP]; ok {
				out = append(out, *svc)
				break
			}
		}
	}
	return out, nil
}

// backendRef is a normalized backend Service reference collected from any xRoute.
type backendRef struct {
	namespace string
	name      string
}

// routeCritInfo accumulates, for a backend ref, whether any attaching route is
// marked critical and whether at least one such route carries the annotation.
// Used to resolve route-level criticality (precedence: Service > Route >
// Gateway).
type routeCritInfo struct {
	// present is true when at least one route referencing this backend carries
	// the beacon.io/critical annotation.
	present bool
	// any is true when at least one such route sets it to a truthy value.
	any bool
}

// BackendRouteCritical resolves whether a Service's attaching route(s) mark it
// critical. Callers combine this with the Service/Gateway settings via
// policy.BackendCritical.
type BackendRouteCritical struct {
	Present bool
	Any     bool
}

// findBackendServices collects the backend Services referenced by all xRoutes
// (HTTPRoute, GRPCRoute, TCPRoute, TLSRoute) whose parentRefs attach them to
// this Gateway. Only backends of kind Service (core group) are considered. It
// also returns, per resolved Service, the accumulated route-level critical
// annotation info (a Service can be referenced by multiple routes).
func (r *Resolver) findBackendServices(ctx context.Context, gw *gwapiv1.Gateway) ([]corev1.Service, map[types.NamespacedName]BackendRouteCritical, error) {
	refs := map[backendRef]*routeCritInfo{}

	// HTTPRoutes (gateway.networking.k8s.io/v1)
	httpRoutes := &gwapiv1.HTTPRouteList{}
	if err := r.listRoutes(ctx, httpRoutes); err != nil {
		return nil, nil, err
	}
	for i := range httpRoutes.Items {
		rt := &httpRoutes.Items[i]
		if !routeAttachedToGateway(rt.Spec.ParentRefs, rt.Namespace, gw) {
			continue
		}
		crit, present := policy.RouteCritical(rt.Annotations)
		for _, rule := range rt.Spec.Rules {
			for _, b := range rule.BackendRefs {
				addServiceRef(refs, b.BackendObjectReference, rt.Namespace, crit, present)
			}
		}
	}

	// GRPCRoutes (gateway.networking.k8s.io/v1)
	grpcRoutes := &gwapiv1.GRPCRouteList{}
	if err := r.listRoutes(ctx, grpcRoutes); err != nil {
		return nil, nil, err
	}
	for i := range grpcRoutes.Items {
		rt := &grpcRoutes.Items[i]
		if !routeAttachedToGateway(rt.Spec.ParentRefs, rt.Namespace, gw) {
			continue
		}
		crit, present := policy.RouteCritical(rt.Annotations)
		for _, rule := range rt.Spec.Rules {
			for _, b := range rule.BackendRefs {
				addServiceRef(refs, b.BackendObjectReference, rt.Namespace, crit, present)
			}
		}
	}

	// TCPRoutes (gateway.networking.k8s.io/v1alpha2) - optional CRD.
	tcpRoutes := &gwapiv1alpha2.TCPRouteList{}
	if err := r.listRoutes(ctx, tcpRoutes); err != nil {
		return nil, nil, err
	}
	for i := range tcpRoutes.Items {
		rt := &tcpRoutes.Items[i]
		if !routeAttachedToGateway(rt.Spec.ParentRefs, rt.Namespace, gw) {
			continue
		}
		crit, present := policy.RouteCritical(rt.Annotations)
		for _, rule := range rt.Spec.Rules {
			for _, b := range rule.BackendRefs {
				addServiceRef(refs, b.BackendObjectReference, rt.Namespace, crit, present)
			}
		}
	}

	// TLSRoutes (gateway.networking.k8s.io/v1alpha2) - optional CRD.
	tlsRoutes := &gwapiv1alpha2.TLSRouteList{}
	if err := r.listRoutes(ctx, tlsRoutes); err != nil {
		return nil, nil, err
	}
	for i := range tlsRoutes.Items {
		rt := &tlsRoutes.Items[i]
		if !routeAttachedToGateway(rt.Spec.ParentRefs, rt.Namespace, gw) {
			continue
		}
		crit, present := policy.RouteCritical(rt.Annotations)
		for _, rule := range rt.Spec.Rules {
			for _, b := range rule.BackendRefs {
				addServiceRef(refs, b.BackendObjectReference, rt.Namespace, crit, present)
			}
		}
	}

	// Resolve the collected refs into Service objects.
	var out []corev1.Service
	critByService := map[types.NamespacedName]BackendRouteCritical{}
	for ref, info := range refs {
		key := types.NamespacedName{Namespace: ref.namespace, Name: ref.name}
		svc := &corev1.Service{}
		if err := r.Client.Get(ctx, key, svc); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, nil, err
			}
			continue // backend Service missing; skip
		}
		out = append(out, *svc)
		critByService[key] = BackendRouteCritical{Present: info.present, Any: info.any}
	}
	return out, critByService, nil
}

// listRoutes lists a route kind, tolerating clusters where that route CRD is
// not installed (or the type is not registered in the scheme). Route kinds like
// TCPRoute/TLSRoute are optional/experimental and frequently absent; their
// absence must not fail reconciliation.
func (r *Resolver) listRoutes(ctx context.Context, list client.ObjectList) error {
	err := r.Client.List(ctx, list)
	if err == nil {
		return nil
	}
	if apimeta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		return nil
	}
	return err
}

// addServiceRef records a backend reference if it points at a core Service,
// merging the referencing route's critical annotation into the ref's accumulated
// route-critical info.
func addServiceRef(refs map[backendRef]*routeCritInfo, ref gwapiv1.BackendObjectReference, routeNamespace string, routeCritical, routeCriticalPresent bool) {
	// Kind defaults to "Service"; group defaults to core ("").
	if ref.Kind != nil && string(*ref.Kind) != "Service" {
		return
	}
	if ref.Group != nil && string(*ref.Group) != "" {
		return
	}
	ns := routeNamespace
	if ref.Namespace != nil && string(*ref.Namespace) != "" {
		ns = string(*ref.Namespace)
	}
	key := backendRef{namespace: ns, name: string(ref.Name)}
	info := refs[key]
	if info == nil {
		info = &routeCritInfo{}
		refs[key] = info
	}
	if routeCriticalPresent {
		info.present = true
		if routeCritical {
			info.any = true
		}
	}
}

// routeAttachedToGateway reports whether any of the route's parentRefs targets
// the given Gateway. A parentRef targets a Gateway when its (defaulted) group
// is gateway.networking.k8s.io, its (defaulted) kind is Gateway, and its
// name (and resolved namespace) match.
func routeAttachedToGateway(parentRefs []gwapiv1.ParentReference, routeNamespace string, gw *gwapiv1.Gateway) bool {
	for _, p := range parentRefs {
		// Group defaults to gateway.networking.k8s.io.
		if p.Group != nil && string(*p.Group) != gwapiv1.GroupName {
			continue
		}
		// Kind defaults to Gateway.
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		if string(p.Name) != gw.Name {
			continue
		}
		ns := routeNamespace
		if p.Namespace != nil && string(*p.Namespace) != "" {
			ns = string(*p.Namespace)
		}
		if ns == gw.Namespace {
			return true
		}
	}
	return false
}

// podsForService returns the Pods backing a Service using EndpointSlices.
func (r *Resolver) podsForService(ctx context.Context, svc *corev1.Service) ([]corev1.Pod, error) {
	sliceList := &discoveryv1.EndpointSliceList{}
	if err := r.Client.List(ctx, sliceList,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name},
	); err != nil {
		return nil, err
	}

	podRefs := map[types.NamespacedName]struct{}{}
	for i := range sliceList.Items {
		for _, ep := range sliceList.Items[i].Endpoints {
			if ep.TargetRef == nil || ep.TargetRef.Kind != "Pod" {
				continue
			}
			ns := ep.TargetRef.Namespace
			if ns == "" {
				ns = svc.Namespace
			}
			podRefs[types.NamespacedName{Namespace: ns, Name: ep.TargetRef.Name}] = struct{}{}
		}
	}

	// Fallback to selector-based pod listing if no EndpointSlices exist yet.
	if len(podRefs) == 0 && len(svc.Spec.Selector) > 0 {
		podList := &corev1.PodList{}
		if err := r.Client.List(ctx, podList,
			client.InNamespace(svc.Namespace),
			client.MatchingLabels(svc.Spec.Selector),
		); err != nil {
			return nil, err
		}
		return podList.Items, nil
	}

	var pods []corev1.Pod
	for ref := range podRefs {
		pod := &corev1.Pod{}
		if err := r.Client.Get(ctx, ref, pod); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			continue
		}
		pods = append(pods, *pod)
	}
	return pods, nil
}

func appendUnique(s []string, v string) []string {
	for _, existing := range s {
		if existing == v {
			return s
		}
	}
	return append(s, v)
}
