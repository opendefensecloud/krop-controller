// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"github.com/go-logr/logr"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// InstanceLogConstructor returns a multicluster-runtime LogConstructor that tags
// every reconcile line with the blueprint/export/kind and the request's cluster,
// namespace and name (ADR 0013). controller-runtime builds its own "Reconciler
// error" line from this logger too, so errors carry the instance fields. It also
// calls the constructor with a nil request (e.g. on controller start), which
// yields the blueprint-level fields only.
func InstanceLogConstructor(base logr.Logger, blueprint, export, kind string) func(*mcreconcile.Request) logr.Logger {
	l := base.WithValues("blueprint", blueprint, "export", export, "kind", kind)

	return func(req *mcreconcile.Request) logr.Logger {
		if req == nil {
			return l
		}

		return l.WithValues("cluster", string(req.ClusterName), "namespace", req.Namespace, "name", req.Name)
	}
}
