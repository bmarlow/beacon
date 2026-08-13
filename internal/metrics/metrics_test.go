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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestSetClusterInfo verifies beacon_info carries the given identity/version
// and that a changed identity does not leave a stale series behind (Reset
// before Set).
func TestSetClusterInfo(t *testing.T) {
	SetClusterInfo("prod-east", "abc-123", "prod-east", "OpenShiftClusterVersion", "v1.2.3")

	got := testutil.ToFloat64(Info.WithLabelValues("prod-east", "abc-123", "prod-east", "OpenShiftClusterVersion", "v1.2.3"))
	if got != 1 {
		t.Fatalf("expected beacon_info=1 for the current identity, got %v", got)
	}

	// Changing identity must not leave the old series behind.
	SetClusterInfo("prod-west", "xyz-789", "prod-west", "KubeSystemUID", "v1.2.3")
	if n := testutil.CollectAndCount(Info); n != 1 {
		t.Fatalf("expected exactly 1 beacon_info series after identity change, got %d", n)
	}
}

// TestClusterLabeledMetrics verifies the per-Gateway and aggregate metrics
// accept a "cluster" label without panicking and record distinct series per
// cluster (the shape multi-cluster federation depends on).
func TestClusterLabeledMetrics(t *testing.T) {
	ManagedGateways.WithLabelValues("cluster-a").Set(3)
	ManagedGateways.WithLabelValues("cluster-b").Set(5)

	if got := testutil.ToFloat64(ManagedGateways.WithLabelValues("cluster-a")); got != 3 {
		t.Fatalf("cluster-a managed_gateways = %v, want 3", got)
	}
	if got := testutil.ToFloat64(ManagedGateways.WithLabelValues("cluster-b")); got != 5 {
		t.Fatalf("cluster-b managed_gateways = %v, want 5", got)
	}

	GatewayHealthy.WithLabelValues("cluster-a", "ns", "gw").Set(1)
	WithdrawalsTotal.WithLabelValues("cluster-a", "ns", "gw").Inc()
	if got := testutil.ToFloat64(WithdrawalsTotal.WithLabelValues("cluster-a", "ns", "gw")); got != 1 {
		t.Fatalf("withdrawals_total = %v, want 1", got)
	}
}
