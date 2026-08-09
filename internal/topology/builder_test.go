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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
	"github.com/bmarlow/beacon/internal/metallb"
	"github.com/bmarlow/beacon/internal/skupper"
	"github.com/bmarlow/beacon/internal/state"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	must(t, clientgoscheme.AddToScheme(s))
	must(t, gwapiv1.Install(s))
	must(t, gwapiv1alpha2.Install(s))
	must(t, metallb.AddToScheme(s))
	must(t, beaconv1alpha1.AddToScheme(s))
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func typesNN(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

func TestBuild_FullHierarchy(t *testing.T) {
	s := scheme(t)

	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	pool.SetGroupVersionKind(metallb.IPAddressPoolGVK)

	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"},
		Spec:       gwapiv1.GatewaySpec{GatewayClassName: "cls"},
		Status: gwapiv1.GatewayStatus{
			Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.10"}},
		},
	}
	proxySvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gw-proxy", Namespace: "app",
			Labels: map[string]string{"gateway.networking.k8s.io/gateway-name": "gw"},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}},
		},
	}

	backendSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "app-svc", Namespace: "app"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "demo"}},
	}
	// One healthy probed pod + one unhealthy probed pod -> Degraded.
	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-ok", Namespace: "app"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", ReadinessProbe: &corev1.Probe{}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	badPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-bad", Namespace: "app"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", ReadinessProbe: &corev1.Probe{}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "app-svc-1", Namespace: "app",
			Labels: map[string]string{discoveryv1.LabelServiceName: "app-svc"}},
		Endpoints: []discoveryv1.Endpoint{
			{TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "pod-ok", Namespace: "app"}},
			{TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "pod-bad", Namespace: "app"}},
		},
	}

	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "app"},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}}},
			Hostnames:       []gwapiv1.Hostname{"demo.example.com"},
			Rules: []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{{
				BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Name: "app-svc"}},
			}}}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pool, gw, proxySvc, backendSvc, readyPod, badPod, slice, route).Build()

	// Simulate controller state: this gateway's IP is currently advertised.
	store := state.New()
	store.Set(typesNN("app", "gw"), state.GatewaySnapshot{Health: "Degraded", Advertisement: "Advertised"})

	b := &Builder{Client: cl, States: store, PolicyName: "cluster"}
	g, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if len(g.Pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(g.Pools))
	}
	p := g.Pools[0]
	if p.Name != "gw-pool" || len(p.IPs) != 1 {
		t.Fatalf("unexpected pool: %+v", p)
	}
	ip := p.IPs[0]
	if ip.IP != "192.0.2.10" {
		t.Fatalf("expected VIP 192.0.2.10, got %s", ip.IP)
	}
	if len(ip.Gateways) != 1 {
		t.Fatalf("expected 1 gateway under IP, got %d", len(ip.Gateways))
	}
	gn := ip.Gateways[0]
	if gn.Name != "gw" {
		t.Fatalf("expected gateway 'gw', got %s", gn.Name)
	}
	if len(gn.Routes) != 1 || gn.Routes[0].Kind != "HTTPRoute" {
		t.Fatalf("expected 1 HTTPRoute, got %+v", gn.Routes)
	}
	rt := gn.Routes[0]
	if len(rt.Hostnames) != 1 || rt.Hostnames[0] != "demo.example.com" {
		t.Fatalf("expected hostname, got %+v", rt.Hostnames)
	}
	if len(rt.Services) != 1 || rt.Services[0].Name != "app-svc" {
		t.Fatalf("expected backend service app-svc, got %+v", rt.Services)
	}
	svc := rt.Services[0]
	if len(svc.Pods) != 2 {
		t.Fatalf("expected 2 backend pods, got %d", len(svc.Pods))
	}
	// Health is per-Service: the single backend Service has a failing pod, so
	// the service is down. With the default 100% threshold, 0/1 healthy
	// backends => the Gateway is Unhealthy.
	if gn.Health != StatusUnhealthy {
		t.Fatalf("expected gateway health Unhealthy, got %s", gn.Health)
	}
	if gn.CountedBackends != 1 || gn.HealthyBackends != 0 || gn.MinHealthyPercent != 100 {
		t.Fatalf("expected 0/1 backends at 100%%, got %d/%d min=%d",
			gn.HealthyBackends, gn.CountedBackends, gn.MinHealthyPercent)
	}

	// Summary sanity.
	if g.Summary.Pools != 1 || g.Summary.Gateways != 1 || g.Summary.Routes != 1 ||
		g.Summary.Services != 1 || g.Summary.Pods != 2 {
		t.Fatalf("unexpected summary: %+v", g.Summary)
	}
	if g.Summary.AdvertisedIPs != 1 {
		t.Fatalf("expected 1 advertised IP, got %d", g.Summary.AdvertisedIPs)
	}
}

