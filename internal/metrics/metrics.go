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

// Package metrics defines and registers Beacon's domain Prometheus metrics on
// the controller-runtime metrics registry (served on the manager's secure
// metrics endpoint and scraped via the shipped ServiceMonitor).
//
// Every metric carries a "cluster" label (the cluster's effective name, or its
// stable ID when no name is known — see internal/identity). This is groundwork
// for multi-cluster fleets: it lets a hub aggregate Beacon metrics from many
// clusters via classic Prometheus federation (which, unlike remote_write
// external_labels, does not inject an identifying label automatically) without
// series from different clusters colliding, and keeps binary operations
// between two Beacon metrics (e.g. withdrawn_ips / managed_gateways in the
// shipped PrometheusRule) correctly matched per-cluster once federated. A
// single running Beacon instance only ever populates one "cluster" value, so
// this adds no cardinality for a single-cluster install.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "beacon"

// clusterLabels is prepended to every metric's variable labels.
var clusterLabels = []string{"cluster"}

var (
	// Info is a standard "info metric" (always 1) carrying full cluster
	// identity and the operator version, for fleet inventory queries and for
	// joining other Beacon metrics to cluster identity in PromQL (e.g.
	// `beacon_managed_gateways * on(cluster) group_left(cluster_name)
	// beacon_info`) without repeating all identity fields on every metric.
	Info = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "info",
		Help:      "Always 1; labels carry cluster identity and the operator version.",
	}, []string{"cluster", "cluster_id", "cluster_name", "cluster_source", "version"})

	// ManagedGateways is the number of Gateways Beacon currently manages.
	ManagedGateways = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "managed_gateways",
		Help:      "Number of Gateways currently managed by Beacon.",
	}, clusterLabels)

	// AdvertisedIPs is the number of Gateway VIPs currently advertised.
	AdvertisedIPs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advertised_ips",
		Help:      "Number of Gateway VIPs currently advertised via MetalLB.",
	}, clusterLabels)

	// WithdrawnIPs is the number of Gateway VIPs currently withdrawn.
	WithdrawnIPs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "withdrawn_ips",
		Help:      "Number of Gateway VIPs currently withdrawn by Beacon.",
	}, clusterLabels)

	// GatewayHealthy reports per-Gateway health as a gauge (1=healthy,
	// 0=unhealthy) labeled by cluster/namespace/name.
	GatewayHealthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "gateway_healthy",
		Help:      "Per-Gateway health (1=healthy/exempt, 0=unhealthy).",
	}, []string{"cluster", "gateway_namespace", "gateway_name"})

	// GatewayAdvertised reports per-Gateway advertisement state (1=advertised,
	// 0=withdrawn) labeled by cluster/namespace/name.
	GatewayAdvertised = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "gateway_advertised",
		Help:      "Per-Gateway advertisement state (1=advertised, 0=withdrawn/pending-withdraw).",
	}, []string{"cluster", "gateway_namespace", "gateway_name"})

	// WithdrawalsTotal counts route withdrawals performed by Beacon.
	WithdrawalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "withdrawals_total",
		Help:      "Total number of MetalLB route withdrawals performed by Beacon.",
	}, []string{"cluster", "gateway_namespace", "gateway_name"})

	// ReadvertisementsTotal counts route re-advertisements performed by Beacon.
	ReadvertisementsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "readvertisements_total",
		Help:      "Total number of MetalLB route re-advertisements performed by Beacon.",
	}, []string{"cluster", "gateway_namespace", "gateway_name"})

	// ReconcileErrorsTotal counts reconcile errors.
	ReconcileErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_errors_total",
		Help:      "Total number of reconcile errors.",
	}, clusterLabels)
)

// MustRegister registers all Beacon metrics on the controller-runtime registry.
// Safe to call once at startup.
func MustRegister() {
	ctrlmetrics.Registry.MustRegister(
		Info,
		ManagedGateways,
		AdvertisedIPs,
		WithdrawnIPs,
		GatewayHealthy,
		GatewayAdvertised,
		WithdrawalsTotal,
		ReadvertisementsTotal,
		ReconcileErrorsTotal,
	)
}

// SetClusterInfo updates the beacon_info metric with the current cluster
// identity and operator version. Resets first so a changed identity (e.g. an
// edited spec.clusterName) doesn't leave a stale series behind; cheap and safe
// to call on every reconcile since there is only ever one active series.
func SetClusterInfo(clusterLabel, id, name, source, version string) {
	Info.Reset()
	Info.WithLabelValues(clusterLabel, id, name, source, version).Set(1)
}
