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

// internal/engine/naming_test.go
package engine

import (
	"strings"
	"testing"
)

func TestProviderChildName_Deterministic(t *testing.T) {
	a := ProviderChildName("cluster1", "demo", "eu-record")
	b := ProviderChildName("cluster1", "demo", "eu-record")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
}

func TestProviderChildName_CollisionFreeAcrossClusters(t *testing.T) {
	a := ProviderChildName("cluster1", "demo", "eu-record")
	b := ProviderChildName("cluster2", "demo", "eu-record")
	if a == b {
		t.Fatalf("different consumers must not collide, both %q", a)
	}
}

func TestProviderChildName_CollisionFreeAcrossInstances(t *testing.T) {
	a := ProviderChildName("cluster1", "demo", "eu-record")
	b := ProviderChildName("cluster1", "prod", "eu-record")
	if a == b {
		t.Fatalf("different instances must not collide, both %q", a)
	}
}

func TestProviderChildName_InjectiveOnHyphenBoundary(t *testing.T) {
	// The readable hyphen-joined prefix is identical ("a-b-c-d") for both tuples
	// because instance names may contain hyphens, but the tuples differ, so the
	// derived names must differ (strict injectivity via the tuple hash).
	a := ProviderChildName("a", "b-c", "d")
	b := ProviderChildName("a-b", "c", "d")
	if a == b {
		t.Fatalf("distinct tuples with identical readable prefix collided, both %q", a)
	}
}

func TestProviderChildName_LongInputStaysDNSSafe(t *testing.T) {
	long := strings.Repeat("a", 300)
	got := ProviderChildName(long, long, long)
	if len(got) > 253 {
		t.Fatalf("name too long: %d", len(got))
	}
	// deterministic even when hashed
	if got != ProviderChildName(long, long, long) {
		t.Fatal("hashed form not deterministic")
	}
}
