// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

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
