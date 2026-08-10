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

package trace

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := gwapiv1.Install(s); err != nil {
		t.Fatal(err)
	}
	if err := gwapiv1alpha2.Install(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func strptr[T ~string](s T) *T { return &s }

// TestResolve_MonitorsBackendNotProxyPods is the key regression test: Beacon
// must evaluate the BACKEND workload pods (reached via HTTPRoute backendRefs),
// NOT the Gateway's proxy pods (which back the LoadBalancer Service).
func TestResolve_MonitorsBackendNotProxyPods(t *testing.T) {
	s := newScheme(t)
	ns := "app"

	// The Gateway.
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns},
		Spec:       gwapiv1.GatewaySpec{GatewayClassName: "cls"},
		Status: gwapiv1.GatewayStatus{
			Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.10"}},
		},
	}

	// The PROXY LoadBalancer Service (should NOT be the source of health pods).
	proxySvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw-proxy",
			Namespace: ns,
			Labels:    map[string]string{"gateway.networking.k8s.io/gateway-name": "gw"},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}},
			},
		},
	}
	// Proxy pod + its EndpointSlice. If Beacon wrongly monitored the proxy
	// Service, this pod would show up in the result.
	proxyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-pod", Namespace: ns},
	}
	proxySlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw-proxy-slice",
			Namespace: ns,
			Labels:    map[string]string{discoveryv1.LabelServiceName: "gw-proxy"},
		},
		Endpoints: []discoveryv1.Endpoint{{
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "proxy-pod", Namespace: ns},
		}},
	}

	// The BACKEND Service referenced by an HTTPRoute attached to the Gateway.
	backendSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "app-svc", Namespace: ns},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "demo"}},
	}
	backendPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: ns},
	}
	backendSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-svc-slice",
			Namespace: ns,
			Labels:    map[string]string{discoveryv1.LabelServiceName: "app-svc"},
		},
		Endpoints: []discoveryv1.Endpoint{{
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "app-pod", Namespace: ns},
		}},
	}

	// The HTTPRoute: parentRef -> gw, backendRef -> app-svc.
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: ns},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{{Name: "gw"}},
			},
			Rules: []gwapiv1.HTTPRouteRule{{
				BackendRefs: []gwapiv1.HTTPBackendRef{{
					BackendRef: gwapiv1.BackendRef{
						BackendObjectReference: gwapiv1.BackendObjectReference{
							Name: "app-svc",
						},
					},
				}},
			}},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(gw, proxySvc, proxyPod, proxySlice, backendSvc, backendPod, backendSlice, route).
		Build()

	r := &Resolver{Client: cl}
	res, err := r.Resolve(context.Background(), gw, 1, beaconv1alpha1.ZeroReplicasUnhealthy)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// VIP inferred from the Gateway status.
	if len(res.IPs) != 1 || res.IPs[0] != "192.0.2.10" {
		t.Fatalf("expected VIP 192.0.2.10, got %v", res.IPs)
	}

	// Advertisement target must be the PROXY service.
	if len(res.Services()) != 1 || res.Services()[0].Name != "gw-proxy" {
		t.Fatalf("expected proxy service for advertisement, got %v", res.Services())
	}

	// Health pods must be the BACKEND pods, NOT the proxy pods.
	if len(res.Pods) != 1 {
		t.Fatalf("expected exactly 1 health pod, got %d: %+v", len(res.Pods), res.Pods)
	}
	if res.Pods[0].Name != "app-pod" {
		t.Fatalf("expected backend pod 'app-pod' to be health-checked, got %q", res.Pods[0].Name)
	}
	for _, p := range res.Pods {
		if p.Name == "proxy-pod" {
			t.Fatal("proxy pod must NOT be included in health pods")
		}
	}
}

// TestResolve_CrossNamespaceBackend verifies backends in a different namespace
// than the Gateway are resolved via the backendRef namespace.
func TestResolve_CrossNamespaceBackend(t *testing.T) {
	s := newScheme(t)

	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Status: gwapiv1.GatewayStatus{
			Addresses: []gwapiv1.GatewayStatusAddress{{Value: "192.0.2.20"}},
		},
	}
	backendSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "team-a"},
	}
	backendPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "team-a"},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc-slice",
			Namespace: "team-a",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "svc"},
		},
		Endpoints: []discoveryv1.Endpoint{{
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "pod", Namespace: "team-a"},
		}},
	}
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team-a"},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{{
					Name:      "gw",
					Namespace: strptr[gwapiv1.Namespace]("infra"),
				}},
			},
			Rules: []gwapiv1.HTTPRouteRule{{
				BackendRefs: []gwapiv1.HTTPBackendRef{{
					BackendRef: gwapiv1.BackendRef{
						BackendObjectReference: gwapiv1.BackendObjectReference{Name: "svc"},
					},
				}},
			}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(gw, backendSvc, backendPod, slice, route).Build()

	res, err := (&Resolver{Client: cl}).Resolve(context.Background(), gw, 1, beaconv1alpha1.ZeroReplicasUnhealthy)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Pods) != 1 || res.Pods[0].Name != "pod" {
		t.Fatalf("expected cross-ns backend pod, got %+v", res.Pods)
	}
}

var _ client.Client = (client.Client)(nil)
