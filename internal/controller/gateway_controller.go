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

package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	beaconv1alpha1 "github.com/beacon-operator/beacon/api/v1alpha1"
	"github.com/beacon-operator/beacon/internal/advertiser"
	"github.com/beacon-operator/beacon/internal/health"
	"github.com/beacon-operator/beacon/internal/identity"
	"github.com/beacon-operator/beacon/internal/metrics"
	"github.com/beacon-operator/beacon/internal/policy"
	"github.com/beacon-operator/beacon/internal/state"
	"github.com/beacon-operator/beacon/internal/trace"
	"github.com/beacon-operator/beacon/internal/version"
)

// GatewayReconciler reconciles Gateway API Gateways against MetalLB
// advertisements based on the health of the Gateways' backing workloads.
//
// One reconcile pass:
//  1. Load the singleton GatewayHealthPolicy (config).
//  2. Skip if the Gateway is exempt or its class is filtered out.
//  3. Trace the Gateway to its LoadBalancer Service(s) and backing Pods.
//  4. Evaluate aggregate health from pod probes (no-probe pods are exempt).
//  5. Determine whether IPs are sourced from MetalLB.
//  6. Run the dampening state machine (withdrawAfter / readvertiseAfter) and,
//     when a timer elapses, advertise or withdraw the route in MetalLB.
//  7. Update GatewayHealthPolicy status.
type GatewayReconciler struct {
	client.Client
	Recorder record.EventRecorder

	// PolicyName is the name of the singleton GatewayHealthPolicy.
	PolicyName string

	// States is a shared, thread-safe store the web UI reads to annotate the
	// topology graph with live advertisement/health decisions. Optional.
	States *state.Store

	// state tracks per-Gateway health transition timestamps for dampening.
	mu    sync.Mutex
	state map[types.NamespacedName]*gwState
}

type gwState struct {
	health         beaconv1alpha1.HealthState
	sinceHealthy   *time.Time
	sinceUnhealthy *time.Time
	advertisement  beaconv1alpha1.AdvertisementState
	ips            []string
	fromMetalLB    bool
	timer          *state.TimerStatus
}

// Gateways: read + watch (For()). Status is read in-object; no separate verb.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// Routes attached to Gateways: read + watch to react to attachment changes.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes;grpcroutes;tcproutes;tlsroutes,verbs=get;list;watch
// Services: read backends (live, uncached — see cmd/main.go's
// Client.Cache.DisableFor); create+update the operator-owned metrics and
// dashboard Services (never patch/delete). No "watch" verb: Services are
// deliberately not cached/watched (see SetupWithManager's doc comment).
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;create;update
// Pods: read backends (live, uncached, for the same reason). No "watch".
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// Deployments: read all (live, uncached); the only write is a MergeFrom patch
// of the proxy Deployment's replicas/annotations (withdraw/advertise). No
// update/delete, no scale subresource, and no "watch" (see above).
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// IPAddressPools: read-only; list/watch are required because reads go through
// the controller-runtime cache (informer-backed).
// +kubebuilder:rbac:groups=metallb.io,resources=ipaddresspools,verbs=get;list;watch
// Skupper Listeners & OpenShift console: read-only (cache-backed reads need
// list/watch even though only Get is called).
// +kubebuilder:rbac:groups=skupper.io,resources=listeners,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=consoles,verbs=get;list;watch
// Cluster identity (status.cluster): read-only, groundwork for multi-cluster
// fleets correlating Beacon status with Red Hat Advanced Cluster Management.
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions;infrastructures,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// Dashboard Route/ConsoleLink and monitoring CRs: read/watch (cache) + the
// create/update used to provision them. No patch/delete.
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=console.openshift.io,resources=consolelinks,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update
// SubjectAccessReviews: per-user dashboard authorization checks.
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// TokenReviews: used by the oauth-proxy sidecar (shares the operator SA) to
// validate dashboard user tokens.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// Secrets (get + create the dashboard oauth cookie Secret) and ServiceAccounts
// (get + update the operator SA's OAuth redirect annotation) are granted via a
// namespaced Role scoped to the operator's own namespace, not a ClusterRole —
// both are only ever touched there. See config/rbac/dashboard_role.yaml (or,
// for the OLM-bundled install, the CSV's "permissions" block) and the manager
// cache's Cache.ByObject scoping in cmd/main.go, which restricts these two
// types' informers to that same namespace so a namespaced Role suffices.
// GatewayHealthPolicy: read/watch config; only the status subresource is written
// (via Patch). The spec is never modified.
// +kubebuilder:rbac:groups=beacon.io,resources=gatewayhealthpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=beacon.io,resources=gatewayhealthpolicies/status,verbs=patch

