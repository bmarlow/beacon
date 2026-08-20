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
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	beaconv1alpha1 "github.com/beacon-operator/beacon/api/v1alpha1"
	"github.com/beacon-operator/beacon/internal/health"
	"github.com/beacon-operator/beacon/internal/metallb"
	"github.com/beacon-operator/beacon/internal/policy"
	"github.com/beacon-operator/beacon/internal/rcache"
	"github.com/beacon-operator/beacon/internal/skupper"
	"github.com/beacon-operator/beacon/internal/state"
	"github.com/beacon-operator/beacon/internal/version"
)

const gatewayServiceLabel = "gateway.networking.k8s.io/gateway-name"

// gatewayBuildConcurrency bounds how many Gateways' backend trees (Routes ->
// Services -> EndpointSlices -> Pods) are resolved in parallel per Build().
// Pod/Service/Deployment reads are live/uncached against the API server (see
// cmd/main.go's Client.Cache.DisableFor — a deliberate trade-off to keep
// memory bounded on large clusters, see SetupWithManager's doc comment in
// internal/controller/gateway_controller.go). On a cluster with many
// Gateways, resolving them one at a time made a single dashboard load take
// tens of seconds (each Gateway needs several sequential live API calls).
// Bounded parallelism keeps total wall-clock time close to that of a single
// Gateway instead of the sum of all of them, without opening unbounded
// concurrent requests against the API server.
const gatewayBuildConcurrency = 20

// Authorizer decides whether the requesting user may read a given resource.
// Implemented by webui.AccessChecker (SubjectAccessReview-backed). When nil,
// all resources are visible (auth disabled).
type Authorizer interface {
	// Allowed reports whether the user may perform verb on the resource
	// identified by group/resource/namespace/name. namespace is "" for
	// cluster-scoped resources.
	Allowed(ctx context.Context, verb, group, resource, namespace, name string) bool
}

// Builder assembles the topology Graph from cluster state plus live controller
// state (advertisement decisions) from the shared store.
type Builder struct {
	Client     client.Client
	States     *state.Store
	PolicyName string
	// Authz, when set, filters the graph to only the resources the requesting
	// user can read. Nil means no filtering (unauthenticated/local mode).
	Authz Authorizer
	// Cache, when set, short-circuits the repeated live Pod/Service/Deployment/
	// EndpointSlice reads that would otherwise happen on every dashboard poll
	// (the dashboard rebuilds the full graph from scratch on every request; by
	// default those types are uncached against the API server — see
	// cmd/main.go). A nil Cache simply disables this de-duplication (every
	// read goes live), which is safe for tests and callers that don't set one.
	Cache *rcache.Cache
}

// canRead is a nil-safe helper that returns true when no authorizer is set.
func (b *Builder) canRead(ctx context.Context, verb, group, resource, namespace, name string) bool {
	if b.Authz == nil {
		return true
	}
	return b.Authz.Allowed(ctx, verb, group, resource, namespace, name)
}

// routeInfo is an intermediate normalized route with its backend service refs.
type routeInfo struct {
	kind      string
	name      string
	namespace string
	hostnames []string
	backends  []types.NamespacedName
	// criticalAnn / criticalPresent capture the route's beacon.io/critical
	// annotation (for the Service > Route > Gateway precedence).
	criticalAnn     bool
	criticalPresent bool
}

