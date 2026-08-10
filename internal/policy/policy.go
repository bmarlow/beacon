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

// Package policy centralizes exemption and gateway-class filtering decisions so
// they are consistent across the controller and easy to unit test.
package policy

import (
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
)

const (
	// ExemptAnnotation, when set to "true" on a Gateway, exempts it from all
	// Beacon health checks and advertisement management.
	ExemptAnnotation = "beacon.io/exempt"

	// OverrideWithdrawAfterAnnotation lets a Gateway override the global
	// withdrawAfter timer, e.g. "beacon.io/withdraw-after: 10s".
	OverrideWithdrawAfterAnnotation = "beacon.io/withdraw-after"

	// OverrideReadvertiseAfterAnnotation lets a Gateway override the global
	// readvertiseAfter timer, e.g. "beacon.io/readvertise-after: 60s".
	OverrideReadvertiseAfterAnnotation = "beacon.io/readvertise-after"

	// OverrideMinHealthyPercentAnnotation lets a Gateway override the global
	// minHealthyBackendPercent threshold, e.g. "beacon.io/min-healthy-percent: 50".
	OverrideMinHealthyPercentAnnotation = "beacon.io/min-healthy-percent"

	// OverrideMinHealthyPodPercentAnnotation lets a Gateway (or a backend
	// Service) override the minHealthyPodPercent threshold, e.g.
	// "beacon.io/min-healthy-pod-percent: 50". On a Service it overrides the
	// Gateway-level value for that Service only.
	OverrideMinHealthyPodPercentAnnotation = "beacon.io/min-healthy-pod-percent"

	// DefaultMinHealthyBackendPercent is used when neither the policy nor an
	// annotation specifies one: 100% (any counted backend down withdraws).
	DefaultMinHealthyBackendPercent int32 = 100

	// DefaultMinHealthyPodPercent is used when nothing specifies one: 1%, i.e.
	// a single Ready pod keeps the Service (and Gateway) up.
	DefaultMinHealthyPodPercent int32 = 1
)

// IsExempt reports whether a Gateway is exempt from Beacon management, honoring
// BOTH the per-Gateway annotation and the central exemption list in the policy.
func IsExempt(gw *gwapiv1.Gateway, spec *beaconv1alpha1.GatewayHealthPolicySpec) bool {
	if gw.Annotations[ExemptAnnotation] == "true" {
		return true
	}
	for _, ref := range spec.Exemptions {
		if ref.Namespace == gw.Namespace && ref.Name == gw.Name {
			return true
		}
	}
	return false
}

// ClassAllowed reports whether the Gateway's class is permitted by the policy's
// gatewayClassNames filter. An empty filter allows all classes.
func ClassAllowed(gw *gwapiv1.Gateway, spec *beaconv1alpha1.GatewayHealthPolicySpec) bool {
	if len(spec.GatewayClassNames) == 0 {
		return true
	}
	for _, name := range spec.GatewayClassNames {
		if string(gw.Spec.GatewayClassName) == name {
			return true
		}
	}
	return false
}

// Managed reports whether Beacon should actively manage this Gateway.
func Managed(gw *gwapiv1.Gateway, spec *beaconv1alpha1.GatewayHealthPolicySpec) bool {
	return !IsExempt(gw, spec) && ClassAllowed(gw, spec)
}

// WithdrawAfter returns the effective withdraw dampening duration for a Gateway,
// applying a per-Gateway annotation override when present and parseable.
func WithdrawAfter(gw *gwapiv1.Gateway, spec *beaconv1alpha1.GatewayHealthPolicySpec) metav1.Duration {
	if d, ok := parseDurationAnnotation(gw, OverrideWithdrawAfterAnnotation); ok {
		return d
	}
	return spec.WithdrawAfter
}

// ReadvertiseAfter returns the effective re-advertise dampening duration.
func ReadvertiseAfter(gw *gwapiv1.Gateway, spec *beaconv1alpha1.GatewayHealthPolicySpec) metav1.Duration {
	if d, ok := parseDurationAnnotation(gw, OverrideReadvertiseAfterAnnotation); ok {
		return d
	}
	return spec.ReadvertiseAfter
}

// MinHealthyBackendPercent returns the effective minimum-healthy-backend
// percentage threshold for a Gateway: the per-Gateway annotation override if
// present and valid, else the policy value, else the default (100). The result
// is clamped to [0, 100].
func MinHealthyBackendPercent(gw *gwapiv1.Gateway, spec *beaconv1alpha1.GatewayHealthPolicySpec) int32 {
	if n, ok := parsePercentAnnotation(gw.Annotations, OverrideMinHealthyPercentAnnotation); ok {
		return n
	}
	if spec.MinHealthyBackendPercent != nil {
		return clampPercent(*spec.MinHealthyBackendPercent)
	}
	return DefaultMinHealthyBackendPercent
}

// MinHealthyPodPercent returns the Gateway-level minimum-healthy-pod percentage
// (the per-Service pod threshold default for this Gateway): the per-Gateway
// annotation override if present and valid, else the policy value, else the
// default (1 = any Ready pod). Clamped to [0, 100].
func MinHealthyPodPercent(gw *gwapiv1.Gateway, spec *beaconv1alpha1.GatewayHealthPolicySpec) int32 {
	if n, ok := parsePercentAnnotation(gw.Annotations, OverrideMinHealthyPodPercentAnnotation); ok {
		return n
	}
	if spec.MinHealthyPodPercent != nil {
		return clampPercent(*spec.MinHealthyPodPercent)
	}
	return DefaultMinHealthyPodPercent
}

// ServiceMinHealthyPodPercent returns the effective per-Service pod threshold,
// applying precedence: Service annotation > Gateway-level value.
// gatewayLevel is the value from MinHealthyPodPercent(gw, spec).
func ServiceMinHealthyPodPercent(svcAnnotations map[string]string, gatewayLevel int32) int32 {
	if n, ok := parsePercentAnnotation(svcAnnotations, OverrideMinHealthyPodPercentAnnotation); ok {
		return n
	}
	return gatewayLevel
}

// parsePercentAnnotation parses an integer percent annotation (optional trailing
// '%'), returning the clamped value and whether it was present and valid.
func parsePercentAnnotation(annotations map[string]string, key string) (int32, bool) {
	v, ok := annotations[key]
	if !ok || v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), "%"))
	if err != nil {
		return 0, false
	}
	return clampPercent(int32(n)), true
}

func clampPercent(n int32) int32 {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func parseDurationAnnotation(gw *gwapiv1.Gateway, key string) (metav1.Duration, bool) {
	v, ok := gw.Annotations[key]
	if !ok || v == "" {
		return metav1.Duration{}, false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return metav1.Duration{}, false
	}
	return metav1.Duration{Duration: d}, true
}
