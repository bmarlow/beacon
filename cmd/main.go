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

package main

import (
	"crypto/tls"
	"flag"
	"net"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
	"github.com/bmarlow/beacon/internal/controller"
	"github.com/bmarlow/beacon/internal/export"
	"github.com/bmarlow/beacon/internal/metallb"
	"github.com/bmarlow/beacon/internal/metrics"
	"github.com/bmarlow/beacon/internal/monitoring"
	"github.com/bmarlow/beacon/internal/podnamespace"
	"github.com/bmarlow/beacon/internal/state"
	"github.com/bmarlow/beacon/internal/webui"
	// +kubebuilder:scaffold:imports
)

var setupLog = ctrl.Log.WithName("setup")

func main() {
	runtimeScheme := clientgoscheme.Scheme
	utilruntime.Must(clientgoscheme.AddToScheme(runtimeScheme))
	utilruntime.Must(beaconv1alpha1.AddToScheme(runtimeScheme))
	utilruntime.Must(gwapiv1.Install(runtimeScheme))
	utilruntime.Must(gwapiv1alpha2.Install(runtimeScheme))
	utilruntime.Must(metallb.AddToScheme(runtimeScheme))
	utilruntime.Must(corev1.AddToScheme(runtimeScheme))
	utilruntime.Must(discoveryv1.AddToScheme(runtimeScheme))
	// +kubebuilder:scaffold:scheme

	var metricsAddr string
	var metricsCertDir string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var policyName string
	var dashboardAddr string
	var dashboardAuthRequired bool
	var exportAddr string
	var exportCertDir string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "The address the metrics endpoint binds to.")
	flag.StringVar(&metricsCertDir, "metrics-cert-dir", "",
		"Directory containing tls.crt/tls.key for the metrics server (e.g. an "+
			"OpenShift service-serving cert). When empty, a self-signed cert is used.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS.")
	flag.StringVar(&policyName, "policy-name", "cluster",
		"Name of the singleton GatewayHealthPolicy resource to load configuration from.")
	flag.StringVar(&dashboardAddr, "dashboard-bind-address", ":8082",
		"The address the topology dashboard (web UI) binds to. Set empty to disable.")
	flag.BoolVar(&dashboardAuthRequired, "dashboard-auth-required", true,
		"Require authentication (via an OpenShift oauth-proxy front-end) and enforce "+
			"per-user RBAC on the dashboard. When true, the dashboard only serves the "+
			"forwarded-user's identity and filters resources by SubjectAccessReview.")
	flag.StringVar(&exportAddr, "export-bind-address", "",
		"The address the multi-cluster summary export endpoint (GET /api/v1/export/summary) "+
			"binds to, e.g. \":8083\". Empty (default) disables it. Intended for a hub cluster "+
			"to poll cheap, cluster-level status; see internal/export.")
	flag.StringVar(&exportCertDir, "export-cert-dir", "",
		"Directory containing tls.crt/tls.key for the export endpoint (e.g. an OpenShift "+
			"service-serving cert mount). When empty, an ephemeral self-signed certificate is "+
			"generated at startup.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Disable HTTP/2 by default to mitigate Rapid Reset / HPACK CVEs, per
	// Red Hat operator hardening guidance.
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	metricsOpts := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       []func(*tls.Config){disableHTTP2},
	}
	// When a metrics serving-cert directory is provided (OpenShift mounts a
	// service-serving cert here), use it so the ServiceMonitor can verify the
	// endpoint against the cluster's service-ca bundle. Otherwise
	// controller-runtime self-signs.
	if metricsCertDir != "" {
		metricsOpts.CertDir = metricsCertDir
		metricsOpts.CertName = "tls.crt"
		metricsOpts.KeyName = "tls.key"
	}

	// Secrets and ServiceAccounts are only ever read/written in the operator's
	// own namespace (the dashboard's oauth-proxy cookie Secret and the
	// operator's own ServiceAccount OAuth-redirect annotation — see
	// internal/webui/dashboard_resources.go). RBAC for both is a namespaced
	// Role, not a cluster-wide ClusterRole (see config/rbac/dashboard_role.yaml
	// and the CSV's "permissions" block), so the cache's informers for these
	// two types must be scoped to that same namespace — an unscoped,
	// cluster-wide List/Watch would otherwise be denied and the cache would
	// never sync. When the namespace can't be determined (e.g. running
	// locally outside a cluster), fall back to an unscoped cache; the
	// dashboard/monitoring resource managers already degrade gracefully in
	// that case (they skip their own namespace-scoped work and log why).
	cacheOpts := cache.Options{}
	if ns, err := podnamespace.Get(); err == nil {
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&corev1.Secret{}:         {Namespaces: map[string]cache.Config{ns: {}}},
			&corev1.ServiceAccount{}: {Namespaces: map[string]cache.Config{ns: {}}},
		}
	} else {
		setupLog.Info("could not determine operator namespace; caching Secrets/ServiceAccounts cluster-wide",
			"error", err.Error())
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  runtimeScheme,
		Metrics: metricsOpts,
		Cache:   cacheOpts,
		WebhookServer: webhook.NewServer(webhook.Options{
			TLSOpts: []func(*tls.Config){disableHTTP2},
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "beacon.beacon.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Shared state store: the controller publishes live advertisement/health
	// decisions; the web UI reads them to annotate the topology graph.
	// Register Beacon domain metrics on the controller-runtime registry.
	metrics.MustRegister()

	stateStore := state.New()

	if err := (&controller.GatewayReconciler{
		Client:     mgr.GetClient(),
		Recorder:   mgr.GetEventRecorderFor("beacon"),
		PolicyName: policyName,
		States:     stateStore,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Gateway")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Republishes gauge metrics from the shared GatewayHealthPolicy status on
	// every replica (not just the leader, which is the only one running the
	// reconcile loop above) — see MetricsReporter's doc comment for why this
	// matters once the operator runs with >1 replica.
	if err := (&controller.MetricsReporter{
		Client:     mgr.GetClient(),
		PolicyName: policyName,
	}).AddToManager(mgr); err != nil {
		setupLog.Error(err, "unable to register metrics reporter")
		os.Exit(1)
	}

	// Topology dashboard (read-only web UI). Runs on every replica.
	if dashboardAddr != "" {
		dash := webui.NewServer(dashboardAddr, mgr.GetClient(), stateStore, policyName, dashboardAuthRequired)
		if err := dash.AddToManager(mgr); err != nil {
			setupLog.Error(err, "unable to register topology dashboard")
			os.Exit(1)
		}
		// The operator owns the dashboard Service and (on OpenShift) Route,
		// creating them at startup. Leader-elected so only one replica acts.
		port := dashboardPortFromAddr(dashboardAddr)
		const oauthProxyPort = int32(9443)
		rm := webui.NewResourceManager(mgr.GetClient(), port, dashboardAuthRequired, oauthProxyPort)
		if err := rm.AddToManager(mgr); err != nil {
			setupLog.Error(err, "unable to register dashboard resource manager")
			os.Exit(1)
		}
	}

	// Multi-cluster summary export endpoint (opt-in; disabled unless
	// --export-bind-address is set). Runs on every replica (read-only).
	if exportAddr != "" {
		exp := &export.Server{
			Addr:       exportAddr,
			Client:     mgr.GetClient(),
			PolicyName: policyName,
			CertDir:    exportCertDir,
		}
		if err := exp.AddToManager(mgr); err != nil {
			setupLog.Error(err, "unable to register multi-cluster export endpoint")
			os.Exit(1)
		}
	}

	// Wire metrics into OpenShift monitoring: the operator creates the metrics
	// Service, ServiceMonitor, and PrometheusRule at startup (leader-elected).
	if mp := dashboardPortFromAddr(metricsAddr); mp > 0 {
		mon := monitoring.NewManager(mgr.GetClient(), mp)
		if err := mon.AddToManager(mgr); err != nil {
			setupLog.Error(err, "unable to register monitoring resource manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// dashboardPortFromAddr parses the port from a bind address like ":8082" or
// "0.0.0.0:8082". Defaults to 8082 on parse failure.
func dashboardPortFromAddr(addr string) int32 {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 8082
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return 8082
	}
	return int32(p)
}
