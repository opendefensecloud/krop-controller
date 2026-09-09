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

// TestProviderChildName_NoSeparatorSeamOnTruncation exercises the M2 fix: when the
// readable prefix is truncated at a length that lands right on a "-"/"." boundary,
// the trailing separator must be trimmed so the joined name has no "--"/".-" seam
// before the hash suffix (the seam is DNS-valid but unclean).
func TestProviderChildName_NoSeparatorSeamOnTruncation(t *testing.T) {
	// Craft an original name long enough to force truncation, whose byte at the
	// truncation boundary is a separator. The prefix is "<cluster>-<instance>-<orig>";
	// with cluster/instance short, the truncation index falls inside orig. Build orig
	// as many "a" then a "-" exactly at the cut point.
	suffixLen := 12 // hex suffix length used by ProviderChildName
	cut := maxNameLen - 1 - suffixLen
	cluster, instance := "c", "i"
	prefixHead := len(cluster) + 1 + len(instance) + 1 // "c-i-"
	// Make orig so that base[cut-1] (last kept byte) is a '-' or '.'.
	orig := strings.Repeat("a", cut-prefixHead-1) + "-" + strings.Repeat("b", 50)
	got := ProviderChildName(cluster, instance, orig)
	if strings.Contains(got, "--") || strings.Contains(got, ".-") {
		t.Fatalf("derived name has a separator seam: %q", got)
	}
	if len(got) > maxNameLen {
		t.Fatalf("name too long: %d", len(got))
	}
}

func TestChildName_ZeroConstraintsMatchProviderChildName(t *testing.T) {
	// The constrained form must be a strict generalization: with no constraints it
	// reproduces today's names byte-for-byte, so existing children keep their
	// identity and no migration is implied by this feature.
	got := ChildName("cluster1", "demo", "eu-record", NameConstraints{})
	if want := ProviderChildName("cluster1", "demo", "eu-record"); got != want {
		t.Fatalf("zero constraints changed the name: got %q, want %q", got, want)
	}
}

func TestChildName_RespectsMaxLength(t *testing.T) {
	// A kcp logical cluster name plus instance plus original comfortably exceeds
	// the 30-character ceiling Google Cloud enforces (issue #30).
	got := ChildName("231cyw14qhtl611l", "test-project-1-cat-cloudapi", "test-project-1",
		NameConstraints{MaxLength: 30})
	if len(got) > 30 {
		t.Fatalf("name exceeds maxLength: %d chars, %q", len(got), got)
	}
}

func TestChildName_PrefixStartsAlphabetic(t *testing.T) {
	// kcp logical cluster names routinely start with a digit, which RFC1123 label
	// types reject. The prefix is how a blueprint author guarantees a letter start.
	got := ChildName("231cyw14qhtl611l", "demo", "eu-record", NameConstraints{Prefix: "p"})
	if got[0] < 'a' || got[0] > 'z' {
		t.Fatalf("name must start with a letter, got %q", got)
	}
	if !strings.HasPrefix(got, "p-") {
		t.Fatalf("name must carry the configured prefix, got %q", got)
	}
}

func TestChildName_CollisionFreeUnderTruncation(t *testing.T) {
	// Truncating the readable middle must never cost injectivity: the full content
	// hash survives, so distinct tuples stay distinct even at the tightest ceiling.
	c := NameConstraints{MaxLength: 30, Prefix: "p"}
	a := ChildName("231cyw14qhtl611l", "test-project-1", "proj", c)
	b := ChildName("kvdk8299mah3yj1p", "test-project-1", "proj", c)
	if a == b {
		t.Fatalf("different consumers collided under truncation, both %q", a)
	}
	if len(a) > 30 || len(b) > 30 {
		t.Fatalf("truncated names exceed maxLength: %q (%d), %q (%d)", a, len(a), b, len(b))
	}
}

func TestChildName_NoSeparatorSeamUnderMaxLength(t *testing.T) {
	// Same seam rule as the unconstrained form: truncation must not leave a
	// trailing "-"/"." butting against the hash suffix.
	got := ChildName("cluster1", "demo-", "record", NameConstraints{MaxLength: 24})
	if strings.Contains(got, "--") || strings.Contains(got, ".-") {
		t.Fatalf("derived name has a separator seam: %q", got)
	}
}

func TestChildName_MaxLengthTooSmallForReadablePartStillHashes(t *testing.T) {
	// A ceiling that leaves no room for the readable middle must still produce a
	// deterministic, prefixed, in-bounds name rather than panicking or overflowing.
	// (The Registrar rejects such constraints at publish time; this is the
	// belt-and-braces runtime behavior.)
	c := NameConstraints{MaxLength: 15, Prefix: "p"}
	got := ChildName("231cyw14qhtl611l", "demo", "eu-record", c)
	if len(got) > 15 {
		t.Fatalf("name exceeds maxLength: %d chars, %q", len(got), got)
	}
	if got != ChildName("231cyw14qhtl611l", "demo", "eu-record", c) {
		t.Fatal("degenerate form is not deterministic")
	}
	if !strings.HasPrefix(got, "p-") {
		t.Fatalf("prefix must survive: %q", got)
	}
}

func TestNameConstraints_Validate(t *testing.T) {
	cases := []struct {
		name    string
		c       NameConstraints
		wantErr bool
	}{
		{"unconstrained zero value", NameConstraints{}, false},
		{"prefix only", NameConstraints{Prefix: "p"}, false},
		{"gcp ceiling with prefix", NameConstraints{MaxLength: 30, Prefix: "p"}, false},
		{"exactly enough for one readable char", NameConstraints{MaxLength: 16, Prefix: "p"}, false},
		{"one char short of readable", NameConstraints{MaxLength: 15, Prefix: "p"}, true},
		{"long prefix eats the budget", NameConstraints{MaxLength: 16, Prefix: "verylongprefix"}, true},
		{"no prefix needs less room", NameConstraints{MaxLength: 14}, false},
		{"no prefix, one short", NameConstraints{MaxLength: 13}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error for %+v", tc.c)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil for %+v", err, tc.c)
			}
		})
	}
}

// Validate is the publish-time gate for exactly the runtime degeneration
// ChildName falls back to: anything Validate accepts must still leave room for a
// readable character, so an accepted constraint never produces a bare hash.
func TestNameConstraints_ValidateAgreesWithChildName(t *testing.T) {
	c := NameConstraints{MaxLength: 16, Prefix: "p"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	got := ChildName("231cyw14qhtl611l", "demo", "eu-record", c)
	if len(got) > c.MaxLength {
		t.Fatalf("accepted constraint produced an over-length name: %q (%d)", got, len(got))
	}
	if got == "p-"+"" {
		t.Fatal("accepted constraint degenerated to a bare prefix")
	}
	// "p-" + at least one readable char + "-" + 12 hex
	if len(got) < 2+1+1+hashLen {
		t.Fatalf("accepted constraint lost its readable part: %q", got)
	}
}