func TestBuild_WithdrawnReflectsInStatus(t *testing.T) {
	s := scheme(t)
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.5"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, gw).Build()
	store := state.New()
	store.Set(typesNN("app", "gw"), state.GatewaySnapshot{Health: "Unhealthy", Advertisement: "Withdrawn"})

	b := &Builder{Client: cl, States: store, PolicyName: "cluster"}
	g, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(g.Pools) != 1 || len(g.Pools[0].IPs) != 1 {
		t.Fatalf("expected 1 pool/ip, got %+v", g.Pools)
	}
	if g.Pools[0].IPs[0].Status != StatusWithdrawn {
		t.Fatalf("expected IP status Withdrawn, got %s", g.Pools[0].IPs[0].Status)
	}
	if g.Summary.WithdrawnIPs != 1 {
		t.Fatalf("expected 1 withdrawn IP, got %d", g.Summary.WithdrawnIPs)
	}
}

func TestBuild_TimerSurfacedInStatus(t *testing.T) {
	s := scheme(t)
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.5"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, gw).Build()

	// Backoff timer running: unhealthy 2s of 5s threshold.
	store := state.New()
	store.Set(typesNN("app", "gw"), state.GatewaySnapshot{
		Health:        "Unhealthy",
		Advertisement: "PendingWithdrawal",
		Timer: &state.TimerStatus{
			Kind:      "backoff",
			Threshold: 5 * time.Second,
			Elapsed:   2 * time.Second,
			Remaining: 3 * time.Second,
		},
	})

	b := &Builder{Client: cl, States: store, PolicyName: "cluster"}
	g, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ip := g.Pools[0].IPs[0]
	if ip.Timer == nil {
		t.Fatal("expected timer on IP node")
	}
	if ip.Timer.Kind != "backoff" || ip.Timer.ThresholdSec != 5 ||
		ip.Timer.ElapsedSec != 2 || ip.Timer.RemainingSec != 3 {
		t.Fatalf("unexpected timer: %+v", ip.Timer)
	}
	if ip.Gateways[0].Timer == nil || ip.Gateways[0].Timer.Kind != "backoff" {
		t.Fatalf("expected backoff timer on gateway node, got %+v", ip.Gateways[0].Timer)
	}
}

func TestBuild_ReplicasVersionAndTiming(t *testing.T) {
	s := scheme(t)
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.7"}}},
	}
	// Proxy Deployment: desired 2, ready 1.
	two := int32(2)
	proxyDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gw-proxy", Namespace: "app",
			Labels: map[string]string{"gateway.networking.k8s.io/gateway-name": "gw"},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &two},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, gw, proxyDeploy).Build()

	store := state.New()
	store.Set(typesNN("app", "gw"), state.GatewaySnapshot{
		Health:         "Healthy",
		Advertisement:  "Advertised",
		LastTransition: time.Now().Add(-90 * time.Second),
	})

	g, err := (&Builder{Client: cl, States: store, PolicyName: "cluster"}).Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if g.OperatorVersion == "" {
		t.Fatal("expected a non-empty operator version")
	}
	gwn := g.Pools[0].IPs[0].Gateways[0]
	if gwn.ReplicasReady != 1 || gwn.ReplicasDesired != 2 {
		t.Fatalf("expected replicas 1/2, got %d/%d", gwn.ReplicasReady, gwn.ReplicasDesired)
	}
	if gwn.StatusForSeconds < 80 || gwn.StatusForSeconds > 100 {
		t.Fatalf("expected ~90s in status, got %d", gwn.StatusForSeconds)
	}
	if gwn.StatusSince == nil {
		t.Fatal("expected StatusSince to be set")
	}
}

