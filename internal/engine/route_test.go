// Copyright 2026 opendefense contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// internal/engine/route_test.go
package engine

import (
	"testing"
)

func TestParseTarget_EmptyDefaultsToConsumer(t *testing.T) {
	got, err := ParseTarget("")
	if err != nil || got != TargetConsumer {
		t.Fatalf("want consumer,nil; got %q,%v", got, err)
	}
}

func TestParseTarget_ValidValues(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Target
	}{
		{"consumer", TargetConsumer},
		{"provider", TargetProvider},
		{"host", TargetHost},
	} {
		got, err := ParseTarget(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ParseTarget(%q) = %q,%v; want %q,nil", tc.in, got, err, tc.want)
		}
	}
}

func TestParseTarget_RejectsInvalid(t *testing.T) {
	if _, err := ParseTarget("bogus"); err == nil {
		t.Fatal("want error for invalid target value")
	}
}

func TestTargetForNode_Hit(t *testing.T) {
	routing := map[string]Target{"vpc": TargetProvider, "vm": TargetHost}
	if got := TargetForNode("vm", routing); got != TargetHost {
		t.Fatalf("TargetForNode hit = %q, want host", got)
	}
}

func TestTargetForNode_MissDefaultsToConsumer(t *testing.T) {
	routing := map[string]Target{"vpc": TargetProvider}
	if got := TargetForNode("absent", routing); got != TargetConsumer {
		t.Fatalf("TargetForNode miss = %q, want consumer", got)
	}
	if got := TargetForNode("x", nil); got != TargetConsumer {
		t.Fatalf("TargetForNode nil map = %q, want consumer", got)
	}
}