// Reconcile implements the control loop for a single Gateway.
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("gateway", req.NamespacedName)

	// Load configuration (singleton policy).
	pol, err := r.loadPolicy(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pol == nil {
		// No policy configured yet; nothing to do. Requeue slowly.
		logger.V(1).Info("no GatewayHealthPolicy found; skipping")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	spec := &pol.Spec
	resync := durationOr(spec.ResyncInterval.Duration, 10*time.Second)

	// Resolve cluster identity once per reconcile (cheap cache-backed reads;
	// see internal/identity). Used to label metrics and to populate
	// status.cluster, so a multi-cluster hub can attribute this Gateway's data
	// to the right cluster.
	clusterIdentity := identity.Resolve(ctx, r.Client, spec.ClusterName)
	clusterLabel := identity.Label(clusterIdentity)

	// Fetch the Gateway.
	gw := &gwapiv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gw); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.clearState(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Exemption / class filtering.
	if !policy.Managed(gw, spec) {
		logger.V(1).Info("gateway exempt or filtered; skipping")
		r.recordGatewayStatus(req.NamespacedName, beaconv1alpha1.HealthExempt, beaconv1alpha1.AdvertisementAdvertised, nil)
		return ctrl.Result{}, nil
	}

	// Trace to services and pods. The per-Service pod-health threshold defaults
	// to the Gateway-level value (Services may override via annotation).
	resolver := &trace.Resolver{Client: r.Client}
	gatewayPodPercent := int(policy.MinHealthyPodPercent(gw, spec))
	gatewayZeroPolicy := policy.ZeroReplicasPolicy(gw, spec)
	resolution, err := resolver.Resolve(ctx, gw, gatewayPodPercent, gatewayZeroPolicy)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Determine whether IPs come from MetalLB.
	fromMetalLB, err := r.ipsFromMetalLB(ctx, spec.MetalLB.Namespace, resolution.IPs)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Evaluate Gateway health per-backend-Service against the configured
	// minimum-healthy-backend percentage (default 100 = any counted backend
	// down withdraws). Skupper-linked backends are already folded into
	// ServiceHealths.
	threshold := int(policy.MinHealthyBackendPercent(gw, spec))
	decision := health.EvaluateGateway(resolution.ServiceHealths, threshold)
	current := beaconv1alpha1.HealthExempt
	switch {
	case decision.Exempt:
		current = beaconv1alpha1.HealthExempt
	case decision.Unhealthy:
		current = beaconv1alpha1.HealthUnhealthy
	default:
		current = beaconv1alpha1.HealthHealthy
	}
	logger.V(1).Info("evaluated health",
		"health", current,
		"countedBackends", decision.Counted,
		"healthyBackends", decision.Healthy,
		"healthyPercent", decision.HealthyPercent,
		"threshold", threshold,
		"criticalDown", decision.CriticalDown,
		"remoteBackends", len(resolution.RemoteBackends),
		"ips", resolution.IPs,
		"fromMetalLB", fromMetalLB,
	)
	if decision.CriticalDown {
		logger.Info("gateway has a critical backend down; forcing withdrawal regardless of min-healthy-backend threshold",
			"gateway", req.NamespacedName.String())
	}

	// If IPs are not from MetalLB, we observe but never mutate advertisements.
	if !fromMetalLB || len(resolution.IPs) == 0 {
		r.recordGatewayStatus(req.NamespacedName, current, beaconv1alpha1.AdvertisementAdvertised, resolution.IPs)
		return ctrl.Result{RequeueAfter: resync}, nil
	}

	// Run dampening state machine and act.
	adv := &advertiser.Advertiser{Client: r.Client, Config: spec.MetalLB}

	requeue, advState, timer, err := r.reconcileAdvertisement(ctx, gw, spec, current, resolution, adv, resync, clusterLabel)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.recordGatewayStatusWithIP(req.NamespacedName, current, advState, timer, resolution.IPs, fromMetalLB)
	if err := r.updatePolicyStatus(ctx, pol, clusterIdentity); err != nil {
		logger.V(1).Info("failed updating policy status", "error", err.Error())
	}

	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcileAdvertisement runs the flap-dampening state machine. It returns the
// requeue interval and the advertisement state now in effect.
func (r *GatewayReconciler) reconcileAdvertisement(
	ctx context.Context,
	gw *gwapiv1.Gateway,
	spec *beaconv1alpha1.GatewayHealthPolicySpec,
	current beaconv1alpha1.HealthState,
	resolution *trace.GatewayResolution,
	adv *advertiser.Advertiser,
	resync time.Duration,
	clusterLabel string,
) (time.Duration, beaconv1alpha1.AdvertisementState, *state.TimerStatus, error) {
	key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	now := time.Now()

	withdrawAfter := durationOr(policy.WithdrawAfter(gw, spec).Duration, 5*time.Second)
	readvertiseAfter := durationOr(policy.ReadvertiseAfter(gw, spec).Duration, 30*time.Second)

	r.mu.Lock()
	st := r.state[key]
	if st == nil {
		st = &gwState{advertisement: beaconv1alpha1.AdvertisementAdvertised}
		r.state[key] = st
	}

	// Update health transition timestamps.
	switch current {
	case beaconv1alpha1.HealthUnhealthy:
		if st.sinceUnhealthy == nil {
			t := now
			st.sinceUnhealthy = &t
		}
		st.sinceHealthy = nil
	default: // Healthy / Exempt / Unknown are treated as "not failing".
		if st.sinceHealthy == nil {
			t := now
			st.sinceHealthy = &t
		}
		st.sinceUnhealthy = nil
	}
	st.health = current
	prevAdv := st.advertisement
	r.mu.Unlock()

	// If paused, never mutate; just report intended state.
	if spec.Paused {
		return resync, prevAdv, nil, nil
	}

	// Decision logic.
	switch prevAdv {
	case beaconv1alpha1.AdvertisementWithdrawn, beaconv1alpha1.AdvertisementPendingReadvertise:
		// Currently withdrawn (or pending re-advertise). Restore only after the
		// workload has been continuously healthy for the recovery duration
		// (readvertiseAfter).
		if current == beaconv1alpha1.HealthUnhealthy {
			d, s, err := r.transition(ctx, key, adv, resolution, beaconv1alpha1.AdvertisementWithdrawn, false, resync)
			return d, s, nil, err
		}
		r.mu.Lock()
		healthySince := st.sinceHealthy
		r.mu.Unlock()
		if healthySince != nil && now.Sub(*healthySince) >= readvertiseAfter {
			r.event(gw, corev1.EventTypeNormal, "Readvertised",
				fmt.Sprintf("workload healthy for %s (recovery); restoring MetalLB advertisement for %v", readvertiseAfter, resolution.IPs))
			metrics.ReadvertisementsTotal.WithLabelValues(clusterLabel, gw.Namespace, gw.Name).Inc()
			d, s, err := r.transition(ctx, key, adv, resolution, beaconv1alpha1.AdvertisementAdvertised, true, resync)
			return d, s, nil, err
		}
		// Recovery timer still running.
		r.setAdvState(key, beaconv1alpha1.AdvertisementPendingReadvertise)
		elapsed := now.Sub(deref(healthySince, now))
		remaining := readvertiseAfter - elapsed
		timer := newTimer("recovery", readvertiseAfter, elapsed)
		return clampRequeue(remaining, resync), beaconv1alpha1.AdvertisementPendingReadvertise, timer, nil

	default: // Advertised or PendingWithdrawal
		if current != beaconv1alpha1.HealthUnhealthy {
			// Healthy: ensure advertised.
			d, s, err := r.transition(ctx, key, adv, resolution, beaconv1alpha1.AdvertisementAdvertised, true, resync)
			return d, s, nil, err
		}
		// Unhealthy: withdraw only after continuous unhealthy for the backoff
		// duration (withdrawAfter).
		r.mu.Lock()
		unhealthySince := st.sinceUnhealthy
		r.mu.Unlock()
		if unhealthySince != nil && now.Sub(*unhealthySince) >= withdrawAfter {
			r.event(gw, corev1.EventTypeWarning, "Withdrawn",
				fmt.Sprintf("workload unhealthy for %s (backoff); withdrawing MetalLB advertisement for %v", withdrawAfter, resolution.IPs))
			metrics.WithdrawalsTotal.WithLabelValues(clusterLabel, gw.Namespace, gw.Name).Inc()
			d, s, err := r.transition(ctx, key, adv, resolution, beaconv1alpha1.AdvertisementWithdrawn, false, resync)
			return d, s, nil, err
		}
		r.setAdvState(key, beaconv1alpha1.AdvertisementPendingWithdrawal)
		elapsed := now.Sub(deref(unhealthySince, now))
		remaining := withdrawAfter - elapsed
		timer := newTimer("backoff", withdrawAfter, elapsed)
		return clampRequeue(remaining, resync), beaconv1alpha1.AdvertisementPendingWithdrawal, timer, nil
	}
}

// newTimer builds a TimerStatus, clamping elapsed/remaining to sane bounds.
func newTimer(kind string, threshold, elapsed time.Duration) *state.TimerStatus {
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > threshold {
		elapsed = threshold
	}
	remaining := threshold - elapsed
	if remaining < 0 {
		remaining = 0
	}
	return &state.TimerStatus{
		Kind:      kind,
		Threshold: threshold,
		Elapsed:   elapsed,
		Remaining: remaining,
	}
}

// transition applies the advertise/withdraw action and records the new state.
func (r *GatewayReconciler) transition(
	ctx context.Context,
	key types.NamespacedName,
	adv *advertiser.Advertiser,
	resolution *trace.GatewayResolution,
	target beaconv1alpha1.AdvertisementState,
	advertise bool,
	resync time.Duration,
) (time.Duration, beaconv1alpha1.AdvertisementState, error) {
	var err error
	if advertise {
		err = adv.Advertise(ctx, key.Namespace, key.Name)
	} else {
		err = adv.Withdraw(ctx, key.Namespace, key.Name)
	}
	if err != nil {
		return resync, target, err
	}
	r.setAdvState(key, target)
	return resync, target, nil
}

func (r *GatewayReconciler) setAdvState(key types.NamespacedName, s beaconv1alpha1.AdvertisementState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.state[key]; st != nil {
		st.advertisement = s
	}
}

func (r *GatewayReconciler) clearState(key types.NamespacedName) {
	r.mu.Lock()
	delete(r.state, key)
	r.mu.Unlock()
	if r.States != nil {
		r.States.Delete(key)
	}
}

// ipsFromMetalLB reports whether any of the given IPs falls within a MetalLB
// IPAddressPool.
func (r *GatewayReconciler) ipsFromMetalLB(ctx context.Context, namespace string, ips []string) (bool, error) {
	if len(ips) == 0 {
		return false, nil
	}
	pools := &metallbPoolList{}
	if err := r.List(ctx, pools.list(), client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for _, ip := range ips {
		if pools.contains(ip) {
			return true, nil
		}
	}
	return false, nil
}

func aggregateHealth(res health.Result) beaconv1alpha1.HealthState {
	if res.AllExempt() {
		return beaconv1alpha1.HealthExempt
	}
	if res.Healthy() {
		return beaconv1alpha1.HealthHealthy
	}
	return beaconv1alpha1.HealthUnhealthy
}

func durationOr(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

func clampRequeue(remaining, resync time.Duration) time.Duration {
	if remaining <= 0 {
		return time.Second
	}
	if remaining < resync {
		return remaining
	}
	return resync
}

func deref(t *time.Time, fallback time.Time) time.Time {
	if t == nil {
		return fallback
	}
	return *t
}

// timeToMeta converts an optional *time.Time to *metav1.Time, nil-safe.
func timeToMeta(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	mt := metav1.NewTime(*t)
	return &mt
}

func (r *GatewayReconciler) event(gw *gwapiv1.Gateway, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(gw, eventType, reason, msg)
	}
}

// SetupWithManager wires the controller's watches.
//
// Health is derived from BACKEND workload pods, which may live in different
// namespaces than the Gateway and are reached indirectly via xRoutes. Because a
// precise reverse mapping (pod -> backend Service -> route -> Gateway) is
// expensive to compute on every pod event, Beacon takes the robust approach of
// re-enqueuing all Gateways on any relevant backend change. Each Gateway
// reconcile then re-traces precisely and cheaply; the resync interval bounds
// worst-case latency.
//
// Deliberately NOT watched here: Pods, Services, and Deployments. A .Watches()
// registration makes controller-runtime open a cluster-wide list+watch informer
// that caches every object of that type FOREVER, in full, regardless of
// whether Beacon cares about it — on a large or shared cluster that is by far
// the biggest driver of the operator's memory footprint, and it grows with
// total cluster size, not with the number of Gateways Beacon actually manages.
// EndpointSlices are watched instead: they already carry per-endpoint
// readiness (kept in sync with backend Pod readiness by the endpointslice
// controller on essentially the same timeline pod-watching would give us), and
// there are normally an order of magnitude fewer, much smaller EndpointSlice
// objects than Pods. Pod/Service/Deployment data is instead read live
// (uncached; see cmd/main.go's Client.Cache.DisableFor) on demand, narrowly
// scoped by namespace+label to the specific Gateway being reconciled (see
// internal/trace and internal/advertiser) — bounded by what Beacon manages,
// not by total cluster size. Any change these types could still cause that an
// EndpointSlice update wouldn't (e.g. a Service's selector or type changing) is
// picked up within one resync interval, same as today.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.state == nil {
		r.state = map[types.NamespacedName]*gwState{}
	}
	logger := mgr.GetLogger().WithName("setup")

	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&gwapiv1.Gateway{}).
		// Backend workload readiness signal (any namespace); see the
		// doc comment above for why Pods/Services/Deployments aren't watched
		// directly.
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.mapToAllGateways)).
		// Route attachment changes (which backends a Gateway fronts). HTTPRoute
		// and GRPCRoute are part of the standard Gateway API channel; TCPRoute
		// and TLSRoute are experimental and often absent, so their watches are
		// only registered when the CRD is installed.
		Watches(&gwapiv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(r.mapToAllGateways)).
		Watches(&gwapiv1.GRPCRoute{}, handler.EnqueueRequestsFromMapFunc(r.mapToAllGateways)).
		// Configuration changes.
		Watches(&beaconv1alpha1.GatewayHealthPolicy{}, handler.EnqueueRequestsFromMapFunc(r.mapToAllGateways))

	if r.crdInstalled(mgr, gwapiv1alpha2.SchemeGroupVersion.WithKind("TCPRoute")) {
		bldr = bldr.Watches(&gwapiv1alpha2.TCPRoute{}, handler.EnqueueRequestsFromMapFunc(r.mapToAllGateways))
	} else {
		logger.Info("TCPRoute CRD not installed; skipping its watch")
	}
	if r.crdInstalled(mgr, gwapiv1alpha2.SchemeGroupVersion.WithKind("TLSRoute")) {
		bldr = bldr.Watches(&gwapiv1alpha2.TLSRoute{}, handler.EnqueueRequestsFromMapFunc(r.mapToAllGateways))
	} else {
		logger.Info("TLSRoute CRD not installed; skipping its watch")
	}

	return bldr.Named("gateway-health").Complete(r)
}

