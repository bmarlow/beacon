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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
	"github.com/bmarlow/beacon/internal/metallb"
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
	// Degraded because one of two probed pods is failing.
	if gn.Health != StatusDegraded {
		t.Fatalf("expected gateway health Degraded, got %s", gn.Health)
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
