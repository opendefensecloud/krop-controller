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

package engine

// GC-tracking label keys stamped on every materialized child so instance
// deletion can enumerate + delete them across workspaces (idea.md §11).
// Provider-target children live in a different workspace than the instance, so
// owner references cannot reach them — labels are the cross-workspace handle.
const (
	LabelInstanceUID     = "krop.opendefense.cloud/instance-uid"
	LabelConsumerCluster = "krop.opendefense.cloud/consumer-cluster"
	LabelBlueprint       = "krop.opendefense.cloud/blueprint"

	// Finalizer on the instance drives cross-workspace child cleanup on delete.
	Finalizer = "krop.opendefense.cloud/gc"
)

// GCLabels returns the GC-tracking label set for one instance.
func GCLabels(instanceUID, consumerCluster, blueprint string) map[string]string {
	return map[string]string{
		LabelInstanceUID:     instanceUID,
		LabelConsumerCluster: consumerCluster,
		LabelBlueprint:       blueprint,
	}
}