// crdInstalled reports whether the given kind is served by the API server, using
// the manager's RESTMapper. Used to avoid registering watches for optional CRDs
// (TCPRoute/TLSRoute) that are not present.
func (r *GatewayReconciler) crdInstalled(mgr ctrl.Manager, gvk schema.GroupVersionKind) bool {
	mappings, err := mgr.GetRESTMapper().RESTMappings(gvk.GroupKind(), gvk.Version)
	return err == nil && len(mappings) > 0
}

// mapToAllGateways enqueues every Gateway. Used for backend/route/config
// signals whose precise Gateway association is resolved during reconcile.
func (r *GatewayReconciler) mapToAllGateways(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.allGatewayRequests(ctx)
}

func (r *GatewayReconciler) allGatewayRequests(ctx context.Context) []reconcile.Request {
	list := &gwapiv1.GatewayList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace, Name: list.Items[i].Name,
		}})
	}
	return reqs
}

func (r *GatewayReconciler) loadPolicy(ctx context.Context) (*beaconv1alpha1.GatewayHealthPolicy, error) {
	pol := &beaconv1alpha1.GatewayHealthPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.PolicyName}, pol); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, err
	}
	return pol, nil
}

// --- status bookkeeping (in-memory, flushed to the policy status) ---

func (r *GatewayReconciler) recordGatewayStatus(key types.NamespacedName, h beaconv1alpha1.HealthState, a beaconv1alpha1.AdvertisementState, ips []string) {
	r.recordGatewayStatusWithIP(key, h, a, nil, ips, false)
}

