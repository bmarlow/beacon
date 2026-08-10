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

// Package monitoring provisions the metrics Service, ServiceMonitor, and
// PrometheusRule at operator startup so Beacon's metrics are wired into
// OpenShift (user-workload) monitoring. OLM CSVs cannot ship arbitrary objects,
// so the operator creates them itself (leader-elected).
package monitoring

import (
	"context"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	metricsServiceName  = "beacon-metrics"
	metricsTLSSecret    = "beacon-metrics-tls"
	serviceMonitorName  = "beacon-controller-manager"
	prometheusRuleName  = "beacon-alerts"
	operatorDeployment  = "beacon-controller-manager"
	controlPlaneLabel   = "control-plane"
	controlPlaneValue   = "controller-manager"
	podNamespaceEnv     = "POD_NAMESPACE"
	saNamespaceFilePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

var (
	serviceMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	prometheusRuleGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"}
)

// Manager creates and owns Beacon's monitoring resources. Leader-elected so
// only the active replica reconciles them.
type Manager struct {
	Client      client.Client
	MetricsPort int32
}

// NewManager constructs a monitoring Manager.
func NewManager(c client.Client, metricsPort int32) *Manager {
	return &Manager{Client: c, MetricsPort: metricsPort}
}

// NeedLeaderElection ensures only the leader provisions these.
func (m *Manager) NeedLeaderElection() bool { return true }

// Start ensures the metrics Service, ServiceMonitor, and PrometheusRule exist.
func (m *Manager) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("monitoring")

	ns, err := operatorNamespace()
	if err != nil {
		logger.Info("could not determine operator namespace; skipping monitoring resources", "error", err.Error())
		return nil
	}
	owner := m.ownerRef(ctx, ns)

	if err := m.ensureMetricsService(ctx, ns, owner); err != nil {
		logger.Error(err, "failed ensuring metrics Service")
	} else {
		logger.Info("ensured metrics Service", "name", metricsServiceName)
	}

	if !m.prometheusOperatorInstalled() {
		logger.Info("Prometheus Operator CRDs not present; skipping ServiceMonitor/PrometheusRule")
		return nil
	}
	if err := m.ensureServiceMonitor(ctx, ns, owner); err != nil {
		logger.Error(err, "failed ensuring ServiceMonitor")
	} else {
		logger.Info("ensured ServiceMonitor", "name", serviceMonitorName)
	}
	if err := m.ensurePrometheusRule(ctx, ns, owner); err != nil {
		logger.Error(err, "failed ensuring PrometheusRule")
	} else {
		logger.Info("ensured PrometheusRule", "name", prometheusRuleName)
	}
	return nil
}

func operatorNamespace() (string, error) {
	if v := os.Getenv(podNamespaceEnv); v != "" {
		return v, nil
	}
	data, err := os.ReadFile(saNamespaceFilePath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", saNamespaceFilePath, err)
	}
	ns := strings.TrimSpace(string(data))
	if ns == "" {
		return "", fmt.Errorf("empty namespace")
	}
	return ns, nil
}

func (m *Manager) ownerRef(ctx context.Context, ns string) *metav1.OwnerReference {
	dep := &appsv1.Deployment{}
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: operatorDeployment}, dep); err != nil {
		return nil
	}
	return &metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID}
}

func labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "beacon",
		"app.kubernetes.io/component":  "metrics",
		"app.kubernetes.io/managed-by": "beacon-operator",
		controlPlaneLabel:              controlPlaneValue,
	}
}

