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

// Package rcache provides a minimal, generic, short-TTL read-through cache.
//
// This is deliberately NOT a controller-runtime informer/watch cache. An
// informer cache lists+watches an entire GVK (optionally namespace/label
// scoped) and holds every matching object in memory for as long as the
// process runs — exactly the design that once caused Beacon's controller
// manager to OOM on a large cluster (see cmd/main.go's Client.Cache.DisableFor
// for Pod/Service/Deployment, and SetupWithManager's doc comment).
//
// rcache instead only ever holds entries that were actually requested, each
// expiring after a short TTL (seconds, not "forever"). That keeps memory
// bounded by "what's been read recently" — which scales with the number of
// Gateways Beacon manages and their backends, not with total cluster size —
// while still eliminating the redundant, repeated live API calls that the
// same narrow, targeted Get/List would otherwise make on every dashboard
// poll (every few seconds, per open browser tab) and reconcile pass.
package rcache

import (
	"sync"
	"time"
)

type entry struct {
	value   any
	expires time.Time
}

// Cache is a concurrency-safe, short-TTL cache keyed by string. A nil *Cache
// is valid and behaves as an always-miss cache (Get always returns false,
// Set is a no-op) so callers can treat an unset cache as "caching disabled"
// without extra nil checks at every call site.
type Cache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]entry
}

// New returns a Cache whose entries expire after ttl.
func New(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl, entries: make(map[string]entry)}
}

// Get returns the cached value for key if present and not expired.
func (c *Cache) Get(key string) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return e.value, true
}

// Set stores value under key with the Cache's configured TTL. It also
// opportunistically evicts a handful of already-expired entries so a churn
// of one-off keys (e.g. deleted Pods) doesn't accumulate indefinitely
// between explicit reads of the same key.
func (c *Cache) Set(key string, value any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry{value: value, expires: time.Now().Add(c.ttl)}
	now := time.Now()
	// Bound the eviction sweep so Set stays cheap even if the map is large;
	// entries missed this round are still correctly treated as expired by
	// Get, they just linger in the map a little longer.
	const maxSweep = 32
	swept := 0
	for k, e := range c.entries {
		if swept >= maxSweep {
			break
		}
		swept++
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
}
