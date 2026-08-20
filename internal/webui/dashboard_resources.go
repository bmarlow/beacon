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

package webui

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/beacon-operator/beacon/internal/podnamespace"
)

const (
	dashboardName      = "beacon-dashboard"
	dashboardPortName  = "dashboard"
	controlPlaneLabel  = "control-plane"
	controlPlaneValue  = "controller-manager"
	operatorDeployment = "beacon-controller-manager"
)

var (
	routeGVK       = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
	consoleLinkGVK = schema.GroupVersionKind{Group: "console.openshift.io", Version: "v1", Kind: "ConsoleLink"}
)

const consoleLinkName = "beacon-dashboard"

// ResourceManager creates and owns the dashboard Service and (on OpenShift) the
// Route at operator startup, so these are lifecycle-managed by the operator
// rather than applied out-of-band. It runs as a leader-elected Runnable so only
// the active replica reconciles them.
//
// When AuthEnabled is true, the dashboard is fronted by an OpenShift oauth-proxy
// sidecar (in the operator Deployment) listening on ProxyPort with TLS. In that
// mode the ResourceManager also:
//   - ensures the oauth-proxy cookie Secret exists,
//   - annotates the Service for a service-serving certificate,
//   - annotates the ServiceAccount with an OAuth redirect reference to the Route,
//   - makes the Route reencrypt to the proxy port.
type ResourceManager struct {
	Client client.Client
	// DashboardPort is the plaintext port the manager serves the UI on
	// (used when AuthEnabled is false, or as the proxy upstream otherwise).
	DashboardPort int32
	// AuthEnabled indicates the oauth-proxy front-end is in use.
	AuthEnabled bool
	// ProxyPort is the oauth-proxy HTTPS port (Service/Route target when
	// AuthEnabled).
	ProxyPort int32
}

// NewResourceManager constructs a ResourceManager.
func NewResourceManager(c client.Client, dashboardPort int32, authEnabled bool, proxyPort int32) *ResourceManager {
	return &ResourceManager{Client: c, DashboardPort: dashboardPort, AuthEnabled: authEnabled, ProxyPort: proxyPort}
}

const (
	tlsSecretName    = "beacon-dashboard-tls"
	cookieSecretName = "beacon-dashboard-proxy"
)

// servicePort returns the port the Service/Route should target.
func (m *ResourceManager) servicePort() int32 {
	if m.AuthEnabled {
		return m.ProxyPort
	}
	return m.DashboardPort
}

// NeedLeaderElection ensures only the leader creates/updates these resources.
func (m *ResourceManager) NeedLeaderElection() bool { return true }

// Start runs once when this replica becomes leader: it ensures the Service and
// Route exist, then returns (the resources are static and owner-referenced, so
// no ongoing reconcile loop is needed).
func (m *ResourceManager) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("dashboard-resources")

	ns, err := podnamespace.Get()
	if err != nil {
		logger.Info("could not determine operator namespace; skipping dashboard resource management", "error", err.Error())
		return nil
	}

	owner := m.ownerRef(ctx, ns) // best-effort; nil if not found

	if m.AuthEnabled {
		// The oauth-proxy sidecar needs a cookie secret and the operator SA
		// annotated as an OAuth client with a redirect reference to the Route.
		if err := m.ensureCookieSecret(ctx, ns, owner); err != nil {
			logger.Error(err, "failed ensuring oauth-proxy cookie Secret")
		} else {
			logger.Info("ensured oauth-proxy cookie Secret", "name", cookieSecretName)
		}
		if err := m.ensureSAOAuthRedirect(ctx, ns); err != nil {
			logger.Error(err, "failed annotating ServiceAccount for OAuth redirect")
		} else {
			logger.Info("annotated ServiceAccount OAuth redirect reference")
		}
	}

	if err := m.ensureService(ctx, ns, owner); err != nil {
		logger.Error(err, "failed ensuring dashboard Service")
	} else {
		logger.Info("ensured dashboard Service", "namespace", ns, "name", dashboardName)
	}

	if m.routeInstalled() {
		if err := m.ensureRoute(ctx, ns); err != nil {
			logger.Error(err, "failed ensuring dashboard Route")
		} else {
			logger.Info("ensured dashboard Route", "namespace", ns, "name", dashboardName)
		}

		// Publish a ConsoleLink in the console's Application Launcher (the grid
		// menu, top-right of every console page) so the dashboard is easy to
		// find. Requires the Route host to be admitted.
		if host := m.routeHost(ctx, ns); host != "" && m.consoleLinkInstalled() {
			if err := m.ensureConsoleLink(ctx, "https://"+host+"/"); err != nil {
				logger.Error(err, "failed ensuring dashboard ConsoleLink")
			} else {
				logger.Info("ensured dashboard ConsoleLink", "name", consoleLinkName)
			}
		}
	} else {
		logger.Info("Route CRD not present; skipping dashboard Route (non-OpenShift cluster)")
	}
	return nil
}

