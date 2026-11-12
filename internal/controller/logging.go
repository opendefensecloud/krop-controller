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