// Build produces a full topology Graph.
func (b *Builder) Build(ctx context.Context) (*Graph, error) {
	g := &Graph{GeneratedAt: time.Now(), OperatorVersion: version.Get(), SchemaVersion: SchemaVersion}
	g.ConsoleBaseURL = b.consoleBaseURL(ctx)

	// Load policy (config + status). Status is shared across all replicas, so
	// using it for advertisement/timer keeps the dashboard consistent
	// regardless of which replica serves the request. Cluster identity is
	// likewise read from the shared status (computed once by the reconciling
	// leader) rather than re-resolved here, so it stays consistent across
	// replicas and requires no extra RBAC for the dashboard path.
	pol := b.loadPolicy(ctx)
	spec := &pol.Spec
	g.Cluster = ClusterInfo{
		ID:     pol.Status.Cluster.ID,
		Name:   pol.Status.Cluster.Name,
		Source: string(pol.Status.Cluster.Source),
	}
	if pol.Status.LastReconciled != nil {
		t := pol.Status.LastReconciled.Time
		g.LastReconciled = &t
	}
	if spec.MetalLB.Namespace == "" {
		spec.MetalLB.Namespace = "metallb-system"
	}
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

	// Build a GatewayNode for each Gateway. Prefer the policy's shared status
	// (written by the leader) so every replica renders identical timer/advert
	// state; fall back to the local in-memory store if status is empty.
	snaps := snapshotsFromPolicyStatus(pol)
	if len(snaps) == 0 && b.States != nil {
		snaps = b.States.Snapshot()
	}

	// Resolve each Gateway's backend tree with bounded parallelism — see
	// gatewayBuildConcurrency's doc comment for why this matters. Results are
	// written into a slice pre-sized to gwList.Items so ordering stays
	// identical to a sequential loop; canRead/buildGatewayNode only read
	// shared state here (routesByGateway, snaps), so this is race-free.
	type gwResult struct {
		node GatewayNode
		ok   bool
	}
	results := make([]gwResult, len(gwList.Items))
	sem := make(chan struct{}, gatewayBuildConcurrency)
	var wg sync.WaitGroup
	for i := range gwList.Items {
		gw := &gwList.Items[i]
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, gw *gwapiv1.Gateway) {
			defer wg.Done()
			defer func() { <-sem }()
			// Hide Gateways the requesting user cannot read.
			if !b.canRead(ctx, "get", "gateway.networking.k8s.io", "gateways", gw.Namespace, gw.Name) {
				return
			}
			results[i] = gwResult{node: b.buildGatewayNode(ctx, gw, spec, routesByGateway, snaps), ok: true}
		}(i, gw)
	}
	wg.Wait()

	var gatewayNodes []GatewayNode
	for i := range results {
		if results[i].ok {
			gatewayNodes = append(gatewayNodes, results[i].node)
		}
	}

	// Group Gateways under the pool that owns their IP.
	//
	// A pool is shown to the user if and only if it backs at least one Gateway
	// the user can already see. We do NOT require the user to have read access
	// to the IPAddressPool itself — the pool/VIP is contextual metadata about a
	// Gateway they already own, and hiding it would collapse the whole
	// hierarchy for users who lack access to the MetalLB namespace (the common
	// case). Pools the user cannot directly read are marked Restricted and are
	// not rendered as clickable console links.
	poolNodesByName := map[string]*PoolNode{}
	var poolOrder []string
	poolCanRead := map[string]bool{}

	ensurePoolNode := func(p *metallb.IPAddressPool) *PoolNode {
		if pn, ok := poolNodesByName[p.Name]; ok {
			return pn
		}
		canRead := b.canRead(ctx, "get", "metallb.io", "ipaddresspools", p.Namespace, p.Name)
		poolCanRead[p.Name] = canRead
		pn := &PoolNode{
			Name:       p.Name,
			Namespace:  p.Namespace,
			Addresses:  append([]string(nil), p.Spec.Addresses...),
			AutoAssign: p.Spec.AutoAssign,
			Restricted: !canRead,
		}
		if canRead {
			pn.Ref = poolRef(p.Namespace, p.Name)
		}
		poolNodesByName[p.Name] = pn
		poolOrder = append(poolOrder, p.Name)
		return pn
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
			// Create/reuse the pool node on demand, driven by visible gateways.
			pn := ensurePoolNode(pool)
			key := pool.Name + "/" + ip
			ipn := ipNodeIndex[key]
			if ipn == nil {
				ipn = &IPNode{IP: ip}
				ipNodeIndex[key] = ipn
				pn.IPs = append(pn.IPs, IPNode{}) // placeholder; filled after
			}
			ipn.Advertisement = gn.Advertisement
			if gn.Timer != nil {
				ipn.Timer = gn.Timer
			}
			// The VIP's "time in status" mirrors the owning Gateway's.
			if gn.StatusSince != nil {
				ipn.StatusTiming = gn.StatusTiming
			}
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
		var ipT []StatusTiming
		for i := range pn.IPs {
			ipT = append(ipT, pn.IPs[i].StatusTiming)
		}
		pn.StatusTiming = latestChildTiming(ipT)
	}

	// Additionally show pools the user CAN read directly even if they back no
	// visible Gateway (e.g. empty pools for admins/metallb readers). Users who
	// cannot read a pool only see it when it backs one of their Gateways (added
	// on demand above), so no extra MetalLB detail leaks.
	for i := range poolList.Items {
		p := &poolList.Items[i]
		if _, exists := poolNodesByName[p.Name]; exists {
			continue
		}
		if b.canRead(ctx, "get", "metallb.io", "ipaddresspools", p.Namespace, p.Name) {
			pn := &PoolNode{
				Name:       p.Name,
				Namespace:  p.Namespace,
				Addresses:  append([]string(nil), p.Spec.Addresses...),
				AutoAssign: p.Spec.AutoAssign,
				Ref:        poolRef(p.Namespace, p.Name),
				Status:     StatusUnknown,
			}
			poolNodesByName[p.Name] = pn
			poolOrder = append(poolOrder, p.Name)
		}
	}

	// Emit pools in stable order.
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
		Ref:       gatewayRef(gw.Namespace, gw.Name),
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

	// Proxy Deployment replica counts (data-plane scale). This is also Beacon's
	// withdrawal lever, so scaled-to-zero == withdrawn.
	ready, desired, _, proxyDrained := b.proxyReplicas(ctx, gw)
	node.ReplicasReady = ready
	node.ReplicasDesired = desired

	// Advertisement state is determined from GROUND TRUTH — whether the
	// Gateway's proxy Deployment is scaled to zero (Beacon's withdrawal
	// mechanism) — so the dashboard is consistent regardless of which replica
	// serves it and survives controller restarts. The in-memory store (only
	// populated on the leader) is used solely to surface the transient
	// "Pending*" states, which are not encoded in the proxy scale.
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
		if snap.Timer != nil {
			node.Timer = &Timer{
				Kind:         snap.Timer.Kind,
				ThresholdSec: int64(snap.Timer.Threshold.Round(time.Second) / time.Second),
				ElapsedSec:   int64(snap.Timer.Elapsed.Round(time.Second) / time.Second),
				RemainingSec: int64(snap.Timer.Remaining.Round(time.Second) / time.Second),
			}
		}
	}

	// Routes -> backend services -> pods.
	// Collect per-Service health for the minimum-healthy-backend-percentage rule.
	// gatewayPodPercent is the Gateway-level per-Service pod-health threshold
	// and gatewayZeroPolicy the scaled-to-zero policy (Services may override
	// both via annotations).
	gatewayPodPercent := policy.MinHealthyPodPercent(gw, spec)
	gatewayZeroPolicy := policy.ZeroReplicasPolicy(gw, spec)
	gatewayCritical := policy.GatewayCritical(gw)
	var svcHealths []health.ServiceHealth
	routes := routesByGateway[key]
	sort.Slice(routes, func(i, j int) bool { return routes[i].name < routes[j].name })
	for _, ri := range routes {
		// Hide routes the user cannot read.
		if !b.canRead(ctx, "get", "gateway.networking.k8s.io", routePlural(ri.kind), ri.namespace, ri.name) {
			continue
		}
		rn := RouteNode{
			Kind:      ri.kind,
			Name:      ri.name,
			Namespace: ri.namespace,
			Hostnames: ri.hostnames,
			Ref:       routeRef(ri.kind, ri.namespace, ri.name),
		}
		for _, bref := range ri.backends {
			// Hide backend Services the user cannot read.
			if !b.canRead(ctx, "get", "", "services", bref.Namespace, bref.Name) {
				continue
			}
			svc := b.getService(ctx, bref)
			if svc == nil {
				continue
			}
			sn := ServiceNode{
				Name: svc.Name, Namespace: svc.Namespace, Type: string(svc.Spec.Type),
				Ref: svcConsoleRef(svc.Namespace, svc.Name),
			}

			// Skupper-backed backend: the real workload is on a remote cluster
			// over a Skupper link. Evaluate the Listener status instead of local
			// pods (whose endpoints are the skupper-router, not the workload).
			if lname, ok := skupper.ServiceListenerName(svc.Labels); ok {
				sh := skupper.EvaluateListener(ctx, b.Client, svc.Namespace, lname)
				sn.Skupper = &SkupperInfo{ListenerName: lname, Ready: sh.Ready, Reason: sh.Reason}
				// Represent the remote workload as a single synthetic leaf so it
				// shows in the tree and counts toward Gateway health exactly like
				// a probed local pod.
				remote := PodNode{
					Name:      "remote: " + lname,
					Namespace: svc.Namespace,
					Phase:     "Remote (Skupper)",
					Probed:    true,
					Ready:     sh.Ready,
					Reason:    sh.Reason,
					Remote:    true,
				}
				if sh.Ready {
					remote.Status = StatusHealthy
				} else {
					remote.Status = StatusUnhealthy
				}
				sn.Critical = policy.BackendCritical(svc.Annotations, ri.criticalAnn, ri.criticalPresent, gatewayCritical)
				svcHealths = append(svcHealths, health.ServiceHealth{
					Namespace: svc.Namespace, Name: svc.Name, Counted: true, Healthy: sh.Ready,
					Critical: sn.Critical,
				})
				sn.Pods = append(sn.Pods, remote)
				sn.Status = worstPodStatus(sn.Pods)
				rn.Services = append(rn.Services, sn)
				continue
			}

			pods := b.podsForService(ctx, svc)
			svcProbed, svcUnhealthy := 0, 0
			for pi := range pods {
				// Hide pods the user cannot read.
				if !b.canRead(ctx, "get", "", "pods", pods[pi].Namespace, pods[pi].Name) {
					continue
				}
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
					Ref:       podRef(pods[pi].Namespace, pods[pi].Name),
				}
				pn.StatusTiming = podStatusTiming(&pods[pi])
				if eval.Probed {
					svcProbed++
					if !eval.Ready {
						svcUnhealthy++
					}
				}
				sn.Pods = append(sn.Pods, pn)
			}
			// Per-Service "up" verdict uses the pod-health percentage threshold
			// (Service annotation overrides the Gateway-level value; default
			// 1 = any Ready pod). A Service with a selector but zero pods
			// (scaled to zero) is counted as failing unless its effective
			// zeroReplicasPolicy is Exempt.
			podPct := policy.ServiceMinHealthyPodPercent(svc.Annotations, gatewayPodPercent)
			svcCounted := svcProbed > 0
			svcReady := svcProbed - svcUnhealthy
			svcHealthy := false
			svcScaledToZero := false
			if svcCounted {
				svcHealthy = (100*svcReady)/svcProbed >= int(podPct)
			} else if len(pods) == 0 && len(svc.Spec.Selector) > 0 {
				zeroPol := policy.ServiceZeroReplicasPolicy(svc.Annotations, gatewayZeroPolicy)
				if zeroPol == beaconv1alpha1.ZeroReplicasUnhealthy {
					svcCounted = true // counted + not healthy
					svcScaledToZero = true
				}
			}
			sn.ScaledToZero = svcScaledToZero
			sn.Critical = policy.BackendCritical(svc.Annotations, ri.criticalAnn, ri.criticalPresent, gatewayCritical)
			svcHealths = append(svcHealths, health.ServiceHealth{
				Namespace: svc.Namespace, Name: svc.Name,
				Counted:  svcCounted,
				Healthy:  svcHealthy,
				Critical: sn.Critical,
			})
			// The Service chip reflects the SAME threshold verdict used for the
			// Gateway decision (minHealthyPodPercent), not the worst individual
			// pod — otherwise a Service that is "up" per the threshold (e.g. the
			// default 1% = any Ready pod) would still show red while the Gateway
			// correctly stays up. A counted Service that meets the threshold but
			// has some pods down is Degraded (up, but partially failing); below
			// the threshold it is Unhealthy.
			switch {
			case svcScaledToZero:
				sn.Status = StatusUnhealthy
			case !svcCounted:
				// No probed pods (all probe-less / exempt) → not health-checked.
				sn.Status = worstPodStatus(sn.Pods)
			case !svcHealthy:
				sn.Status = StatusUnhealthy
			case svcUnhealthy > 0:
				sn.Status = StatusDegraded
			default:
				sn.Status = StatusHealthy
			}
			// A Service's "time in status" is the most recent of its pods'
			// status transitions (the last time its aggregate could have changed).
			sn.StatusTiming = latestChildTiming(podTimings(sn.Pods))
			rn.Services = append(rn.Services, sn)
		}
		rn.Status = worstServiceStatus(rn.Services)
		rn.StatusTiming = latestChildTiming(serviceTimings(rn.Services))
		node.Routes = append(node.Routes, rn)
	}

	// Aggregate Gateway health per-Service against the min-healthy-backend
	// percentage threshold (default 100). Below threshold => Unhealthy (VIP
	// withdrawn); at/above threshold but with some counted backend down =>
	// Degraded (still advertised); all healthy => Healthy.
	threshold := int(policy.MinHealthyBackendPercent(gw, spec))
	node.MinHealthyPercent = int32(threshold)
	decision := health.EvaluateGateway(svcHealths, threshold)
	node.HealthyBackends = int32(decision.Healthy)
	node.CountedBackends = int32(decision.Counted)
	node.CriticalBackendDown = decision.CriticalDown
	switch {
	case node.Exempt || decision.Exempt:
		node.Health = StatusExempt
	case decision.Unhealthy:
		node.Health = StatusUnhealthy
	case decision.Healthy == decision.Counted:
		node.Health = StatusHealthy
	default:
		node.Health = StatusDegraded
	}

	node.Status = gatewayStatus(node)
	// Gateway "time in status": prefer the controller-recorded last transition
	// (when Beacon last changed health/advertisement); fall back to the newest
	// backend-pod transition.
	if hasSnap && !snap.LastTransition.IsZero() {
		node.StatusTiming = timingSince(snap.LastTransition)
	} else {
		var childT []StatusTiming
		for i := range node.Routes {
			childT = append(childT, node.Routes[i].StatusTiming)
		}
		node.StatusTiming = latestChildTiming(childT)
	}
	return node
}

