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

// Package health evaluates the aggregate health of the workloads that back a
// Gateway API Gateway by tracing:
//
//	Gateway -> (LoadBalancer) Service(s) -> Endpoints/Pods -> containers -> probes
//
// The rules, per the operator's contract:
//
//   - A pod with NO readiness/liveness/startup probes is EXEMPT and ignored.
//   - A pod with at least one probe is considered. If any of its probes is
//     failing (pod not Ready), it counts as unhealthy.
//   - A Gateway is Unhealthy if at least one non-exempt backing pod is failing.
//   - A Gateway whose backing pods are ALL exempt (or has no pods) is Exempt.
package health

import (
	corev1 "k8s.io/api/core/v1"
)

// PodEvaluation is the per-pod outcome of a health assessment.
type PodEvaluation struct {
	Namespace string
	Name      string
	// Probed indicates the pod declares at least one readiness/liveness/startup probe.
	Probed bool
	// Ready indicates the pod's containers report Ready via their probes.
	Ready bool
	// Reason is a short human-readable explanation.
	Reason string
}

// Result aggregates pod evaluations into an overall Gateway health decision.
type Result struct {
	// TotalPods is the number of backing pods discovered.
	TotalPods int
	// ProbedPods is the number of pods that declare probes (non-exempt).
	ProbedPods int
	// UnhealthyPods is the number of probed pods currently failing.
	UnhealthyPods int
	// Pods holds the per-pod detail (useful for events/status/messages).
	Pods []PodEvaluation
}

// AllExempt reports whether every discovered pod is exempt (declares no probes),
// or no pods were discovered at all.
func (r Result) AllExempt() bool {
	return r.ProbedPods == 0
}

// Healthy reports whether the Gateway should be considered healthy: there is at
// least one probed pod and none of the probed pods are failing.
func (r Result) Healthy() bool {
	return r.ProbedPods > 0 && r.UnhealthyPods == 0
}

// ServiceHealth is the health of a single backend behind a Gateway, at the
// Service granularity used for the minimum-healthy-backend-percentage decision.
type ServiceHealth struct {
	Namespace string
	Name      string
	// Counted indicates the backend has a health signal and participates in the
	// percentage (probed pods, or a Skupper Listener). Probe-less/exempt
	// backends are not counted.
	Counted bool
	// Healthy is meaningful only when Counted; true when the backend is up.
	Healthy bool
}

// GatewayDecision is the aggregate outcome for a Gateway given its per-service
// health and the configured minimum-healthy-backend percentage.
type GatewayDecision struct {
	// Counted / Healthy are the denominator / numerator of the ratio.
	Counted int
	Healthy int
	// HealthyPercent is 100*Healthy/Counted (0 when Counted==0).
	HealthyPercent int
	// Threshold is the configured minimum healthy percentage (inclusive).
	Threshold int
	// Unhealthy is true when the Gateway should be withdrawn: it has at least
	// one counted backend and the healthy ratio is strictly below Threshold.
	Unhealthy bool
	// Exempt is true when no backend is counted (nothing to health-check).
	Exempt bool
}

// EvaluateGateway applies the minimum-healthy-backend-percentage rule to a set
// of per-service health results.
//
// The Gateway stays advertised (not Unhealthy) while
//
//	100 * healthy / counted >= threshold   (inclusive)
//
// and is Unhealthy when the ratio drops below the threshold. With threshold=100
// (the default) any single counted backend going down makes the Gateway
// Unhealthy. When no backend is counted, the Gateway is Exempt.
func EvaluateGateway(services []ServiceHealth, thresholdPercent int) GatewayDecision {
	d := GatewayDecision{Threshold: thresholdPercent}
	for _, s := range services {
		if !s.Counted {
			continue
		}
		d.Counted++
		if s.Healthy {
			d.Healthy++
		}
	}
	if d.Counted == 0 {
		d.Exempt = true
		return d
	}
	d.HealthyPercent = (100 * d.Healthy) / d.Counted
	// Inclusive: stay up while healthy% >= threshold; withdraw when below.
	d.Unhealthy = d.HealthyPercent < thresholdPercent
	return d
}

// PodHasProbes reports whether any container in the pod declares a readiness,
// liveness, or startup probe. Init containers are ignored for this purpose
// because they do not represent steady-state serving health.
func PodHasProbes(pod *corev1.Pod) bool {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.ReadinessProbe != nil || c.LivenessProbe != nil || c.StartupProbe != nil {
			return true
		}
	}
	return false
}

// PodReady returns true when the pod's PodReady condition is True. Readiness is
// the signal that a pod's readiness probe (and thus its health) is passing and
// that it is eligible to receive traffic.
func PodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// EvaluatePod assesses a single pod against the exemption/health rules.
func EvaluatePod(pod *corev1.Pod) PodEvaluation {
	eval := PodEvaluation{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}

	// Terminating or completed pods should not by themselves mark a Gateway
	// unhealthy; they are transient. Only running/pending pods that declare
	// probes participate.
	if pod.DeletionTimestamp != nil {
		eval.Probed = false
		eval.Reason = "pod terminating; ignored"
		return eval
	}

	if !PodHasProbes(pod) {
		eval.Probed = false
		eval.Reason = "no health probes declared; exempt"
		return eval
	}

	eval.Probed = true
	if PodReady(pod) {
		eval.Ready = true
		eval.Reason = "all probes passing (PodReady=True)"
		return eval
	}

	eval.Ready = false
	eval.Reason = "one or more probes failing (PodReady!=True)"
	return eval
}

// Evaluate aggregates a set of pods into a Result.
func Evaluate(pods []corev1.Pod) Result {
	res := Result{TotalPods: len(pods)}
	for i := range pods {
		e := EvaluatePod(&pods[i])
		res.Pods = append(res.Pods, e)
		if !e.Probed {
			continue
		}
		res.ProbedPods++
		if !e.Ready {
			res.UnhealthyPods++
		}
	}
	return res
}
