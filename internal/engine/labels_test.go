// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

// internal/engine/labels_test.go
package engine

import "testing"

func TestGCLabels(t *testing.T) {
	l := GCLabels("uid-123", "cluster-abc", "kubernetescluster")
	if l[LabelInstanceUID] != "uid-123" {
		t.Fatalf("instance-uid = %q", l[LabelInstanceUID])
	}
	if l[LabelConsumerCluster] != "cluster-abc" {
		t.Fatalf("consumer-cluster = %q", l[LabelConsumerCluster])
	}
	if l[LabelBlueprint] != "kubernetescluster" {
		t.Fatalf("blueprint = %q", l[LabelBlueprint])
	}
	if len(l) != 3 {
		t.Fatalf("want 3 labels, got %d", len(l))
	}
}