func (m *Manager) ensureMetricsService(ctx context.Context, ns string, owner *metav1.OwnerReference) error {
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      metricsServiceName,
			Namespace: ns,
			Labels:    labels(),
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": metricsTLSSecret,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{controlPlaneLabel: controlPlaneValue},
			Ports: []corev1.ServicePort{{
				Name:       "https",
				Port:       m.MetricsPort,
				TargetPort: intstr.FromInt32(m.MetricsPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if owner != nil {
		desired.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	existing := &corev1.Service{}
	err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: metricsServiceName}, existing)
	if apierrors.IsNotFound(err) {
		return m.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range desired.Annotations {
		existing.Annotations[k] = v
	}
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	if owner != nil && len(existing.OwnerReferences) == 0 {
		existing.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return m.Client.Update(ctx, existing)
}

func (m *Manager) prometheusOperatorInstalled() bool {
	mappings, err := m.Client.RESTMapper().RESTMappings(serviceMonitorGVK.GroupKind(), serviceMonitorGVK.Version)
	return err == nil && len(mappings) > 0
}

func (m *Manager) ensureServiceMonitor(ctx context.Context, ns string, owner *metav1.OwnerReference) error {
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(serviceMonitorName)
	sm.SetNamespace(ns)
	sm.SetLabels(labels())
	if owner != nil {
		sm.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	spec := map[string]interface{}{
		"selector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"app.kubernetes.io/component": "metrics",
				controlPlaneLabel:             controlPlaneValue,
			},
		},
		"endpoints": []interface{}{
			map[string]interface{}{
				"port":            "https",
				"scheme":          "https",
				"path":            "/metrics",
				"interval":        "30s",
				"bearerTokenFile": "/var/run/secrets/kubernetes.io/serviceaccount/token",
				"tlsConfig": map[string]interface{}{
					"caFile":     "/etc/prometheus/configmaps/serving-certs-ca-bundle/service-ca.crt",
					"serverName": fmt.Sprintf("%s.%s.svc", metricsServiceName, ns),
				},
			},
		},
	}
	if err := unstructured.SetNestedMap(sm.Object, spec, "spec"); err != nil {
		return err
	}
	return m.applyUnstructured(ctx, sm)
}

func (m *Manager) ensurePrometheusRule(ctx context.Context, ns string, owner *metav1.OwnerReference) error {
	pr := &unstructured.Unstructured{}
	pr.SetGroupVersionKind(prometheusRuleGVK)
	pr.SetName(prometheusRuleName)
	pr.SetNamespace(ns)
	pr.SetLabels(labels())
	if owner != nil {
		pr.SetOwnerReferences([]metav1.OwnerReference{*owner})
	}
	rule := func(alert, expr, forDur, sev, summary, desc string) map[string]interface{} {
		return map[string]interface{}{
			"alert": alert, "expr": expr, "for": forDur,
			"labels":      map[string]interface{}{"severity": sev},
			"annotations": map[string]interface{}{"summary": summary, "description": desc},
		}
	}
	spec := map[string]interface{}{
		"groups": []interface{}{
			map[string]interface{}{
				"name": "beacon.rules",
				"rules": []interface{}{
					rule("BeaconGatewayVIPWithdrawn", "beacon_gateway_advertised == 0", "2m", "warning",
						"Beacon has withdrawn a Gateway VIP",
						"Beacon has withdrawn the MetalLB advertisement for Gateway {{ $labels.gateway_namespace }}/{{ $labels.gateway_name }} for at least 2 minutes."),
					rule("BeaconGatewayUnhealthy", "beacon_gateway_healthy == 0", "5m", "warning",
						"Beacon Gateway is unhealthy",
						"Gateway {{ $labels.gateway_namespace }}/{{ $labels.gateway_name }} has been unhealthy for at least 5 minutes."),
					rule("BeaconManyWithdrawnIPs",
						"beacon_withdrawn_ips > 0 and (beacon_withdrawn_ips / clamp_min(beacon_managed_gateways, 1)) >= 0.5",
						"5m", "critical",
						"Half or more of Beacon-managed Gateway VIPs are withdrawn",
						"A large-scale backend outage may be in progress."),
					rule("BeaconReconcileErrors", "rate(beacon_reconcile_errors_total[5m]) > 0", "15m", "warning",
						"Beacon is experiencing reconcile errors",
						"The Beacon controller has been logging reconcile errors for at least 15 minutes."),
					rule("BeaconControllerDown", `up{job="beacon-metrics"} == 0`, "10m", "critical",
						"Beacon controller-manager metrics endpoint is down",
						"Prometheus has not scraped the Beacon controller-manager for at least 10 minutes."),
				},
			},
		},
	}
	if err := unstructured.SetNestedMap(pr.Object, spec, "spec"); err != nil {
		return err
	}
	return m.applyUnstructured(ctx, pr)
}

func (m *Manager) applyUnstructured(ctx context.Context, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	err := m.Client.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		return m.Client.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return m.Client.Update(ctx, obj)
}

// AddToManager registers the monitoring Manager as a manager Runnable.
func (m *Manager) AddToManager(mgr manager.Manager) error {
	return mgr.Add(m)
}
