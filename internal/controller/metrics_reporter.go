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
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
	"github.com/bmarlow/beacon/internal/identity"
	"github.com/bmarlow/beacon/internal/metrics"
	"github.com/bmarlow/beacon/internal/version"
)

// defaultMetricsRefreshInterval is how often MetricsReporter republishes gauge
// metrics from the shared GatewayHealthPolicy status.
const defaultMetricsRefreshInterval = 15 * time.Second

// MetricsReporter periodically republishes Beacon's *gauge* metrics (current
// state: managed/advertised/withdrawn counts, per-Gateway health/advertisement,
// cluster identity) from the shared, cross-replica GatewayHealthPolicy status,
// on every replica regardless of leadership.
//
// Why this exists: GatewayReconciler's reconcile loop — and therefore
// updatePolicyStatus, which sets these same gauges from live in-memory state —
// only runs on the leader replica (controllers default to
// NeedLeaderElection=true). The manager's metrics HTTP listener, however, runs
// on every replica, and the operator is typically deployed with >1 replica for
// HA. Without this reporter, scraping a non-leader replica's /metrics (e.g. via
// the Service, which load-balances across replicas with no leader-only
// selector) would show these gauges as entirely absent — Prometheus
// GaugeVec/CounterVec series don't exist until WithLabelValues is first called
// in that process. That silently breaks federation/aggregation into a
// multi-cluster hub, which is the whole point of labeling these metrics by
// cluster in the first place.
//
// Event counters (WithdrawalsTotal, ReadvertisementsTotal) are deliberately
// NOT touched here: a counter must be incremented exactly once per actual
// event, and only the leader witnesses real transitions, so those remain
// leader-only by design — republishing them here would either double-count or
// require tracking deltas for no benefit, since Prometheus already tolerates a
// counter existing on only one target.
type MetricsReporter struct {
	// Client reads the GatewayHealthPolicy singleton.
	Client client.Client
	// PolicyName is the singleton GatewayHealthPolicy name.
	PolicyName string
	// Interval is how often to refresh. Defaults to 15s when zero.
	Interval time.Duration
}

// NeedLeaderElection makes the reporter run on every replica: it only reads
// the already-computed, shared status, so every replica can (and should)
// answer identically when scraped.
func (m *MetricsReporter) NeedLeaderElection() bool { return false }

// AddToManager registers the MetricsReporter as a manager Runnable.
func (m *MetricsReporter) AddToManager(mgr manager.Manager) error {
	return mgr.Add(m)
}

// Start implements manager.Runnable.
func (m *MetricsReporter) Start(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = defaultMetricsRefreshInterval
	}
	logger := log.FromContext(ctx).WithName("metrics-reporter")

	m.refresh(ctx, logger)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.refresh(ctx, logger)
		}
	}
}

// refresh reads the shared GatewayHealthPolicy status and republishes every
// gauge metric from it. Tolerates the policy not existing yet (nothing to
// report) without logging noise on every tick.
func (m *MetricsReporter) refresh(ctx context.Context, logger logr.Logger) {
	pol := &beaconv1alpha1.GatewayHealthPolicy{}
	if err := m.Client.Get(ctx, types.NamespacedName{Name: m.PolicyName}, pol); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "failed reading GatewayHealthPolicy for metrics refresh")
		}
		return
	}

	clusterLabel := identity.Label(pol.Status.Cluster)
	metrics.ManagedGateways.WithLabelValues(clusterLabel).Set(float64(pol.Status.ManagedGateways))
	metrics.AdvertisedIPs.WithLabelValues(clusterLabel).Set(float64(pol.Status.AdvertisedIPs))
	metrics.WithdrawnIPs.WithLabelValues(clusterLabel).Set(float64(pol.Status.WithdrawnIPs))
	metrics.SetClusterInfo(clusterLabel, pol.Status.Cluster.ID, pol.Status.Cluster.Name,
		string(pol.Status.Cluster.Source), version.Get())

	for i := range pol.Status.Gateways {
		gs := &pol.Status.Gateways[i]
		healthy := 0.0
		if gs.Health != beaconv1alpha1.HealthUnhealthy {
			healthy = 1.0
		}
		metrics.GatewayHealthy.WithLabelValues(clusterLabel, gs.Namespace, gs.Name).Set(healthy)
		adv := 1.0
		switch gs.Advertisement {
		case beaconv1alpha1.AdvertisementWithdrawn, beaconv1alpha1.AdvertisementPendingReadvertise:
			adv = 0.0
		}
		metrics.GatewayAdvertised.WithLabelValues(clusterLabel, gs.Namespace, gs.Name).Set(adv)
	}
}
