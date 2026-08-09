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

package skupper

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func schemeWithListener() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(ListenerGVK, &unstructured.Unstructured{})
	lst := ListenerGVK
	lst.Kind = "ListenerList"
	s.AddKnownTypeWithName(lst, &unstructured.UnstructuredList{})
	return s
}

func listener(ns, name, status, msg string, ready bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ListenerGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	rs := "False"
	if ready {
		rs = "True"
	}
	_ = unstructured.SetNestedField(u.Object, status, "status", "status")
	_ = unstructured.SetNestedField(u.Object, msg, "status", "message")
	_ = unstructured.SetNestedSlice(u.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": rs, "message": msg},
	}, "status", "conditions")
	return u
}

func TestEvaluateListener_Ready(t *testing.T) {
	s := schemeWithListener()
	l := listener("ns", "app", "Ready", "OK", true)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(l).Build()
	h := EvaluateListener(context.Background(), cl, "ns", "app")
	if !h.Ready {
		t.Fatalf("expected ready, got %+v", h)
	}
}

func TestEvaluateListener_NoMatchingConnector(t *testing.T) {
	s := schemeWithListener()
	l := listener("ns", "app", "Pending", "No matching connectors", false)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(l).Build()
	h := EvaluateListener(context.Background(), cl, "ns", "app")
	if h.Ready {
		t.Fatalf("expected NOT ready (no matching connectors), got %+v", h)
	}
	if h.Reason == "" {
		t.Fatal("expected a reason")
	}
}

func TestEvaluateListener_Missing(t *testing.T) {
	s := schemeWithListener()
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	h := EvaluateListener(context.Background(), cl, "ns", "gone")
	if h.Ready {
		t.Fatal("missing listener should be treated as not ready")
	}
}

func TestServiceListenerName(t *testing.T) {
	if n, ok := ServiceListenerName(map[string]string{ListenerLabel: "beta"}); !ok || n != "beta" {
		t.Fatalf("expected beta, got %q ok=%v", n, ok)
	}
	if _, ok := ServiceListenerName(map[string]string{"other": "x"}); ok {
		t.Fatal("expected not skupper-backed")
	}
}
