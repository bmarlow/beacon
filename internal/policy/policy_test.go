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

func TestMinHealthyBackendPercent(t *testing.T) {
	// default when unset
	g := gw("ns", "a", nil, "cls")
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{}
	if got := MinHealthyBackendPercent(g, spec); got != 100 {
		t.Fatalf("expected default 100, got %d", got)
	}
	// policy value
	v := int32(50)
	spec.MinHealthyBackendPercent = &v
	if got := MinHealthyBackendPercent(g, spec); got != 50 {
		t.Fatalf("expected policy 50, got %d", got)
	}
	// annotation override (with % suffix + clamp)
	g2 := gw("ns", "a", map[string]string{OverrideMinHealthyPercentAnnotation: "75%"}, "cls")
	if got := MinHealthyBackendPercent(g2, spec); got != 75 {
		t.Fatalf("expected annotation 75, got %d", got)
	}
	g3 := gw("ns", "a", map[string]string{OverrideMinHealthyPercentAnnotation: "250"}, "cls")
	if got := MinHealthyBackendPercent(g3, spec); got != 100 {
		t.Fatalf("expected clamp to 100, got %d", got)
	}
}

func TestMinHealthyPodPercent(t *testing.T) {
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{}
	// default = 1 (any ready pod)
	g := gw("ns", "a", nil, "cls")
	if got := MinHealthyPodPercent(g, spec); got != 1 {
		t.Fatalf("expected default 1, got %d", got)
	}
	// policy value
	v := int32(50)
	spec.MinHealthyPodPercent = &v
	if got := MinHealthyPodPercent(g, spec); got != 50 {
		t.Fatalf("expected policy 50, got %d", got)
	}
	// gateway annotation override
	g2 := gw("ns", "a", map[string]string{OverrideMinHealthyPodPercentAnnotation: "100"}, "cls")
	if got := MinHealthyPodPercent(g2, spec); got != 100 {
		t.Fatalf("expected gateway annotation 100, got %d", got)
	}
}

func TestServiceMinHealthyPodPercent_Precedence(t *testing.T) {
	gatewayLevel := int32(50)
	// no service annotation -> gateway level
	if got := ServiceMinHealthyPodPercent(nil, gatewayLevel); got != 50 {
		t.Fatalf("expected gateway level 50, got %d", got)
	}
	// service annotation wins
	svcAnn := map[string]string{OverrideMinHealthyPodPercentAnnotation: "100"}
	if got := ServiceMinHealthyPodPercent(svcAnn, gatewayLevel); got != 100 {
		t.Fatalf("expected service override 100, got %d", got)
	}
}

func TestZeroReplicasPolicy(t *testing.T) {
	spec := &beaconv1alpha1.GatewayHealthPolicySpec{}
	// default = Unhealthy
	g := gw("ns", "a", nil, "cls")
	if got := ZeroReplicasPolicy(g, spec); got != beaconv1alpha1.ZeroReplicasUnhealthy {
		t.Fatalf("expected default Unhealthy, got %q", got)
	}
	// policy value
	spec.ZeroReplicasPolicy = beaconv1alpha1.ZeroReplicasExempt
	if got := ZeroReplicasPolicy(g, spec); got != beaconv1alpha1.ZeroReplicasExempt {
		t.Fatalf("expected policy Exempt, got %q", got)
	}
	// gateway annotation override (case-insensitive)
	g2 := gw("ns", "a", map[string]string{OverrideZeroReplicasPolicyAnnotation: "unhealthy"}, "cls")
	if got := ZeroReplicasPolicy(g2, spec); got != beaconv1alpha1.ZeroReplicasUnhealthy {
		t.Fatalf("expected gateway annotation Unhealthy, got %q", got)
	}
	// invalid annotation falls back to policy value
	g3 := gw("ns", "a", map[string]string{OverrideZeroReplicasPolicyAnnotation: "bogus"}, "cls")
	if got := ZeroReplicasPolicy(g3, spec); got != beaconv1alpha1.ZeroReplicasExempt {
		t.Fatalf("expected fallback to policy Exempt, got %q", got)
	}
}

func TestServiceZeroReplicasPolicy_Precedence(t *testing.T) {
	gatewayLevel := beaconv1alpha1.ZeroReplicasExempt
	// no service annotation -> gateway level
	if got := ServiceZeroReplicasPolicy(nil, gatewayLevel); got != beaconv1alpha1.ZeroReplicasExempt {
		t.Fatalf("expected gateway level Exempt, got %q", got)
	}
	// service annotation wins
	svcAnn := map[string]string{OverrideZeroReplicasPolicyAnnotation: "Unhealthy"}
	if got := ServiceZeroReplicasPolicy(svcAnn, gatewayLevel); got != beaconv1alpha1.ZeroReplicasUnhealthy {
		t.Fatalf("expected service override Unhealthy, got %q", got)
	}
}
