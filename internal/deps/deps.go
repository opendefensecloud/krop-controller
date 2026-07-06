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

// Package deps is a TEMPORARY M1 import anchor. It exists only so that
// `go mod tidy` keeps the kcp + multicluster-runtime modules that M1 will
// build on (the apiexport provider, multicluster manager/builder/reconcile,
// and the kcp SDK API groups) as direct dependencies before any real
// controller code exists. It also proves the whole stack compiles together
// against the resolved go.mod. DELETE THIS FILE once the M1 controller wires
// these packages up for real.
package deps

import (
	// kcp multicluster-provider: apiexport-backed provider + typed client.
	_ "github.com/kcp-dev/multicluster-provider/apiexport"
	_ "github.com/kcp-dev/multicluster-provider/client"

	// kcp SDK API groups M1 needs (APIExport/APIBinding, core logicalcluster,
	// tenancy workspaces).
	_ "github.com/kcp-dev/logicalcluster/v3"
	_ "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	_ "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	_ "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	_ "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"

	// multicluster-runtime: manager (mcmanager), builder (mcbuilder),
	// reconcile (mcreconcile).
	_ "sigs.k8s.io/multicluster-runtime/pkg/builder"
	_ "sigs.k8s.io/multicluster-runtime/pkg/manager"
	_ "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)
