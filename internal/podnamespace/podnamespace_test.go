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

package podnamespace

import "testing"

func TestGet_FromEnvVar(t *testing.T) {
	t.Setenv(EnvVar, "beacon-test-ns")
	got, err := Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != "beacon-test-ns" {
		t.Fatalf("Get() = %q, want %q", got, "beacon-test-ns")
	}
}

func TestGet_ErrorsWhenNeitherAvailable(t *testing.T) {
	t.Setenv(EnvVar, "")
	// ServiceAccountFile won't exist in a test environment; just verify we get
	// a descriptive error rather than a panic or silent empty success.
	if _, err := Get(); err == nil {
		t.Fatal("expected an error when neither POD_NAMESPACE nor the SA file is available")
	}
}
