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
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	beaconv1alpha1 "github.com/beacon-operator/beacon/api/v1alpha1"
	"github.com/beacon-operator/beacon/internal/metrics"
	"github.com/beacon-operator/beacon/internal/version"
)

func reporterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := beaconv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMetricsReporter_RefreshFromStatus is the regression test for the
// leader-only-metrics gap: it verifies that gauges are populated purely by
// reading GatewayHealthPolicy.status (as every replica does), with no
// reconcile loop / in-memory state involved at all.
func TestMetricsReporter_RefreshFromStatus(t *testing.T) {
	pol := &beaconv1alpha1.GatewayHealthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: beaconv1alpha1.GatewayHealthPolicyStatus{
			ManagedGateways: 2,
			AdvertisedIPs:   1,
			WithdrawnIPs:    1,
			Cluster: beaconv1alpha1.ClusterIdentity{
				ID: "abc-123", Name: "prod-east", Source: beaconv1alpha1.ClusterIdentitySourceOpenShift,
			},
			Gateways: []beaconv1alpha1.GatewayStatus{
				{Namespace: "app", Name: "gw-a", Health: beaconv1alpha1.HealthHealthy, Advertisement: beaconv1alpha1.AdvertisementAdvertised},
				{Namespace: "app", Name: "gw-b", Health: beaconv1alpha1.HealthUnhealthy, Advertisement: beaconv1alpha1.AdvertisementWithdrawn},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(reporterScheme(t)).WithObjects(pol).Build()
	r := &MetricsReporter{Client: cl, PolicyName: "cluster"}

	r.refresh(context.Background(), logr.Discard())

	if got := testutil.ToFloat64(metrics.ManagedGateways.WithLabelValues("prod-east")); got != 2 {
		t.Fatalf("ManagedGateways = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.AdvertisedIPs.WithLabelValues("prod-east")); got != 1 {
		t.Fatalf("AdvertisedIPs = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.WithdrawnIPs.WithLabelValues("prod-east")); got != 1 {
		t.Fatalf("WithdrawnIPs = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.GatewayHealthy.WithLabelValues("prod-east", "app", "gw-a")); got != 1 {
		t.Fatalf("gw-a GatewayHealthy = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.GatewayHealthy.WithLabelValues("prod-east", "app", "gw-b")); got != 0 {
		t.Fatalf("gw-b GatewayHealthy = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.GatewayAdvertised.WithLabelValues("prod-east", "app", "gw-a")); got != 1 {
		t.Fatalf("gw-a GatewayAdvertised = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.GatewayAdvertised.WithLabelValues("prod-east", "app", "gw-b")); got != 0 {
		t.Fatalf("gw-b GatewayAdvertised = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.Info.WithLabelValues("prod-east", "abc-123", "prod-east", "OpenShiftClusterVersion", version.Get())); got != 1 {
		t.Fatalf("Info = %v, want 1", got)
	}
}

// TestMetricsReporter_NoPolicyIsTolerant verifies refresh doesn't panic or log
// spam when the GatewayHealthPolicy doesn't exist yet.
func TestMetricsReporter_NoPolicyIsTolerant(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(reporterScheme(t)).Build()
	r := &MetricsReporter{Client: cl, PolicyName: "cluster"}
	r.refresh(context.Background(), logr.Discard()) // must not panic
}

func TestMetricsReporter_NeedLeaderElection(t *testing.T) {
	r := &MetricsReporter{}
	if r.NeedLeaderElection() {
		t.Fatal("expected MetricsReporter to run on every replica (NeedLeaderElection=false)")
	}
}
