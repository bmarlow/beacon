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

package controller

import (
	"github.com/beacon-operator/beacon/internal/metallb"
)

// metallbPoolList is a small adapter wrapping metallb.IPAddressPoolList so the
// reconciler can list pools and test IP membership succinctly.
type metallbPoolList struct {
	items metallb.IPAddressPoolList
}

func (m *metallbPoolList) list() *metallb.IPAddressPoolList {
	return &m.items
}

func (m *metallbPoolList) contains(ip string) bool {
	return metallb.PoolForIP(m.items.Items, ip) != nil
}
