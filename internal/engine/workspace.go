// internal/engine/workspace.go
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

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// AnnotationConsumerCluster is the instance-metadata annotation the reconciler
// stamps (runtime-only) with the consumer's kcp logical-cluster name. Blueprints
// reference it via CEL to derive collision-free host/provider child names:
//
//	${schema.metadata.annotations["krop.opendefense.cloud/consumer-cluster"]}
//
// It deliberately shares its key with LabelConsumerCluster: materialized children
// carry the same value as a label, so the annotation on the instance and the label
// on its children always agree. kro exposes the instance metadata (spec + metadata,
// no status) as the `schema` variable, and `annotations` is an open string map, so
// this is the one CEL-reachable place to surface data kro's build-time validation
// does not know about (a bare `${workspace.name}` top-level variable would not
// type-check).
const AnnotationConsumerCluster = LabelConsumerCluster

// StampConsumerCluster sets the consumer-cluster annotation on obj in place. The
// reconciler applies it to a runtime-only DEEP COPY of the instance before building
// the kro runtime, so the (globally unique, immutable) consumer logical-cluster name
// is visible to blueprint CEL. It is never written back to the stored instance.
func StampConsumerCluster(obj *unstructured.Unstructured, clusterName string) {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnotationConsumerCluster] = clusterName
	obj.SetAnnotations(ann)
}
