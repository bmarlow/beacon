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

package export

import (
	"time"

	beaconv1alpha1 "github.com/bmarlow/beacon/api/v1alpha1"
	"github.com/bmarlow/beacon/internal/version"
)

// Build converts a GatewayHealthPolicy's already-computed status into a
// Summary. Pure and side-effect-free: the caller is responsible for fetching
// pol (see Server.handleSummary), so this stays trivially testable and, unlike
// the topology dashboard, requires no additional cluster reads beyond the one
// GatewayHealthPolicy object.
func Build(pol *beaconv1alpha1.GatewayHealthPolicy) *Summary {
	s := &Summary{
		SchemaVersion:   SchemaVersion,
		GeneratedAt:     time.Now(),
		OperatorVersion: version.Get(),
		Cluster: ClusterInfo{
			ID:     pol.Status.Cluster.ID,
			Name:   pol.Status.Cluster.Name,
			Source: string(pol.Status.Cluster.Source),
		},
		ManagedGateways: pol.Status.ManagedGateways,
		AdvertisedIPs:   pol.Status.AdvertisedIPs,
		WithdrawnIPs:    pol.Status.WithdrawnIPs,
	}
	if pol.Status.LastReconciled != nil {
		t := pol.Status.LastReconciled.Time
		s.LastReconciled = &t
	}
	for _, gs := range pol.Status.Gateways {
		s.Gateways = append(s.Gateways, GatewaySummary{
			Namespace:     gs.Namespace,
			Name:          gs.Name,
			IPs:           append([]string(nil), gs.IPs...),
			FromMetalLB:   gs.FromMetalLB,
			Health:        string(gs.Health),
			Advertisement: string(gs.Advertisement),
		})
	}
	return s
}
