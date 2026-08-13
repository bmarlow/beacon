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

package export

import (
	"reflect"
	"testing"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
)

func TestBuild(t *testing.T) {
	pol := &beaconv1alpha1.GatewayHealthPolicy{
		Status: beaconv1alpha1.GatewayHealthPolicyStatus{
			ManagedGateways: 3,
			AdvertisedIPs:   2,
			WithdrawnIPs:    1,
			Cluster: beaconv1alpha1.ClusterIdentity{
				ID: "abc-123", Name: "prod-east", Source: beaconv1alpha1.ClusterIdentitySourceOpenShift,
			},
			Gateways: []beaconv1alpha1.GatewayStatus{
				{
					Namespace: "app", Name: "gw-a", IPs: []string{"10.0.0.1"}, FromMetalLB: true,
					Health: beaconv1alpha1.HealthHealthy, Advertisement: beaconv1alpha1.AdvertisementAdvertised,
				},
				{
					Namespace: "app", Name: "gw-b", IPs: []string{"10.0.0.2"}, FromMetalLB: true,
					Health: beaconv1alpha1.HealthUnhealthy, Advertisement: beaconv1alpha1.AdvertisementWithdrawn,
				},
			},
		},
	}

	s := Build(pol)

	if s.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", s.SchemaVersion, SchemaVersion)
	}
	if s.GeneratedAt.IsZero() {
		t.Fatal("expected GeneratedAt to be set")
	}
	wantCluster := ClusterInfo{ID: "abc-123", Name: "prod-east", Source: "OpenShiftClusterVersion"}
	if s.Cluster != wantCluster {
		t.Fatalf("Cluster = %+v, want %+v", s.Cluster, wantCluster)
	}
	if s.ManagedGateways != 3 || s.AdvertisedIPs != 2 || s.WithdrawnIPs != 1 {
		t.Fatalf("unexpected aggregate counts: %+v", s)
	}
	if len(s.Gateways) != 2 {
		t.Fatalf("expected 2 gateways, got %d", len(s.Gateways))
	}
	want0 := GatewaySummary{
		Namespace: "app", Name: "gw-a", IPs: []string{"10.0.0.1"}, FromMetalLB: true,
		Health: "Healthy", Advertisement: "Advertised",
	}
	if !reflect.DeepEqual(s.Gateways[0], want0) {
		t.Fatalf("Gateways[0] = %+v, want %+v", s.Gateways[0], want0)
	}
}

func TestBuild_NoPolicyYieldsEmptySummary(t *testing.T) {
	s := Build(&beaconv1alpha1.GatewayHealthPolicy{})
	if s.SchemaVersion != SchemaVersion {
		t.Fatalf("expected SchemaVersion set even with empty policy, got %q", s.SchemaVersion)
	}
	if s.ManagedGateways != 0 || len(s.Gateways) != 0 {
		t.Fatalf("expected zero-value counts/gateways, got %+v", s)
	}
}
