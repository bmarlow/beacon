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

package topology

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	beaconv1alpha1 "github.com/beacon-operator/beacon/api/v1alpha1"
	"github.com/beacon-operator/beacon/internal/health"
)

// timingSince builds a StatusTiming from a transition timestamp.
func timingSince(t time.Time) StatusTiming {
	if t.IsZero() {
		return StatusTiming{}
	}
	secs := int64(time.Since(t).Round(time.Second) / time.Second)
	if secs < 0 {
		secs = 0
	}
	tt := t
	return StatusTiming{StatusSince: &tt, StatusForSeconds: secs}
}

// podStatusTiming derives when a pod entered its current readiness status from
// the PodReady condition's lastTransitionTime (falling back to start time).
func podStatusTiming(pod *corev1.Pod) StatusTiming {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && !c.LastTransitionTime.IsZero() {
			return timingSince(c.LastTransitionTime.Time)
		}
	}
	if pod.Status.StartTime != nil {
		return timingSince(pod.Status.StartTime.Time)
	}
	if !pod.CreationTimestamp.IsZero() {
		return timingSince(pod.CreationTimestamp.Time)
	}
	return StatusTiming{}
}

// latestChildTiming returns the most-recent (smallest duration) timing among
// children — i.e. the last time the aggregate could have changed.
func latestChildTiming(children []StatusTiming) StatusTiming {
	var best StatusTiming
	var bestSince *time.Time
	for _, c := range children {
		if c.StatusSince == nil {
			continue
		}
		if bestSince == nil || c.StatusSince.After(*bestSince) {
			bestSince = c.StatusSince
			best = c
		}
	}
	return best
}

func podTimings(pods []PodNode) []StatusTiming {
	out := make([]StatusTiming, 0, len(pods))
	for i := range pods {
		out = append(out, pods[i].StatusTiming)
	}
	return out
}

func serviceTimings(svcs []ServiceNode) []StatusTiming {
	out := make([]StatusTiming, 0, len(svcs))
	for i := range svcs {
		out = append(out, svcs[i].StatusTiming)
	}
	return out
}

// severity ranks statuses so we can compute the "worst" child status.
func severity(s Status) int {
	switch s {
	case StatusUnhealthy:
		return 5
	case StatusWithdrawn:
		return 4
	case StatusDegraded:
		return 3
	case StatusPending:
		return 2
	case StatusUnknown:
		return 1
	case StatusHealthy:
		return 0
	case StatusExempt:
		return 0
	}
	return 1
}

func podStatus(e health.PodEvaluation) Status {
	if !e.Probed {
		return StatusExempt
	}
	if e.Ready {
		return StatusHealthy
	}
	return StatusUnhealthy
}

func worstPodStatus(pods []PodNode) Status {
	if len(pods) == 0 {
		return StatusUnknown
	}
	worst := StatusHealthy
	anyProbed := false
	for _, p := range pods {
		if p.Probed {
			anyProbed = true
		}
		if severity(p.Status) > severity(worst) {
			worst = p.Status
		}
	}
	if !anyProbed {
		return StatusExempt
	}
	return worst
}

func worstServiceStatus(svcs []ServiceNode) Status {
	if len(svcs) == 0 {
		return StatusUnknown
	}
	worst := StatusHealthy
	for _, s := range svcs {
		if severity(s.Status) > severity(worst) {
			worst = s.Status
		}
	}
	return worst
}

// gatewayStatus folds the Gateway's health and advertisement into one status.
func gatewayStatus(n GatewayNode) Status {
	switch n.Advertisement {
	case string(beaconv1alpha1.AdvertisementWithdrawn):
		return StatusWithdrawn
	case string(beaconv1alpha1.AdvertisementPendingWithdrawal),
		string(beaconv1alpha1.AdvertisementPendingReadvertise):
		return StatusPending
	}
	switch Status(n.Health) {
	case StatusUnhealthy:
		return StatusUnhealthy
	case StatusDegraded:
		return StatusDegraded
	case StatusExempt:
		return StatusExempt
	case StatusHealthy:
		return StatusHealthy
	}
	return StatusUnknown
}

func worstGatewayStatus(gws []GatewayNode) Status {
	worst := StatusHealthy
	for _, g := range gws {
		if severity(g.Status) > severity(worst) {
			worst = g.Status
		}
	}
	return worst
}

// statusForAdvertisement decides an IP node's status from its advertisement
// state and the worst status of the gateways sharing it.
func statusForAdvertisement(adv string, worst Status) Status {
	switch adv {
	case string(beaconv1alpha1.AdvertisementWithdrawn):
		return StatusWithdrawn
	case string(beaconv1alpha1.AdvertisementPendingWithdrawal),
		string(beaconv1alpha1.AdvertisementPendingReadvertise):
		return StatusPending
	}
	return worst
}

func worstIPStatus(ips []IPNode) Status {
	worst := StatusHealthy
	for _, ip := range ips {
		if severity(ip.Status) > severity(worst) {
			worst = ip.Status
		}
	}
	if len(ips) == 0 {
		return StatusUnknown
	}
	return worst
}

func summarize(g *Graph) Summary {
	var s Summary
	s.Pools = len(g.Pools)
	seenGw := map[string]bool{}
	count := func(gw GatewayNode) {
		id := gw.Namespace + "/" + gw.Name
		if seenGw[id] {
			return
		}
		seenGw[id] = true
		s.Gateways++
		if Status(gw.Health) == StatusUnhealthy || Status(gw.Health) == StatusDegraded {
			s.UnhealthyGateway++
		}
		for _, r := range gw.Routes {
			s.Routes++
			for _, svc := range r.Services {
				s.Services++
				s.Pods += len(svc.Pods)
			}
		}
	}
	for _, p := range g.Pools {
		for _, ip := range p.IPs {
			switch ip.Advertisement {
			case string(beaconv1alpha1.AdvertisementWithdrawn),
				string(beaconv1alpha1.AdvertisementPendingReadvertise):
				s.WithdrawnIPs++
			default:
				s.AdvertisedIPs++
			}
			for _, gw := range ip.Gateways {
				count(gw)
			}
		}
	}
	for _, gw := range g.UnpooledGateways {
		count(gw)
	}
	return s
}
