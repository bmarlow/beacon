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

// Package state provides a small, thread-safe store of per-Gateway health and
// advertisement state. The controller writes to it during reconciliation; the
// web UI's topology builder reads from it to annotate the graph with live
// advertisement decisions (which are only known to the running controller, not
// derivable from cluster objects alone).
package state

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// GatewaySnapshot is the externally-visible state for one Gateway.
type GatewaySnapshot struct {
	Health         string
	Advertisement  string
	LastTransition time.Time
	Message        string

	// Timer reflects a running dampening timer (backoff before scaling to zero,
	// or recovery before scaling back up). Nil when no timer is active.
	Timer *TimerStatus
}

// TimerStatus describes a running dampening timer for the UI.
type TimerStatus struct {
	// Kind is "backoff" (unhealthy -> withdraw) or "recovery" (healthy ->
	// re-advertise).
	Kind string
	// Threshold is the configured duration the condition must persist.
	Threshold time.Duration
	// Elapsed is how long the condition has persisted so far.
	Elapsed time.Duration
	// Remaining is Threshold-Elapsed (never negative).
	Remaining time.Duration
}

// Store is a concurrency-safe map of Gateway -> snapshot.
type Store struct {
	mu    sync.RWMutex
	items map[types.NamespacedName]GatewaySnapshot
}

// New returns an initialized Store.
func New() *Store {
	return &Store{items: map[types.NamespacedName]GatewaySnapshot{}}
}

// Set records the snapshot for a Gateway.
func (s *Store) Set(key types.NamespacedName, snap GatewaySnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = snap
}

// Get returns the snapshot for a Gateway and whether it was present.
func (s *Store) Get(key types.NamespacedName) (GatewaySnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[key]
	return v, ok
}

// Delete removes a Gateway's snapshot.
func (s *Store) Delete(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key)
}

// Snapshot returns a copy of all entries.
func (s *Store) Snapshot() map[types.NamespacedName]GatewaySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[types.NamespacedName]GatewaySnapshot, len(s.items))
	for k, v := range s.items {
		out[k] = v
	}
	return out
}
