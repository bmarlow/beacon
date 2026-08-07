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

package topology

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	"github.com/bmarlow/beacon/internal/metallb"
	"github.com/bmarlow/beacon/internal/policy"
	"github.com/bmarlow/beacon/internal/state"
)

const gatewayServiceLabel = "gateway.networking.k8s.io/gateway-name"

// Builder assembles the topology Graph from cluster state plus live controller
// state (advertisement decisions) from the shared store.
type Builder struct {
	Client     client.Client
	States     *state.Store
	PolicyName string
}

// routeInfo is an intermediate normalized route with its backend service refs.
type routeInfo struct {
	kind      string
	name      string
	namespace string
	hostnames []string
	backends  []types.NamespacedName
}

// Build produces a full topology Graph.
func (b *Builder) Build(ctx context.Context) (*Graph, error) {
	g := &Graph{GeneratedAt: time.Now()}

	// Load policy (config): timers, exemptions, metallb namespace.
	spec := b.loadPolicySpec(ctx)
	g.MetalLBNamespace = spec.MetalLB.Namespace

	// Load MetalLB pools.
	poolList := &metallb.IPAddressPoolList{}
	if err := b.Client.List(ctx, poolList, client.InNamespace(spec.MetalLB.Namespace)); err != nil {
		if !ignorableListErr(err) {
			return nil, fmt.Errorf("listing IPAddressPools: %w", err)
		}
	}

	// Load all Gateways.
	gwList := &gwapiv1.GatewayList{}
	if err := b.Client.List(ctx, gwList); err != nil {
		return nil, fmt.Errorf("listing Gateways: %w", err)
	}

	// Load all routes once, indexed by target Gateway.
	routesByGateway, err := b.indexRoutes(ctx)
	if err != nil {
		return nil, err
	}

	// Build a GatewayNode for each Gateway.
	var gatewayNodes []GatewayNode
	snaps := map[types.NamespacedName]state.GatewaySnapshot{}
	if b.States != nil {
		snaps = b.States.Snapshot()
	}

	for i := range gwList.Items {
		gw := &gwList.Items[i]
		node := b.buildGatewayNode(ctx, gw, spec, routesByGateway, snaps)
		gatewayNodes = append(gatewayNodes, node)
	}

	// Group Gateways under the pool that owns their IP.
	poolNodesByName := map[string]*PoolNode{}
	var poolOrder []string
	for i := range poolList.Items {
		p := &poolList.Items[i]
		pn := &PoolNode{
			Name:       p.Name,
			Namespace:  p.Namespace,
			Addresses:  append([]string(nil), p.Spec.Addresses...),
			AutoAssign: p.Spec.AutoAssign,
		}
		poolNodesByName[p.Name] = pn
		poolOrder = append(poolOrder, p.Name)
	}

	// IP -> IPNode within a pool, keyed by "pool/ip".
	ipNodeIndex := map[string]*IPNode{}

	for gi := range gatewayNodes {
		gn := &gatewayNodes[gi]
		placed := false
		for _, ip := range gn.IPs {
			pool := metallb.PoolForIP(poolList.Items, ip)
			if pool == nil {
				continue
			}
			pn := poolNodesByName[pool.Name]
			key := pool.Name + "/" + ip
			ipn := ipNodeIndex[key]
			if ipn == nil {
				ipn = &IPNode{IP: ip}
				ipNodeIndex[key] = ipn
				pn.IPs = append(pn.IPs, IPNode{}) // placeholder; filled after
			}
			ipn.Advertisement = gn.Advertisement
			ipn.Gateways = append(ipn.Gateways, *gn)
			placed = true
		}
		if !placed {
			g.UnpooledGateways = append(g.UnpooledGateways, *gn)
		}
	}

	// Rebuild pool IP lists from the index (the placeholders above kept order
	// stable but we now assemble the real values).
	for name, pn := range poolNodesByName {
		pn.IPs = pn.IPs[:0]
		for key, ipn := range ipNodeIndex {
			if strings.HasPrefix(key, name+"/") {
				ipn.Status = statusForAdvertisement(ipn.Advertisement, worstGatewayStatus(ipn.Gateways))
				pn.IPs = append(pn.IPs, *ipn)
			}
		}
		sort.Slice(pn.IPs, func(i, j int) bool { return ipLess(pn.IPs[i].IP, pn.IPs[j].IP) })
		pn.Status = worstIPStatus(pn.IPs)
	}

	// Emit pools in stable order, skipping pools with no Gateways (still show
	// them so operators can see empty pools).
	for _, name := range poolOrder {
		g.Pools = append(g.Pools, *poolNodesByName[name])
	}

	g.Summary = summarize(g)
	return g, nil
}

