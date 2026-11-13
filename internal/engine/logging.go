// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"regexp"

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

// conditionReason is the format Kubernetes requires of a condition reason
// (metav1.Condition): a CamelCase code such as UpdateFailed. maxReadyReasonLen
// keeps it to a code.
var conditionReason = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

const maxReadyReasonLen = 64

// readyReason returns the reason of the first Ready condition among objs whose
// status is not True, e.g. Config Connector's UpdateFailed or DependencyNotReady.
// It is the only status value krop logs (ADR 0013): the reason is a code meant
// for programs, whereas the condition's message is free text another controller
// wrote, often copied from a cloud API error, and may contain anything. A reason
// that is not a plain code (wrong format or longer than maxReadyReasonLen) is
// treated as free text and dropped.
func readyReason(objs []*unstructured.Unstructured) string {
	for _, o := range objs {
		raw, _, _ := unstructured.NestedFieldNoCopy(o.Object, "status", "conditions")
		conds, _ := raw.([]any)
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if !ok || m["type"] != "Ready" || m["status"] == "True" {
				continue
			}
			reason, _ := m["reason"].(string)
			if len(reason) <= maxReadyReasonLen && conditionReason.MatchString(reason) {
				return reason
			}
		}
	}

	return ""
}

// readyMessage returns the message of the first Ready condition among objs whose
// status is not True, the free text readyReason leaves out. It feeds the instance
// status, which the consumer reads (#49), and is never logged.
func readyMessage(objs []*unstructured.Unstructured) string {
	for _, o := range objs {
		raw, _, _ := unstructured.NestedFieldNoCopy(o.Object, "status", "conditions")
		conds, _ := raw.([]any)
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if !ok || m["type"] != "Ready" || m["status"] == "True" {
				continue
			}
			msg, _ := m["message"].(string)

			return msg
		}
	}

	return ""
}

// logNode writes one V(2) line for a node the pass applied or read. It logs only
// identities (names, GVK, target) and readiness, never object content: children
// may carry secrets in status. A single resource logs child (+childNamespace),
// a collection logs the children's names. A not-ready node adds its Ready
// condition's reasonCode as readyReason, when it has one (see readyReason).
func logNode(l logr.Logger, msg, nodeID string, target Target, gvk schema.GroupVersionKind,
	observed []*unstructured.Unstructured, collection bool, readyErr error, reasonCode string) {
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
		if reasonCode != "" {
			kv = append(kv, "readyReason", reasonCode)
		}
	}
	v.Info(msg, kv...)
}