// ownerRef returns an OwnerReference to the operator's own Deployment so the
// Service/Route are garbage-collected when the operator is uninstalled. Returns
// nil if the Deployment can't be found (resources are still created, just not
// owner-referenced).
func (m *ResourceManager) ownerRef(ctx context.Context, ns string) *metav1.OwnerReference {
	dep := &appsv1.Deployment{}
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: operatorDeployment}, dep); err != nil {
		return nil
	}
	// Note: we intentionally do NOT set Controller/BlockOwnerDeletion. Setting
	// those would require the operator to have update on the owner's
	// finalizers subresource. A plain (non-controller) ownerRef only requires
	// delete permission on the owner type, which we grant for deployments.
	return &metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       dep.Name,
		UID:        dep.UID,
	}
}

func dashboardLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "beacon",
		"app.kubernetes.io/component":  "dashboard",
		"app.kubernetes.io/managed-by": "beacon-operator",
	}
}

func (m *ResourceManager) ensureService(ctx context.Context, ns string, owner *metav1.OwnerReference) error {
	annotations := map[string]string{}
	if m.AuthEnabled {
		// Ask the service-ca operator to mint TLS for the oauth-proxy.
		annotations["service.beta.openshift.io/serving-cert-secret-name"] = tlsSecretName
	}
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        dashboardName,
			Namespace:   ns,
			Labels:      dashboardLabels(),
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{controlPlaneLabel: controlPlaneValue},
			Ports: []corev1.ServicePort{{
				Name:       dashboardPortName,
				Port:       m.servicePort(),
				TargetPort: intstr.FromString(dashboardPortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if owner != nil {
		desired.OwnerReferences = []metav1.OwnerReference{*owner}
	}

	existing := &corev1.Service{}
	err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: dashboardName}, existing)
	if apierrors.IsNotFound(err) {
		return m.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Update mutable fields, preserving the cluster-assigned ClusterIP.
	existing.Labels = desired.Labels
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		existing.Annotations[k] = v
	}
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	if owner != nil && len(existing.OwnerReferences) == 0 {
		existing.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return m.Client.Update(ctx, existing)
}

func (m *ResourceManager) routeInstalled() bool {
	mappings, err := m.Client.RESTMapper().RESTMappings(routeGVK.GroupKind(), routeGVK.Version)
	return err == nil && len(mappings) > 0
}

func (m *ResourceManager) ensureRoute(ctx context.Context, ns string) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK)
	key := types.NamespacedName{Namespace: ns, Name: dashboardName}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(routeGVK)
	err := m.Client.Get(ctx, key, existing)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) && apimeta.IsNoMatchError(err) {
		return nil // CRD vanished between check and get; treat as non-OpenShift
	}
	if err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		return err
	}

	route.SetName(dashboardName)
	route.SetNamespace(ns)
	route.SetLabels(dashboardLabels())
	route.SetAnnotations(map[string]string{"haproxy.router.openshift.io/timeout": "30s"})
	// NOTE: We intentionally do NOT set an owner reference on the Route. The
	// OpenShift route.openshift.io admission (OwnerReferencesPermissionEnforcement)
	// rejects setting a cross-group ownerRef to the operator's Deployment even
	// with delete permission on deployments. The Route is idempotently
	// reconciled on startup; on uninstall it is a harmless orphan (its target
	// Service is owner-referenced and IS garbage-collected).
	spec := map[string]interface{}{
		"to": map[string]interface{}{
			"kind":   "Service",
			"name":   dashboardName,
			"weight": int64(100),
		},
		"port": map[string]interface{}{
			"targetPort": dashboardPortName,
		},
		"tls":            m.routeTLS(),
		"wildcardPolicy": "None",
	}
	if err := unstructured.SetNestedMap(route.Object, spec, "spec"); err != nil {
		return err
	}

	if !exists {
		return m.Client.Create(ctx, route)
	}
	// Preserve the existing resourceVersion for update.
	route.SetResourceVersion(existing.GetResourceVersion())
	return m.Client.Update(ctx, route)
}

