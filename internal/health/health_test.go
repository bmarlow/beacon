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

package health

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWithProbe(name string, ready bool) corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:           "c",
				ReadinessProbe: &corev1.Probe{},
			}},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func podNoProbe(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c"}},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
}

func TestPodHasProbes(t *testing.T) {
	p := podWithProbe("a", true)
	if !PodHasProbes(&p) {
		t.Fatal("expected pod to have probes")
	}
	np := podNoProbe("b")
	if PodHasProbes(&np) {
		t.Fatal("expected pod to have no probes")
	}
}

func TestEvaluate_AllExempt(t *testing.T) {
	res := Evaluate([]corev1.Pod{podNoProbe("a"), podNoProbe("b")})
	if !res.AllExempt() {
		t.Fatalf("expected all exempt, got %+v", res)
	}
	if res.Healthy() {
		t.Fatal("all-exempt should not be healthy")
	}
}

func TestEvaluate_Healthy(t *testing.T) {
	res := Evaluate([]corev1.Pod{podWithProbe("a", true), podNoProbe("b")})
	if !res.Healthy() {
		t.Fatalf("expected healthy, got %+v", res)
	}
	if res.UnhealthyPods != 0 {
		t.Fatalf("expected 0 unhealthy, got %d", res.UnhealthyPods)
	}
}

func TestEvaluate_Unhealthy(t *testing.T) {
	res := Evaluate([]corev1.Pod{podWithProbe("a", true), podWithProbe("b", false)})
	if res.Healthy() {
		t.Fatal("expected unhealthy")
	}
	if res.UnhealthyPods != 1 {
		t.Fatalf("expected 1 unhealthy, got %d", res.UnhealthyPods)
	}
}

func TestEvaluateService_ScaledToZero(t *testing.T) {
	tests := []struct {
		name                  string
		pods                  []corev1.Pod
		hasSelector           bool
		zeroReplicasUnhealthy bool
		wantCounted           bool
		wantHealthy           bool
	}{
		{
			name:                  "scaled to zero, selector, unhealthy policy -> counted+down",
			pods:                  nil,
			hasSelector:           true,
			zeroReplicasUnhealthy: true,
			wantCounted:           true,
			wantHealthy:           false,
		},
		{
			name:                  "scaled to zero, selector, exempt policy -> not counted",
			pods:                  nil,
			hasSelector:           true,
			zeroReplicasUnhealthy: false,
			wantCounted:           false,
			wantHealthy:           false,
		},
		{
			name:                  "scaled to zero, no selector -> not counted even under unhealthy policy",
			pods:                  nil,
			hasSelector:           false,
			zeroReplicasUnhealthy: true,
			wantCounted:           false,
			wantHealthy:           false,
		},
		{
			name:                  "has probed pods -> normal path, not scaled to zero",
			pods:                  []corev1.Pod{podWithProbe("a", true)},
			hasSelector:           true,
			zeroReplicasUnhealthy: true,
			wantCounted:           true,
			wantHealthy:           true,
		},
		{
			name:                  "pods exist but none probed -> exempt (not scaled to zero)",
			pods:                  []corev1.Pod{podNoProbe("a")},
			hasSelector:           true,
			zeroReplicasUnhealthy: true,
			wantCounted:           false,
			wantHealthy:           false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			counted, healthy, _, _ := EvaluateService(tc.pods, 1, tc.hasSelector, tc.zeroReplicasUnhealthy)
			if counted != tc.wantCounted {
				t.Fatalf("counted = %v, want %v", counted, tc.wantCounted)
			}
			if healthy != tc.wantHealthy {
				t.Fatalf("healthy = %v, want %v", healthy, tc.wantHealthy)
			}
		})
	}
}

func TestEvaluate_TerminatingIgnored(t *testing.T) {
	p := podWithProbe("a", false)
	now := metav1.Now()
	p.DeletionTimestamp = &now
	res := Evaluate([]corev1.Pod{p})
	if res.ProbedPods != 0 {
		t.Fatalf("terminating pod should be ignored, got probed=%d", res.ProbedPods)
	}
}
