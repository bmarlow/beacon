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

package policy

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
)

func gw(ns, name string, ann map[string]string, class string) *gwapiv1.Gateway {
	return &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: ann},
		Spec:       gwapiv1.GatewaySpec{GatewayClassName: gwapiv1.ObjectName(class)},
	}
}

func TestIsExempt_Annotation(t *testing.T) {
	g := gw("ns", "a", map[string]string{ExemptAnnotation: "true"}, "cls")
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{}
	if !IsExempt(g, spec) {
		t.Fatal("expected exempt by annotation")
	}
}

func TestIsExempt_CentralList(t *testing.T) {
	g := gw("ns", "a", nil, "cls")
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{
		Exemptions: []beaconv1alpha1.GatewayReference{{Namespace: "ns", Name: "a"}},
	}
	if !IsExempt(g, spec) {
		t.Fatal("expected exempt by central list")
	}
}

func TestClassAllowed(t *testing.T) {
	g := gw("ns", "a", nil, "istio")
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{GatewayClassNames: []string{"istio"}}
	if !ClassAllowed(g, spec) {
		t.Fatal("expected class allowed")
	}
	spec.GatewayClassNames = []string{"nginx"}
	if ClassAllowed(g, spec) {
		t.Fatal("expected class filtered out")
	}
	spec.GatewayClassNames = nil
	if !ClassAllowed(g, spec) {
		t.Fatal("empty filter should allow all")
	}
}

func TestWithdrawAfter_Override(t *testing.T) {
	g := gw("ns", "a", map[string]string{OverrideWithdrawAfterAnnotation: "12s"}, "cls")
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{
		WithdrawAfter: metav1.Duration{Duration: 5 * time.Second},
	}
	if got := WithdrawAfter(g, spec).Duration; got != 12*time.Second {
		t.Fatalf("expected 12s override, got %s", got)
	}
	g2 := gw("ns", "a", nil, "cls")
	if got := WithdrawAfter(g2, spec).Duration; got != 5*time.Second {
		t.Fatalf("expected 5s default, got %s", got)
	}
}
