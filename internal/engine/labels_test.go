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