// buildGatewayNode assembles a single Gateway subtree.
func (b *Builder) buildGatewayNode(
	ctx context.Context,
	gw *gwapiv1.Gateway,
	spec *beaconv1alpha1.GatewayHealthPolicySpec,
	routesByGateway map[types.NamespacedName][]routeInfo,
	snaps map[types.NamespacedName]state.GatewaySnapshot,
) GatewayNode {
	key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	node := GatewayNode{
		Name:      gw.Name,
		Namespace: gw.Namespace,
		ClassName: string(gw.Spec.GatewayClassName),
		Exempt:    policy.IsExempt(gw, spec),
		Managed:   policy.Managed(gw, spec),
		IPs:       extractGatewayIPs(gw),
	}

	// Proxy service (for VIP display).
	proxy := b.findProxyService(ctx, gw)
	if proxy != nil {
		node.ProxyService = &ServiceRef{Name: proxy.Name, Namespace: proxy.Namespace, Type: string(proxy.Spec.Type)}
		if len(node.IPs) == 0 {
			for _, ing := range proxy.Status.LoadBalancer.Ingress {
				if ing.IP != "" {
					node.IPs = appendUnique(node.IPs, ing.IP)
				}
			}
		}
	}

	// MetalLB attribution is filled by the caller via pool matching, but record
	// a best-effort flag here too.
	node.FromMetalLB = false

	// Advertisement state is determined from GROUND TRUTH — whether the
	// Gateway's proxy Deployment is scaled to zero (Beacon's withdrawal
	// mechanism) — so the dashboard is consistent regardless of which replica
	// serves it and survives controller restarts. The in-memory store (only
	// populated on the leader) is used solely to surface the transient
	// "Pending*" states, which are not encoded in the proxy scale.
	proxyDrained := b.proxyScaledToZero(ctx, gw)
	snap, hasSnap := snaps[key]
	switch {
	case proxyDrained:
		node.Advertisement = string(beaconv1alpha1.AdvertisementWithdrawn)
	case hasSnap && snap.Advertisement != "":
		node.Advertisement = snap.Advertisement
	default:
		node.Advertisement = string(beaconv1alpha1.AdvertisementAdvertised)
	}
	// Overlay a Pending* state from the leader's store when the ground-truth
	// label and the desired state disagree (a dampening timer is running).
	if hasSnap {
		switch snap.Advertisement {
		case string(beaconv1alpha1.AdvertisementPendingWithdrawal):
			if node.Advertisement == string(beaconv1alpha1.AdvertisementAdvertised) {
				node.Advertisement = snap.Advertisement
			}
		case string(beaconv1alpha1.AdvertisementPendingReadvertise):
			if node.Advertisement == string(beaconv1alpha1.AdvertisementWithdrawn) {
				node.Advertisement = snap.Advertisement
			}
		}
		node.Message = snap.Message
	}

	// Routes -> backend services -> pods.
	var allProbed, allUnhealthy int
	routes := routesByGateway[key]
	sort.Slice(routes, func(i, j int) bool { return routes[i].name < routes[j].name })
	for _, ri := range routes {
		rn := RouteNode{
			Kind:      ri.kind,
			Name:      ri.name,
			Namespace: ri.namespace,
			Hostnames: ri.hostnames,
		}
		for _, bref := range ri.backends {
			svc := &corev1.Service{}
			if err := b.Client.Get(ctx, bref, svc); err != nil {
				continue
			}
			sn := ServiceNode{Name: svc.Name, Namespace: svc.Namespace, Type: string(svc.Spec.Type)}
			pods := b.podsForService(ctx, svc)
			for pi := range pods {
				eval := health.EvaluatePod(&pods[pi])
				pn := PodNode{
					Name:      pods[pi].Name,
					Namespace: pods[pi].Namespace,
					Node:      pods[pi].Spec.NodeName,
					Phase:     string(pods[pi].Status.Phase),
					Probed:    eval.Probed,
					Ready:     eval.Ready,
					Reason:    eval.Reason,
					Status:    podStatus(eval),
				}
				if eval.Probed {
					allProbed++
					if !eval.Ready {
						allUnhealthy++
					}
				}
				sn.Pods = append(sn.Pods, pn)
			}
			sn.Status = worstPodStatus(sn.Pods)
			rn.Services = append(rn.Services, sn)
		}
		rn.Status = worstServiceStatus(rn.Services)
		node.Routes = append(node.Routes, rn)
	}

	// Aggregate Gateway health from backend pods.
	switch {
	case node.Exempt:
		node.Health = StatusExempt
	case allProbed == 0:
		node.Health = StatusExempt
	case allUnhealthy == 0:
		node.Health = StatusHealthy
	case allUnhealthy == allProbed:
		node.Health = StatusUnhealthy
	default:
		node.Health = StatusDegraded
	}

	node.Status = gatewayStatus(node)
	return node
}

