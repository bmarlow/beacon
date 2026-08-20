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

package identity

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	beaconv1alpha1 "github.com/beacon-operator/beacon/api/v1alpha1"
)

var (
	clusterVersionGVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "ClusterVersion"}
	infrastructureGVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure"}
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{clusterVersionGVK, infrastructureGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		list := gvk
		list.Kind += "List"
		s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	}
	return s
}

func clusterVersion(id string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(clusterVersionGVK)
	u.SetName("version")
	_ = unstructured.SetNestedField(u.Object, id, "spec", "clusterID")
	return u
}

func infrastructure(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(infrastructureGVK)
	u.SetName("cluster")
	_ = unstructured.SetNestedField(u.Object, name, "status", "infrastructureName")
	return u
}

func kubeSystemNS(uid string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(uid)},
	}
}

func TestResolve_OpenShiftClusterVersion(t *testing.T) {
	s := newScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(clusterVersion("abc-123"), infrastructure("prod-east")).
		Build()

	got := Resolve(context.Background(), cl, "")
	want := beaconv1alpha1.ClusterIdentity{
		ID: "abc-123", Name: "prod-east", Source: beaconv1alpha1.ClusterIdentitySourceOpenShift,
	}
	if got != want {
		t.Fatalf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestResolve_NameOverrideWins(t *testing.T) {
	s := newScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(clusterVersion("abc-123"), infrastructure("prod-east")).
		Build()

	got := Resolve(context.Background(), cl, "fleet-name-1")
	if got.Name != "fleet-name-1" {
		t.Fatalf("expected override name, got %q", got.Name)
	}
	if got.ID != "abc-123" || got.Source != beaconv1alpha1.ClusterIdentitySourceOpenShift {
		t.Fatalf("expected ID/Source unaffected by name override, got %+v", got)
	}
}

func TestResolve_FallsBackToKubeSystemUID(t *testing.T) {
	s := newScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(kubeSystemNS("uid-xyz")).
		Build()

	got := Resolve(context.Background(), cl, "")
	if got.ID != "uid-xyz" || got.Source != beaconv1alpha1.ClusterIdentitySourceKubeSystem {
		t.Fatalf("expected kube-system UID fallback, got %+v", got)
	}
	if got.Name != "" {
		t.Fatalf("expected empty name (no Infrastructure, no override), got %q", got.Name)
	}
}

func TestResolve_ManualWhenNothingAvailable(t *testing.T) {
	s := newScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()

	got := Resolve(context.Background(), cl, "")
	if got.Source != beaconv1alpha1.ClusterIdentitySourceManual {
		t.Fatalf("expected Manual source, got %+v", got)
	}
	if got.ID != "" {
		t.Fatalf("expected empty ID, got %q", got.ID)
	}
}

func TestResolve_ManualWithNameOverrideOnly(t *testing.T) {
	s := newScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()

	got := Resolve(context.Background(), cl, "my-cluster")
	if got.Source != beaconv1alpha1.ClusterIdentitySourceManual {
		t.Fatalf("expected Manual source, got %+v", got)
	}
	if got.Name != "my-cluster" {
		t.Fatalf("expected override name, got %q", got.Name)
	}
}

func TestLabel(t *testing.T) {
	tests := []struct {
		name string
		ci   beaconv1alpha1.ClusterIdentity
		want string
	}{
		{name: "name wins over id", ci: beaconv1alpha1.ClusterIdentity{ID: "abc", Name: "prod-east"}, want: "prod-east"},
		{name: "falls back to id", ci: beaconv1alpha1.ClusterIdentity{ID: "abc"}, want: "abc"},
		{name: "empty when neither set", ci: beaconv1alpha1.ClusterIdentity{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Label(tc.ci); got != tc.want {
				t.Fatalf("Label(%+v) = %q, want %q", tc.ci, got, tc.want)
			}
		})
	}
}
