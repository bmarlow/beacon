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

package rcache

import (
	"sync"
	"testing"
	"time"
)

func TestGetSet(t *testing.T) {
	c := New(time.Minute)
	if _, ok := c.Get("missing"); ok {
		t.Fatal("expected miss on empty cache")
	}
	c.Set("k", "v")
	v, ok := c.Get("k")
	if !ok || v != "v" {
		t.Fatalf("expected hit with value %q, got %v, %v", "v", v, ok)
	}
}

func TestExpiry(t *testing.T) {
	c := New(10 * time.Millisecond)
	c.Set("k", 42)
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("expected immediate hit, got %v, %v", v, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected miss after TTL expiry")
	}
}

// TestNilCache verifies a nil *Cache is a safe, always-miss no-op, so callers
// can pass a nil Cache (caching disabled) without special-casing every call
// site.
func TestNilCache(t *testing.T) {
	var c *Cache
	c.Set("k", "v") // must not panic
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected nil cache to always miss")
	}
}

// TestCanCacheNilValue verifies a cached nil pointer (e.g. "object not
// found") round-trips as a hit with a nil value, distinct from a miss —
// callers rely on this to avoid re-issuing a live lookup for a confirmed-
// absent object within the TTL window.
func TestCanCacheNilValue(t *testing.T) {
	c := New(time.Minute)
	var nilPod *struct{}
	c.Set("k", nilPod)
	v, ok := c.Get("k")
	if !ok {
		t.Fatal("expected hit for cached nil value")
	}
	if v.(*struct{}) != nil {
		t.Fatal("expected the cached value to still be nil")
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Set("k", i)
			c.Get("k")
		}(i)
	}
	wg.Wait()
}