func TestBuild_RefsPopulated(t *testing.T) {
	s := scheme(t)
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.9"}}},
	}
	backendSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "app"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "app"}}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "app",
			Labels: map[string]string{discoveryv1.LabelServiceName: "svc"}},
		Endpoints: []discoveryv1.Endpoint{{TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "pod", Namespace: "app"}}},
	}
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "app"},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}}},
			Rules: []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{{
				BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Name: "svc"}},
			}}}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pool, gw, backendSvc, pod, slice, route).Build()

	g, err := (&Builder{Client: cl, PolicyName: "cluster"}).Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	p := g.Pools[0]
	if p.Ref == nil || p.Ref.Group != "metallb.io" || p.Ref.Kind != "IPAddressPool" {
		t.Fatalf("pool ref wrong: %+v", p.Ref)
	}
	gwn := p.IPs[0].Gateways[0]
	if gwn.Ref == nil || gwn.Ref.Group != "gateway.networking.k8s.io" || gwn.Ref.Kind != "Gateway" {
		t.Fatalf("gateway ref wrong: %+v", gwn.Ref)
	}
	rt := gwn.Routes[0]
	if rt.Ref == nil || rt.Ref.Kind != "HTTPRoute" || rt.Ref.Version != "v1" {
		t.Fatalf("route ref wrong: %+v", rt.Ref)
	}
	sv := rt.Services[0]
	if sv.Ref == nil || sv.Ref.Plural != "services" || sv.Ref.Group != "" {
		t.Fatalf("service ref wrong: %+v", sv.Ref)
	}
	pd := sv.Pods[0]
	if pd.Ref == nil || pd.Ref.Plural != "pods" {
		t.Fatalf("pod ref wrong: %+v", pd.Ref)
	}
}

// fakeAuthz allows only resources in an allow-list; everything else hidden.
type fakeAuthz struct{ allow map[string]bool }

func (f *fakeAuthz) Allowed(_ context.Context, verb, group, resource, namespace, name string) bool {
	return f.allow[group+"/"+resource+"/"+namespace+"/"+name]
}

func TestBuild_RBACFiltering(t *testing.T) {
	s := scheme(t)
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	gwA := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a", Namespace: "team-a"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.10"}}},
	}
	gwB := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-b", Namespace: "team-b"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.11"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, gwA, gwB).Build()

	// User can read the pool and gw-a only (not gw-b).
	az := &fakeAuthz{allow: map[string]bool{
		"metallb.io/ipaddresspools/metallb-system/pool":  true,
		"gateway.networking.k8s.io/gateways/team-a/gw-a": true,
	}}
	g, err := (&Builder{Client: cl, PolicyName: "cluster", Authz: az}).Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Collect visible gateway names.
	var names []string
	for _, p := range g.Pools {
		for _, ip := range p.IPs {
			for _, gw := range ip.Gateways {
				names = append(names, gw.Name)
			}
		}
	}
	if len(names) != 1 || names[0] != "gw-a" {
		t.Fatalf("expected only gw-a visible, got %v", names)
	}
}

// A user who can see a Gateway but cannot read the backing IPAddressPool should
// still get a correctly-rendered pool -> VIP -> gateway tree, with the pool
// marked Restricted (contextual) and not linkable.
func TestBuild_RestrictedPoolStillShownForContext(t *testing.T) {
	s := scheme(t)
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.10"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, gw).Build()
	// User can see the Gateway but NOT read the pool (no metallb access).
	az := &fakeAuthz{allow: map[string]bool{
		"gateway.networking.k8s.io/gateways/app/gw": true,
	}}
	g, err := (&Builder{Client: cl, PolicyName: "cluster", Authz: az}).Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(g.Pools) != 1 {
		t.Fatalf("expected the pool shown for context, got %d", len(g.Pools))
	}
	p := g.Pools[0]
	if !p.Restricted {
		t.Fatal("expected pool marked Restricted")
	}
	if p.Ref != nil {
		t.Fatal("restricted pool must not be linkable (Ref should be nil)")
	}
	if len(p.IPs) != 1 || len(p.IPs[0].Gateways) != 1 || p.IPs[0].Gateways[0].Name != "gw" {
		t.Fatalf("expected gw under the pool's VIP, got %+v", p.IPs)
	}
	if len(g.UnpooledGateways) != 0 {
		t.Fatalf("expected no unpooled gateways, got %d", len(g.UnpooledGateways))
	}
}