func (r *GatewayReconciler) recordGatewayStatusWithIP(key types.NamespacedName, h beaconv1alpha1.HealthState, a beaconv1alpha1.AdvertisementState, timer *state.TimerStatus, ips []string, fromMetalLB bool) {
	r.mu.Lock()
	st := r.state[key]
	if st == nil {
		st = &gwState{}
		r.state[key] = st
	}
	st.health = h
	if a != "" {
		st.advertisement = a
	}
	st.ips = ips
	st.fromMetalLB = fromMetalLB
	st.timer = timer
	adv := string(st.advertisement)
	r.mu.Unlock()

	// Publish to the shared store for the web UI.
	if r.States != nil {
		r.States.Set(key, state.GatewaySnapshot{
			Health:         string(h),
			Advertisement:  adv,
			LastTransition: time.Now(),
			Timer:          timer,
		})
	}
}

// updatePolicyStatus recomputes aggregate counters and writes them to the
// policy's status subresource.
func (r *GatewayReconciler) updatePolicyStatus(ctx context.Context, pol *beaconv1alpha1.GatewayHealthPolicy, clusterIdentity beaconv1alpha1.ClusterIdentity) error {
	clusterLabel := identity.Label(clusterIdentity)
	r.mu.Lock()
	var managed, advertised, withdrawn int32
	gateways := make([]beaconv1alpha1.GatewayStatus, 0, len(r.state))
	for key, st := range r.state {
		managed++
		switch st.advertisement {
		case beaconv1alpha1.AdvertisementWithdrawn, beaconv1alpha1.AdvertisementPendingReadvertise:
			withdrawn++
		default:
			advertised++
		}
		gs := beaconv1alpha1.GatewayStatus{
			Namespace:     key.Namespace,
			Name:          key.Name,
			IPs:           append([]string(nil), st.ips...),
			FromMetalLB:   st.fromMetalLB,
			Health:        st.health,
			Advertisement: st.advertisement,
		}
		// LastTransitionTime is when Health most recently flipped between
		// failing (Unhealthy) and not-failing (Healthy/Exempt/Unknown) — the
		// same instant the dampening timers above are measured from. Surfaced
		// in shared status (not just leader-local state) so every dashboard
		// replica, and any future multi-cluster consumer, can compute an
		// accurate "time in status" / staleness signal.
		if st.health == beaconv1alpha1.HealthUnhealthy {
			gs.LastTransitionTime = timeToMeta(st.sinceUnhealthy)
		} else {
			gs.LastTransitionTime = timeToMeta(st.sinceHealthy)
		}
		if st.timer != nil {
			gs.Timer = &beaconv1alpha1.TimerStatus{
				Kind:             st.timer.Kind,
				ThresholdSeconds: int64(st.timer.Threshold.Round(time.Second) / time.Second),
				ElapsedSeconds:   int64(st.timer.Elapsed.Round(time.Second) / time.Second),
				RemainingSeconds: int64(st.timer.Remaining.Round(time.Second) / time.Second),
			}
		}
		gateways = append(gateways, gs)

		// Per-Gateway metric gauges.
		healthy := 0.0
		if st.health != beaconv1alpha1.HealthUnhealthy {
			healthy = 1.0
		}
		metrics.GatewayHealthy.WithLabelValues(clusterLabel, key.Namespace, key.Name).Set(healthy)
		adv := 1.0
		switch st.advertisement {
		case beaconv1alpha1.AdvertisementWithdrawn, beaconv1alpha1.AdvertisementPendingReadvertise:
			adv = 0.0
		}
		metrics.GatewayAdvertised.WithLabelValues(clusterLabel, key.Namespace, key.Name).Set(adv)
	}
	r.mu.Unlock()

	// Aggregate metric gauges.
	metrics.ManagedGateways.WithLabelValues(clusterLabel).Set(float64(managed))
	metrics.AdvertisedIPs.WithLabelValues(clusterLabel).Set(float64(advertised))
	metrics.WithdrawnIPs.WithLabelValues(clusterLabel).Set(float64(withdrawn))
	metrics.SetClusterInfo(clusterLabel, clusterIdentity.ID, clusterIdentity.Name, string(clusterIdentity.Source), version.Get())

	sort.Slice(gateways, func(i, j int) bool {
		if gateways[i].Namespace != gateways[j].Namespace {
			return gateways[i].Namespace < gateways[j].Namespace
		}
		return gateways[i].Name < gateways[j].Name
	})

	patch := client.MergeFrom(pol.DeepCopy())
	pol.Status.ObservedGeneration = pol.Generation
	now := metav1.Now()
	pol.Status.LastReconciled = &now
	pol.Status.ManagedGateways = managed
	pol.Status.AdvertisedIPs = advertised
	pol.Status.WithdrawnIPs = withdrawn
	pol.Status.Gateways = gateways
	pol.Status.Cluster = clusterIdentity
	meta := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "Beacon is reconciling gateways",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: pol.Generation,
	}
	pol.Status.Conditions = upsertCondition(pol.Status.Conditions, meta)
	return r.Status().Patch(ctx, pol, patch)
}

func upsertCondition(conds []metav1.Condition, c metav1.Condition) []metav1.Condition {
	for i := range conds {
		if conds[i].Type == c.Type {
			if conds[i].Status == c.Status {
				c.LastTransitionTime = conds[i].LastTransitionTime
			}
			conds[i] = c
			return conds
		}
	}
	return append(conds, c)
}
