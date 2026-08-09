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

package health

import "testing"

func svc(counted, healthy bool) ServiceHealth {
	return ServiceHealth{Counted: counted, Healthy: healthy}
}

func TestEvaluateGateway_Default100(t *testing.T) {
	// threshold 100: any counted backend down => unhealthy.
	d := EvaluateGateway([]ServiceHealth{svc(true, true), svc(true, false)}, 100)
	if !d.Unhealthy {
		t.Fatalf("expected unhealthy at 100%% with one down, got %+v", d)
	}
	d = EvaluateGateway([]ServiceHealth{svc(true, true), svc(true, true)}, 100)
	if d.Unhealthy || d.Exempt {
		t.Fatalf("expected healthy with all up, got %+v", d)
	}
}

func TestEvaluateGateway_50PercentInclusive(t *testing.T) {
	up, down := svc(true, true), svc(true, false)
	// 4 backends, 2 down => 50% up, threshold 50 (inclusive) => stay up.
	d := EvaluateGateway([]ServiceHealth{up, up, down, down}, 50)
	if d.Unhealthy {
		t.Fatalf("50%% up should meet inclusive 50%% threshold (stay up), got %+v", d)
	}
	if d.HealthyPercent != 50 {
		t.Fatalf("expected 50%%, got %d", d.HealthyPercent)
	}
	// 3 down => 25% up => below threshold => withdraw.
	d = EvaluateGateway([]ServiceHealth{up, down, down, down}, 50)
	if !d.Unhealthy {
		t.Fatalf("25%% up should be below 50%% threshold (withdraw), got %+v", d)
	}
}

func TestEvaluateGateway_ExemptWhenNoneCounted(t *testing.T) {
	d := EvaluateGateway([]ServiceHealth{svc(false, false), svc(false, false)}, 100)
	if !d.Exempt || d.Unhealthy {
		t.Fatalf("expected exempt when no counted backends, got %+v", d)
	}
}

func TestEvaluateGateway_UncountedIgnored(t *testing.T) {
	// One uncounted (probe-less) backend must not affect the ratio.
	up, down := svc(true, true), svc(true, false)
	d := EvaluateGateway([]ServiceHealth{up, down, svc(false, false)}, 50)
	// 2 counted, 1 up => 50% => inclusive threshold 50 => stay up.
	if d.Counted != 2 || d.Healthy != 1 || d.Unhealthy {
		t.Fatalf("uncounted backend should be excluded; got %+v", d)
	}
}

func TestEvaluateGateway_Zero(t *testing.T) {
	// threshold 0: never withdraw due to backend health (as long as counted>0).
	d := EvaluateGateway([]ServiceHealth{svc(true, false), svc(true, false)}, 0)
	if d.Unhealthy {
		t.Fatalf("threshold 0 should never be below; got %+v", d)
	}
}