func TestBuild_SkupperBackendUnhealthy(t *testing.T) {
	s := scheme(t)
	// Register the Skupper Listener GVK (unstructured) in the test scheme.
	s.AddKnownTypeWithName(skupper.ListenerGVK, &unstructured.Unstructured{})
	ll := skupper.ListenerGVK
	ll.Kind = "ListenerList"
	s.AddKnownTypeWithName(ll, &unstructured.UnstructuredList{})

	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "beta"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.30"}}},
	}
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	// Backend Service is Skupper-backed (listener label), selector -> router.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "beta-workload", Namespace: "beta",
			Labels: map[string]string{skupper.ListenerLabel: "beta-workload"},
		},
	}
	// Listener reports "No matching connectors" (remote down).
	lst := &unstructured.Unstructured{}
	lst.SetGroupVersionKind(skupper.ListenerGVK)
	lst.SetNamespace("beta")
	lst.SetName("beta-workload")
	_ = unstructured.SetNestedField(lst.Object, "Pending", "status", "status")
	_ = unstructured.SetNestedField(lst.Object, "No matching connectors", "status", "message")
	_ = unstructured.SetNestedSlice(lst.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "message": "No matching connectors"},
	}, "status", "conditions")

	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "beta"},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}}},
			Rules: []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{{
				BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Name: "beta-workload"}},
			}}}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(gw, pool, svc, lst, route).Build()

	g, err := (&Builder{Client: cl, PolicyName: "cluster"}).Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	gwn := g.Pools[0].IPs[0].Gateways[0]
	if gwn.Health != StatusUnhealthy {
		t.Fatalf("expected gateway Unhealthy due to failed Skupper link, got %s", gwn.Health)
	}
	sn := gwn.Routes[0].Services[0]
	if sn.Skupper == nil || sn.Skupper.Ready {
		t.Fatalf("expected skupper info with Ready=false, got %+v", sn.Skupper)
	}
	if len(sn.Pods) != 1 || !sn.Pods[0].Remote || sn.Pods[0].Ready {
		t.Fatalf("expected one unhealthy remote leaf, got %+v", sn.Pods)
	}
}

// Threshold test: 2 backend services, one down, threshold 50% (via policy) ->
// 50% healthy meets the inclusive threshold, so the Gateway stays up (Degraded).
func TestBuild_ThresholdKeepsGatewayUp(t *testing.T) {
	s := scheme(t)
	pool := &metallb.IPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "metallb-system"},
		Spec:       metallb.IPAddressPoolSpec{Addresses: []string{"192.0.2.0/24"}},
	}
	min := int32(50)
	pol := &beaconv1alpha1.GatewayHealthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       beaconv1alpha1.GatewayHealthPolicySpec{MinHealthyBackendPercent: &min},
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "app"},
		Status:     gwapiv1.GatewayStatus{Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.10"}}},
	}
	mkSvcPod := func(svcName, podName string, ready bool) []client.Object {
		st := corev1.ConditionTrue
		if !ready {
			st = corev1.ConditionFalse
		}
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: "app"}}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "app"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", ReadinessProbe: &corev1.Probe{}}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}},
		}
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: svcName + "-1", Namespace: "app",
				Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
			Endpoints: []discoveryv1.Endpoint{{TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: podName, Namespace: "app"}}},
		}
		return []client.Object{svc, pod, slice}
	}
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "app"},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}}},
			Rules: []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{
				{BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Name: "svc-up"}}},
				{BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{Name: "svc-down"}}},
			}}},
		},
	}
	objs := []client.Object{pool, pol, gw, route}
	objs = append(objs, mkSvcPod("svc-up", "pod-up", true)...)
	objs = append(objs, mkSvcPod("svc-down", "pod-down", false)...)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()

	g, err := (&Builder{Client: cl, PolicyName: "cluster"}).Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	gwn := g.Pools[0].IPs[0].Gateways[0]
	if gwn.Health != StatusDegraded {
		t.Fatalf("expected Degraded (50%% up meets 50%% threshold), got %s", gwn.Health)
	}
	if gwn.CountedBackends != 2 || gwn.HealthyBackends != 1 || gwn.MinHealthyPercent != 50 {
		t.Fatalf("expected 1/2 backends at 50%%, got %d/%d min=%d",
			gwn.HealthyBackends, gwn.CountedBackends, gwn.MinHealthyPercent)
	}
}