// loadPolicy returns the GatewayHealthPolicy (empty object if absent).
func (b *Builder) loadPolicy(ctx context.Context) *beaconv1alpha1.GatewayHealthPolicy {
	pol := &beaconv1alpha1.GatewayHealthPolicy{}
	if err := b.Client.Get(ctx, types.NamespacedName{Name: b.PolicyName}, pol); err != nil {
		return &beaconv1alpha1.GatewayHealthPolicy{}
	}
	return pol
}

// snapshotsFromPolicyStatus converts the policy's shared per-Gateway status into
// the snapshot map the builder consumes.
func snapshotsFromPolicyStatus(pol *beaconv1alpha1.GatewayHealthPolicy) map[types.NamespacedName]state.GatewaySnapshot {
	out := map[types.NamespacedName]state.GatewaySnapshot{}
	for i := range pol.Status.Gateways {
		gs := &pol.Status.Gateways[i]
		snap := state.GatewaySnapshot{
			Health:        string(gs.Health),
			Advertisement: string(gs.Advertisement),
			Message:       gs.Message,
		}
		if gs.LastTransitionTime != nil {
			snap.LastTransition = gs.LastTransitionTime.Time
		}
		if gs.Timer != nil {
			snap.Timer = &state.TimerStatus{
				Kind:      gs.Timer.Kind,
				Threshold: time.Duration(gs.Timer.ThresholdSeconds) * time.Second,
				Elapsed:   time.Duration(gs.Timer.ElapsedSeconds) * time.Second,
				Remaining: time.Duration(gs.Timer.RemainingSeconds) * time.Second,
			}
		}
		out[types.NamespacedName{Namespace: gs.Namespace, Name: gs.Name}] = snap
	}
	return out
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
		ri.criticalAnn, ri.criticalPresent = policy.RouteCritical(rt.Annotations)
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
		ri.criticalAnn, ri.criticalPresent = policy.RouteCritical(rt.Annotations)
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
		ri.criticalAnn, ri.criticalPresent = policy.RouteCritical(rt.Annotations)
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
		ri.criticalAnn, ri.criticalPresent = policy.RouteCritical(rt.Annotations)
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

// proxyReplicas returns the summed ready/desired replica counts across the
// Gateway's proxy Deployment(s), whether any proxy Deployment exists, and
// whether all are scaled to zero (Beacon's ground-truth "withdrawn" signal).
type proxyReplicaResult struct {
	ready, desired int32
	found, allZero bool
}

func (b *Builder) proxyReplicas(ctx context.Context, gw *gwapiv1.Gateway) (ready, desired int32, found, allZero bool) {
	cacheKey := "proxyreplicas/" + gw.Namespace + "/" + gw.Name
	if v, ok := b.Cache.Get(cacheKey); ok {
		r, _ := v.(proxyReplicaResult)
		return r.ready, r.desired, r.found, r.allZero
	}
	ready, desired, found, allZero = b.proxyReplicasLive(ctx, gw)
	b.Cache.Set(cacheKey, proxyReplicaResult{ready, desired, found, allZero})
	return ready, desired, found, allZero
}

func (b *Builder) proxyReplicasLive(ctx context.Context, gw *gwapiv1.Gateway) (ready, desired int32, found, allZero bool) {
	list := &appsv1.DeploymentList{}
	if err := b.Client.List(ctx, list,
		client.InNamespace(gw.Namespace),
		client.MatchingLabels{gatewayServiceLabel: gw.Name},
	); err != nil || len(list.Items) == 0 {
		return 0, 0, false, false
	}
	found = true
	allZero = true
	for i := range list.Items {
		d := &list.Items[i]
		spec := int32(1)
		if d.Spec.Replicas != nil {
			spec = *d.Spec.Replicas
		}
		desired += spec
		ready += d.Status.ReadyReplicas
		if spec != 0 {
			allZero = false
		}
	}
	return ready, desired, found, allZero
}

func (b *Builder) findProxyService(ctx context.Context, gw *gwapiv1.Gateway) *corev1.Service {
	cacheKey := "proxysvc/" + gw.Namespace + "/" + gw.Name
	if v, ok := b.Cache.Get(cacheKey); ok {
		svc, _ := v.(*corev1.Service)
		return svc
	}
	svc := b.findProxyServiceLive(ctx, gw)
	b.Cache.Set(cacheKey, svc)
	return svc
}

func (b *Builder) findProxyServiceLive(ctx context.Context, gw *gwapiv1.Gateway) *corev1.Service {
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

// getService fetches a single Service by name, short-circuiting via Cache
// (including caching "not found" as a nil result) so repeated dashboard polls
// don't re-issue the same live Get for backends that haven't changed.
func (b *Builder) getService(ctx context.Context, key types.NamespacedName) *corev1.Service {
	cacheKey := "svc/" + key.Namespace + "/" + key.Name
	if v, ok := b.Cache.Get(cacheKey); ok {
		svc, _ := v.(*corev1.Service)
		return svc
	}
	svc := &corev1.Service{}
	if err := b.Client.Get(ctx, key, svc); err != nil {
		b.Cache.Set(cacheKey, (*corev1.Service)(nil))
		return nil
	}
	b.Cache.Set(cacheKey, svc)
	return svc
}

// getPod fetches a single Pod by name, same caching rationale as getService.
func (b *Builder) getPod(ctx context.Context, ref types.NamespacedName) *corev1.Pod {
	cacheKey := "pod/" + ref.Namespace + "/" + ref.Name
	if v, ok := b.Cache.Get(cacheKey); ok {
		pod, _ := v.(*corev1.Pod)
		return pod
	}
	pod := &corev1.Pod{}
	if err := b.Client.Get(ctx, ref, pod); err != nil {
		b.Cache.Set(cacheKey, (*corev1.Pod)(nil))
		return nil
	}
	b.Cache.Set(cacheKey, pod)
	return pod
}

func (b *Builder) podsForService(ctx context.Context, svc *corev1.Service) []corev1.Pod {
	epsCacheKey := "eps/" + svc.Namespace + "/" + svc.Name
	var sliceItems []discoveryv1.EndpointSlice
	if v, ok := b.Cache.Get(epsCacheKey); ok {
		sliceItems, _ = v.([]discoveryv1.EndpointSlice)
	} else {
		sliceList := &discoveryv1.EndpointSliceList{}
		if err := b.Client.List(ctx, sliceList,
			client.InNamespace(svc.Namespace),
			client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name},
		); err != nil {
			return nil
		}
		sliceItems = sliceList.Items
		b.Cache.Set(epsCacheKey, sliceItems)
	}
	podRefs := map[types.NamespacedName]struct{}{}
	for i := range sliceItems {
		for _, ep := range sliceItems[i].Endpoints {
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
		pod := b.getPod(ctx, ref)
		if pod == nil {
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

// consoleBaseURL discovers the OpenShift web console URL from the cluster-scoped
// console.config.openshift.io/cluster resource (status.consoleURL), falling back
// to the console Route in openshift-console. Returns "" when not on OpenShift.
func (b *Builder) consoleBaseURL(ctx context.Context) string {
	// Try console.config.openshift.io/v1 "cluster".
	cfg := &unstructured.Unstructured{}
	cfg.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "Console",
	})
	if err := b.Client.Get(ctx, types.NamespacedName{Name: "cluster"}, cfg); err == nil {
		if u, found, _ := unstructured.NestedString(cfg.Object, "status", "consoleURL"); found && u != "" {
			return strings.TrimRight(u, "/")
		}
	}

	// Fallback: the console Route.
	rt := &unstructured.Unstructured{}
	rt.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "route.openshift.io", Version: "v1", Kind: "Route",
	})
	if err := b.Client.Get(ctx, types.NamespacedName{Namespace: "openshift-console", Name: "console"}, rt); err == nil {
		if host, found, _ := unstructured.NestedString(rt.Object, "spec", "host"); found && host != "" {
			return "https://" + host
		}
	}
	return ""
}

// --- console Ref constructors ---

func podRef(namespace, name string) *Ref {
	return &Ref{Version: "v1", Kind: "Pod", Plural: "pods", Namespace: namespace, Name: name}
}

func svcConsoleRef(namespace, name string) *Ref {
	return &Ref{Version: "v1", Kind: "Service", Plural: "services", Namespace: namespace, Name: name}
}

func gatewayRef(namespace, name string) *Ref {
	return &Ref{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway", Namespace: namespace, Name: name}
}

func routeRef(kind, namespace, name string) *Ref {
	// TCPRoute/TLSRoute live in v1alpha2; HTTPRoute/GRPCRoute in v1.
	version := "v1"
	if kind == "TCPRoute" || kind == "TLSRoute" {
		version = "v1alpha2"
	}
	return &Ref{Group: "gateway.networking.k8s.io", Version: version, Kind: kind, Namespace: namespace, Name: name}
}

func poolRef(namespace, name string) *Ref {
	return &Ref{Group: "metallb.io", Version: "v1beta1", Kind: "IPAddressPool", Namespace: namespace, Name: name}
}

// routePlural maps an xRoute Kind to its resource plural for authorization
// checks (SubjectAccessReview ResourceAttributes.Resource).
func routePlural(kind string) string {
	switch kind {
	case "HTTPRoute":
		return "httproutes"
	case "GRPCRoute":
		return "grpcroutes"
	case "TCPRoute":
		return "tcproutes"
	case "TLSRoute":
		return "tlsroutes"
	default:
		return ""
	}
}
