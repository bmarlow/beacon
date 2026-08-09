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

// Package skupper detects and evaluates the health of backend Services that are
// actually Skupper (skupper.io) Listeners — i.e. the real workload lives on a
// remote cluster reached over a Skupper link.
//
// # Why this is needed
//
// For a normal backend, Beacon traces Service -> EndpointSlices -> Pods and
// checks pod probes. A Skupper Listener, however, creates a local Service whose
// endpoints point at the local skupper-router pod — not the remote workload.
// The router is (locally) always healthy, so a failed remote workload would
// never surface through pod probes.
//
// Skupper does expose the true state: the Listener's status. When a remote
// Connector is available and wired up, the Listener's "Matched"/"Ready"
// conditions are True (status.status == "Ready"). When the remote workload is
// down (e.g. its Connector is gone or scaled to zero), the Listener reports
// "No matching connectors" with Ready=False / status "Pending".
//
// Beacon correlates a backend Service to its Listener via the label
// "internal.skupper.io/listener" that Skupper stamps on the Service it creates,
// and treats the Listener's Ready condition as the backend's health.
package skupper

import (
	"context"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ListenerLabel is stamped by Skupper on the Service it creates for a
	// Listener; its value is the Listener's name (same namespace).
	ListenerLabel = "internal.skupper.io/listener"
)

// ListenerGVK identifies the Skupper Listener kind (v2alpha1).
var ListenerGVK = schema.GroupVersionKind{
	Group: "skupper.io", Version: "v2alpha1", Kind: "Listener",
}

// Health is the evaluated state of a Skupper-backed backend.
type Health struct {
	// IsSkupper is true when the Service is backed by a Skupper Listener.
	IsSkupper bool
	// ListenerName is the correlated Listener (same namespace as the Service).
	ListenerName string
	// Ready reflects the Listener's readiness: a matching remote Connector
	// exists and the link is operational.
	Ready bool
	// Reason is a short human-readable explanation (from the Listener status).
	Reason string
}

// ServiceListenerName returns the Listener name a Service is backed by (via the
// skupper label), and whether the Service is Skupper-backed at all.
func ServiceListenerName(svcLabels map[string]string) (string, bool) {
	name, ok := svcLabels[ListenerLabel]
	return name, ok && name != ""
}

// EvaluateListener reads the Listener in the given namespace and reports its
// health. If the Listener CRD is not installed or the object is missing, it
// returns Ready=false with an explanatory reason (fail-safe: an unresolvable
// remote backend is treated as unhealthy).
func EvaluateListener(ctx context.Context, c client.Client, namespace, name string) Health {
	h := Health{IsSkupper: true, ListenerName: name}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ListenerGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		if apimeta.IsNoMatchError(err) {
			// Skupper CRDs not installed — cannot evaluate; treat as unknown but
			// not failing (so we don't withdraw on clusters without Skupper).
			h.Ready = true
			h.Reason = "skupper Listener CRD not installed; skipped"
			return h
		}
		h.Ready = false
		h.Reason = "skupper Listener not found"
		return h
	}

	// Prefer the top-level status.status == "Ready"; fall back to the Ready
	// condition. "No matching connectors" => remote backend unavailable.
	statusStr, _, _ := unstructured.NestedString(obj.Object, "status", "status")
	msg, _, _ := unstructured.NestedString(obj.Object, "status", "message")

	ready := false
	condReason := ""
	if conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions"); found {
		for _, c := range conds {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			t, _ := cm["type"].(string)
			s, _ := cm["status"].(string)
			if t == "Ready" {
				ready = s == "True"
				condReason, _ = cm["message"].(string)
			}
		}
	}
	// If status.status is explicitly "Ready", trust it.
	if statusStr == "Ready" {
		ready = true
	}

	h.Ready = ready
	if msg != "" {
		h.Reason = "skupper: " + msg
	} else if condReason != "" {
		h.Reason = "skupper: " + condReason
	} else if ready {
		h.Reason = "skupper link ready (matching remote connector)"
	} else {
		h.Reason = "skupper link not ready (no matching remote connector)"
	}
	return h
}