// loadPolicySpec returns the effective policy spec (with defaults) even if the
// CR is absent.
func (b *Builder) loadPolicySpec(ctx context.Context) *beaconv1alpha1.GatewayHealthPolicySpec {
	pol := &beaconv1alpha1.GatewayHealthPolicy{}
	err := b.Client.Get(ctx, types.NamespacedName{Name: b.PolicyName}, pol)
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{}
	if err == nil {
		spec = &pol.Spec
	}
	if spec.MetalLB.Namespace == "" {
		spec.MetalLB.Namespace = "metallb-system"
	}
	return spec
}

// indexRoutes lists all supported route kinds and groups them by target Gateway.
func (b *Builder) indexRoutes(ctx context.Context) (map[types.NamespacedName][]routeInfo, error) {
	out := map[types.NamespacedName][]routeInfo{}

	httpRoutes := &gwapiv1.HTTPRouteList{}
	if err := b.list(ctx, httpRoutes); err != nil {
		return nil, err
	}
	for i := range httpRoutes.Items {
		rt := &httpRoutes.Items[i]
		ri := routeInfo{kind: "HTTPRoute", name: rt.Name, namespace: rt.Namespace}
		for _, h := range rt.Spec.Hostnames {
			ri.hostnames = append(ri.hostnames, string(h))
		}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				if ref, ok := serviceRef(br.BackendObjectReference, rt.Namespace); ok {
					ri.backends = append(ri.backends, ref)
				}
			}
		}
		attachRoute(out, rt.Spec.ParentRefs, rt.Namespace, ri)
	}

	grpcRoutes := &gwapiv1.GRPCRouteList{}
	if err := b.list(ctx, grpcRoutes); err != nil {
		return nil, err
	}
	for i := range grpcRoutes.Items {
		rt := &grpcRoutes.Items[i]
		ri := routeInfo{kind: "GRPCRoute", name: rt.Name, namespace: rt.Namespace}
		for _, h := range rt.Spec.Hostnames {
			ri.hostnames = append(ri.hostnames, string(h))
		}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				if ref, ok := serviceRef(br.BackendObjectReference, rt.Namespace); ok {
					ri.backends = append(ri.backends, ref)
				}
			}
		}
		attachRoute(out, rt.Spec.ParentRefs, rt.Namespace, ri)
	}

	tcpRoutes := &gwapiv1alpha2.TCPRouteList{}
	if err := b.list(ctx, tcpRoutes); err != nil {
		return nil, err
	}
	for i := range tcpRoutes.Items {
		rt := &tcpRoutes.Items[i]
		ri := routeInfo{kind: "TCPRoute", name: rt.Name, namespace: rt.Namespace}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				if ref, ok := serviceRef(br.BackendObjectReference, rt.Namespace); ok {
					ri.backends = append(ri.backends, ref)
				}
			}
		}
		attachRoute(out, rt.Spec.ParentRefs, rt.Namespace, ri)
	}

	tlsRoutes := &gwapiv1alpha2.TLSRouteList{}
	if err := b.list(ctx, tlsRoutes); err != nil {
		return nil, err
	}
	for i := range tlsRoutes.Items {
		rt := &tlsRoutes.Items[i]
		ri := routeInfo{kind: "TLSRoute", name: rt.Name, namespace: rt.Namespace}
		for _, h := range rt.Spec.Hostnames {
			ri.hostnames = append(ri.hostnames, string(h))
		}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				if ref, ok := serviceRef(br.BackendObjectReference, rt.Namespace); ok {
					ri.backends = append(ri.backends, ref)
				}
			}
		}
		attachRoute(out, rt.Spec.ParentRefs, rt.Namespace, ri)
	}

	return out, nil
}

