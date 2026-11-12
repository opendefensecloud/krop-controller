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

import (
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// nodeLogLevel is the verbosity of the per-node lines (ADR 0013: V(2) shows
// what each pass did).
const nodeLogLevel = 2

// GVKString renders a GVK as "group/version/Kind" for log fields; the core group
// renders with an empty first segment ("/v1/ConfigMap"), matching the token
// format of the liveness record's child GVK lists.
func GVKString(gvk schema.GroupVersionKind) string {
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}

// logNode writes one V(2) line for a node the pass applied or read. It logs only
// identities (names, GVK, target) and readiness, never object content: children
// may carry secrets in status. A single resource logs child (+childNamespace),
// a collection logs the children's names.
func logNode(l logr.Logger, msg, nodeID string, target Target, gvk schema.GroupVersionKind,
	observed []*unstructured.Unstructured, collection bool, readyErr error) {
	v := l.V(nodeLogLevel)
	if !v.Enabled() {
		return
	}
	kv := []any{"node", nodeID, "target", string(target), "gvk", GVKString(gvk), "ready", readyErr == nil}
	if collection {
		names := make([]string, 0, len(observed))
		for _, o := range observed {
			names = append(names, o.GetName())
		}
		kv = append(kv, "children", names)
	} else if len(observed) > 0 {
		kv = append(kv, "child", observed[0].GetName())
		if ns := observed[0].GetNamespace(); ns != "" {
			kv = append(kv, "childNamespace", ns)
		}
	}
	if readyErr != nil {
		kv = append(kv, "reason", readyErr.Error())
	}
	v.Info(msg, kv...)
}
