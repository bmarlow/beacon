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
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "beacon"

var (
	// ManagedGateways is the number of Gateways Beacon currently manages.
	ManagedGateways = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "managed_gateways",
		Help:      "Number of Gateways currently managed by Beacon.",
	})

	// AdvertisedIPs is the number of Gateway VIPs currently advertised.
	AdvertisedIPs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advertised_ips",
		Help:      "Number of Gateway VIPs currently advertised via MetalLB.",
	})

	// WithdrawnIPs is the number of Gateway VIPs currently withdrawn.
	WithdrawnIPs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "withdrawn_ips",
		Help:      "Number of Gateway VIPs currently withdrawn by Beacon.",
	})

	// GatewayHealthy reports per-Gateway health as a gauge (1=healthy,
	// 0=unhealthy) labeled by namespace/name.
	GatewayHealthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "gateway_healthy",
		Help:      "Per-Gateway health (1=healthy/exempt, 0=unhealthy).",
	}, []string{"gateway_namespace", "gateway_name"})

	// GatewayAdvertised reports per-Gateway advertisement state (1=advertised,
	// 0=withdrawn) labeled by namespace/name.
	GatewayAdvertised = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "gateway_advertised",
		Help:      "Per-Gateway advertisement state (1=advertised, 0=withdrawn/pending-withdraw).",
	}, []string{"gateway_namespace", "gateway_name"})

	// WithdrawalsTotal counts route withdrawals performed by Beacon.
	WithdrawalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "withdrawals_total",
		Help:      "Total number of MetalLB route withdrawals performed by Beacon.",
	}, []string{"gateway_namespace", "gateway_name"})

	// ReadvertisementsTotal counts route re-advertisements performed by Beacon.
	ReadvertisementsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "readvertisements_total",
		Help:      "Total number of MetalLB route re-advertisements performed by Beacon.",
	}, []string{"gateway_namespace", "gateway_name"})

	// ReconcileErrorsTotal counts reconcile errors.
	ReconcileErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_errors_total",
		Help:      "Total number of reconcile errors.",
	})
)

// MustRegister registers all Beacon metrics on the controller-runtime registry.
// Safe to call once at startup.
func MustRegister() {
	ctrlmetrics.Registry.MustRegister(
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