func attachRoute(out map[types.NamespacedName][]routeInfo, parents []gwapiv1.ParentReference, routeNS string, ri routeInfo) {
	for _, p := range parents {
		if p.Group != nil && string(*p.Group) != gwapiv1.GroupName {
			continue
		}
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		ns := routeNS
		if p.Namespace != nil && string(*p.Namespace) != "" {
			ns = string(*p.Namespace)
		}
		key := types.NamespacedName{Namespace: ns, Name: string(p.Name)}
		out[key] = append(out[key], ri)
	}
}

func serviceRef(ref gwapiv1.BackendObjectReference, routeNS string) (types.NamespacedName, bool) {
	if ref.Kind != nil && string(*ref.Kind) != "Service" {
		return types.NamespacedName{}, false
	}
	if ref.Group != nil && string(*ref.Group) != "" {
		return types.NamespacedName{}, false
	}
	ns := routeNS
	if ref.Namespace != nil && string(*ref.Namespace) != "" {
		ns = string(*ref.Namespace)
	}
	return types.NamespacedName{Namespace: ns, Name: string(ref.Name)}, true
}

// proxyScaledToZero reports whether all of the Gateway's proxy Deployments are
// scaled to zero replicas — Beacon's ground-truth "withdrawn" signal.
func (b *Builder) proxyScaledToZero(ctx context.Context, gw *gwapiv1.Gateway) bool {
	list := &appsv1.DeploymentList{}
	if err := b.Client.List(ctx, list,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{gatewayServiceLabel: gw.Name},
	); err != nil || len(list.Items) == 0 {
		return false
	}
	for i := range list.Items {
		r := int32(1)
		if list.Items[i].Spec.Replicas != nil {
			r = *list.Items[i].Spec.Replicas
		}
		if r != 0 {
			return false
		}
	}
	return true
}

func (b *Builder) findProxyService(ctx context.Context, gw *gwapiv1.Gateway) *corev1.Service {
	labeled := &corev1.ServiceList{}
	if err := b.Client.List(ctx, labeled,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{gatewayServiceLabel: gw.Name},
	); err == nil {
		for i := range labeled.Items {
			if labeled.Items[i].Spec.Type == corev1.ServiceTypeLoadBalancer {
				return &labeled.Items[i]
			}
		}
	}
	gwIPs := map[string]struct{}{}
	for _, ip := range extractGatewayIPs(gw) {
		gwIPs[ip] = struct{}{}
	}
	if len(gwIPs) == 0 {
		return nil
	}
	all := &corev1.ServiceList{}
	if err := b.Client.List(ctx, all, client.InNamespace(gw.Namespace)); err != nil {
		return nil
	}
	for i := range all.Items {
		svc := &all.Items[i]
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if _, ok := gwIPs[ing.IP]; ok {
				return svc
			}
		}
	}
	return nil
}

func (b *Builder) podsForService(ctx context.Context, svc *corev1.Service) []corev1.Pod {
	sliceList := &discoveryv1.EndpointSliceList{}
	if err := b.Client.List(ctx, sliceList,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name},
	); err != nil {
		return nil
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
	if len(podRefs) == 0 && len(svc.Spec.Selector) > 0 {
		podList := &corev1.PodList{}
		if err := b.Client.List(ctx, podList,
			client.InNamespace(svc.Namespace),
			client.MatchingLabels(svc.Spec.Selector),
		); err == nil {
			return podList.Items
		}
	}
	var pods []corev1.Pod
	for ref := range podRefs {
		pod := &corev1.Pod{}
		if err := b.Client.Get(ctx, ref, pod); err != nil {
			continue
		}
		pods = append(pods, *pod)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods
}

func (b *Builder) list(ctx context.Context, list client.ObjectList) error {
	err := b.Client.List(ctx, list)
	if ignorableListErr(err) {
		return nil
	}
	return err
}

func ignorableListErr(err error) bool {
	if err == nil {
		return true
	}
	return apimeta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err)
}

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

func appendUnique(s []string, v string) []string {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}

func ipLess(a, b string) bool {
	ia, ib := net.ParseIP(a), net.ParseIP(b)
	if ia == nil || ib == nil {
		return a < b
	}
	return strings.Compare(string(ia.To16()), string(ib.To16())) < 0
}