// routeTLS returns the Route TLS block. With auth (oauth-proxy) the backend
// terminates TLS with a service-serving cert, so the Route must reencrypt.
// Without auth the plaintext dashboard is edge-terminated at the router.
func (m *ResourceManager) routeTLS() map[string]interface{} {
	if m.AuthEnabled {
		return map[string]interface{}{
			"termination":                   "reencrypt",
			"insecureEdgeTerminationPolicy": "Redirect",
		}
	}
	return map[string]interface{}{
		"termination":                   "edge",
		"insecureEdgeTerminationPolicy": "Redirect",
	}
}

// routeHost returns the admitted host of the dashboard Route, or "".
func (m *ResourceManager) routeHost(ctx context.Context, ns string) string {
	rt := &unstructured.Unstructured{}
	rt.SetGroupVersionKind(routeGVK)
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: dashboardName}, rt); err != nil {
		return ""
	}
	if host, found, _ := unstructured.NestedString(rt.Object, "spec", "host"); found {
		return host
	}
	return ""
}

func (m *ResourceManager) consoleLinkInstalled() bool {
	mappings, err := m.Client.RESTMapper().RESTMappings(consoleLinkGVK.GroupKind(), consoleLinkGVK.Version)
	return err == nil && len(mappings) > 0
}

// ensureConsoleLink creates/updates a cluster-scoped ConsoleLink that adds the
// Beacon dashboard to the OpenShift console's Application Launcher (grid menu).
func (m *ResourceManager) ensureConsoleLink(ctx context.Context, href string) error {
	link := &unstructured.Unstructured{}
	link.SetGroupVersionKind(consoleLinkGVK)
	link.SetName(consoleLinkName)
	link.SetLabels(dashboardLabels())
	spec := map[string]interface{}{
		"href":     href,
		"text":     "Beacon Dashboard",
		"location": "ApplicationMenu",
		"applicationMenu": map[string]interface{}{
			"section": "Observability",
		},
	}
	if err := unstructured.SetNestedMap(link.Object, spec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(consoleLinkGVK)
	err := m.Client.Get(ctx, types.NamespacedName{Name: consoleLinkName}, existing)
	if apierrors.IsNotFound(err) {
		return m.Client.Create(ctx, link)
	}
	if err != nil {
		return err
	}
	link.SetResourceVersion(existing.GetResourceVersion())
	return m.Client.Update(ctx, link)
}

// ensureCookieSecret creates the oauth-proxy session cookie Secret if absent
// (a random 32-byte value). It is not overwritten on subsequent runs so proxy
// sessions survive operator restarts.
func (m *ResourceManager) ensureCookieSecret(ctx context.Context, ns string, owner *metav1.OwnerReference) error {
	existing := &corev1.Secret{}
	err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: cookieSecretName}, existing)
	if err == nil {
		return nil // keep existing session secret
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cookieSecretName,
			Namespace: ns,
			Labels:    dashboardLabels(),
		},
		Data: map[string][]byte{
			"session_secret": []byte(base64.StdEncoding.EncodeToString(buf)),
		},
	}
	if owner != nil {
		sec.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return m.Client.Create(ctx, sec)
}

// ensureSAOAuthRedirect annotates the operator ServiceAccount so the OpenShift
// OAuth server treats it as an OAuth client whose allowed redirect is the
// dashboard Route. This is what lets oauth-proxy --provider=openshift complete
// the login flow.
func (m *ResourceManager) ensureSAOAuthRedirect(ctx context.Context, ns string) error {
	sa := &corev1.ServiceAccount{}
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: operatorDeployment}, sa); err != nil {
		return err
	}
	if sa.Annotations == nil {
		sa.Annotations = map[string]string{}
	}
	// Reference the Route by name so the redirect URI tracks the admitted host.
	const key = "serviceaccounts.openshift.io/oauth-redirectreference.primary"
	const val = `{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"beacon-dashboard"}}`
	if sa.Annotations[key] == val {
		return nil
	}
	sa.Annotations[key] = val
	return m.Client.Update(ctx, sa)
}

// AddToManager registers the ResourceManager as a manager Runnable.
func (m *ResourceManager) AddToManager(mgr manager.Manager) error {
	return mgr.Add(m)
}
