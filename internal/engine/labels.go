// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package engine

// GC-tracking label keys stamped on every materialized child so instance
// deletion can enumerate + delete them across workspaces (idea.md §11).
// Provider-target children live in a different workspace than the instance, so
// owner references cannot reach them — labels are the cross-workspace handle.
const (
	LabelInstanceUID     = "krop.opendefense.cloud/instance-uid"
	LabelConsumerCluster = "krop.opendefense.cloud/consumer-cluster"
	LabelBlueprint       = "krop.opendefense.cloud/blueprint"

	// LabelLiveness marks a provider-workspace liveness record (a ConfigMap the
	// reconciler refreshes on every complete pass) so the orphan sweeper can list
	// only those records. See idea.md §11 (orphan risk on mid-life unbind).
	LabelLiveness = "krop.opendefense.cloud/liveness"

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
