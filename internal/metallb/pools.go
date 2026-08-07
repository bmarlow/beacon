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

package metallb

import (
	"bytes"
	"net"
	"strings"
)

// PoolContainsIP reports whether the given IP (string form) falls within any of
// the pool's configured address ranges/CIDRs.
func PoolContainsIP(pool *IPAddressPool, ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, addr := range pool.Spec.Addresses {
		if addressRangeContains(addr, parsed) {
			return true
		}
	}
	return false
}

// addressRangeContains handles both CIDR ("10.0.0.0/24") and hyphenated range
// ("10.0.0.1-10.0.0.9") notations used by MetalLB IPAddressPools.
func addressRangeContains(spec string, ip net.IP) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return false
	}
	if strings.Contains(spec, "/") {
		_, ipnet, err := net.ParseCIDR(spec)
		if err != nil {
			return false
		}
		return ipnet.Contains(ip)
	}
	if strings.Contains(spec, "-") {
		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			return false
		}
		start := net.ParseIP(strings.TrimSpace(parts[0]))
		end := net.ParseIP(strings.TrimSpace(parts[1]))
		if start == nil || end == nil {
			return false
		}
		return ipInRange(ip, start, end)
	}
	// Single address.
	single := net.ParseIP(spec)
	return single != nil && single.Equal(ip)
}

func ipInRange(ip, start, end net.IP) bool {
	ip16, start16, end16 := ip.To16(), start.To16(), end.To16()
	if ip16 == nil || start16 == nil || end16 == nil {
		return false
	}
	return bytes.Compare(ip16, start16) >= 0 && bytes.Compare(ip16, end16) <= 0
}

// PoolForIP returns the first pool from the list whose ranges contain ip, or
// nil if none match.
func PoolForIP(pools []IPAddressPool, ip string) *IPAddressPool {
	for i := range pools {
		if PoolContainsIP(&pools[i], ip) {
			return &pools[i]
		}
	}
	return nil
}
