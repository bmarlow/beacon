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

import "testing"

func TestPoolContainsIP_CIDR(t *testing.T) {
	p := &IPAddressPool{Spec: IPAddressPoolSpec{Addresses: []string{"192.168.10.0/24"}}}
	if !PoolContainsIP(p, "192.168.10.50") {
		t.Fatal("expected 192.168.10.50 in /24")
	}
	if PoolContainsIP(p, "192.168.11.1") {
		t.Fatal("did not expect 192.168.11.1 in /24")
	}
}

func TestPoolContainsIP_Range(t *testing.T) {
	p := &IPAddressPool{Spec: IPAddressPoolSpec{Addresses: []string{"10.0.0.5-10.0.0.9"}}}
	if !PoolContainsIP(p, "10.0.0.7") {
		t.Fatal("expected 10.0.0.7 in range")
	}
	if PoolContainsIP(p, "10.0.0.10") {
		t.Fatal("did not expect 10.0.0.10 in range")
	}
	if PoolContainsIP(p, "10.0.0.4") {
		t.Fatal("did not expect 10.0.0.4 in range")
	}
}

func TestPoolContainsIP_Single(t *testing.T) {
	p := &IPAddressPool{Spec: IPAddressPoolSpec{Addresses: []string{"172.16.0.1"}}}
	if !PoolContainsIP(p, "172.16.0.1") {
		t.Fatal("expected exact match")
	}
	if PoolContainsIP(p, "172.16.0.2") {
		t.Fatal("did not expect non-match")
	}
}

func TestPoolForIP(t *testing.T) {
	pools := []IPAddressPool{
		{Spec: IPAddressPoolSpec{Addresses: []string{"1.1.1.0/24"}}},
		{Spec: IPAddressPoolSpec{Addresses: []string{"2.2.2.0/24"}}},
	}
	if PoolForIP(pools, "2.2.2.2") == nil {
		t.Fatal("expected match in second pool")
	}
	if PoolForIP(pools, "3.3.3.3") != nil {
		t.Fatal("expected no match")
	}
}
