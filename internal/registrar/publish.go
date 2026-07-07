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

package registrar

import (
	"context"
	"fmt"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	graph "github.com/kubernetes-sigs/kro/pkg/graph"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fieldManager identifies the Registrar as the server-side-apply field owner.
const fieldManager = "krop-registrar"

// BuildARS converts the graph's generated instance CRD into an APIResourceSchema
// named v<specHash>.<plural>.<group>. CRDToAPIResourceSchema names the object
// prefix + "." + crd.Name, so the "v"+specHash prefix yields the
// v<hash>.<plural>.<group> convention (design §M4 grounding).
func BuildARS(g *graph.Graph, specHash string) (*apisv1alpha1.APIResourceSchema, error) {
	if g.CRD == nil {
		return nil, fmt.Errorf("graph has no generated CRD")
	}

	return apisv1alpha1.CRDToAPIResourceSchema(g.CRD, "v"+specHash)
}

// applyARS creates the APIResourceSchema if it does not already exist. An ARS is
// immutable once served, and a new specHash mints a new ARS name, so a
// create-if-not-exists is both sufficient and safe: an existing ARS of the same
// name/hash is left untouched (patching a served ARS would be rejected).
func applyARS(ctx context.Context, c client.Client, ars *apisv1alpha1.APIResourceSchema) error {
	existing := &apisv1alpha1.APIResourceSchema{}
	err := c.Get(ctx, types.NamespacedName{Name: ars.Name}, existing)
	if err == nil {
		return nil // already served; ARS is immutable, do not patch.
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting APIResourceSchema %q: %w", ars.Name, err)
	}
	if err := c.Create(ctx, ars); err != nil {
		return fmt.Errorf("creating APIResourceSchema %q: %w", ars.Name, err)
	}

	return nil
}

// identityByGroupResource lists APIBindings in the build workspace and maps each
// bound GroupResource to its identityHash (for resolving foreign-type claims).
// Core types are absent here and resolve to the empty identityHash downstream.
func identityByGroupResource(ctx context.Context, c client.Client) (map[schema.GroupResource]string, error) {
	var bindings apisv1alpha2.APIBindingList
	if err := c.List(ctx, &bindings); err != nil {
		return nil, fmt.Errorf("listing APIBindings: %w", err)
	}
	out := map[schema.GroupResource]string{}
	for i := range bindings.Items {
		for _, br := range bindings.Items[i].Status.BoundResources {
			out[schema.GroupResource{Group: br.Group, Resource: br.Resource}] = br.Schema.IdentityHash
		}
	}

	return out, nil
}

// UpsertAPIExport server-side-applies the APIExport referencing the ARS with the
// derived permissionClaims. exportName is the ARS's <plural>.<group>.
//
// No APIExportEndpointSlice is created here: kcp auto-creates the default
// APIExportEndpointSlice named after the export in the provider workspace
// (design §M2), which the supervisor discovers via internal/kcp.FindEndpointSlice.
func UpsertAPIExport(ctx context.Context, c client.Client, exportName string, ars *apisv1alpha1.APIResourceSchema, claims []apisv1alpha2.PermissionClaim) error {
	export := &apisv1alpha2.APIExport{}
	// A server-side apply patch is serialized to JSON verbatim, so apiVersion/kind
	// must be populated explicitly — the typed client does not inject them for an
	// Apply patch, and kcp rejects a body with an empty GVK ("invalid object type").
	export.SetGroupVersionKind(apisv1alpha2.SchemeGroupVersion.WithKind("APIExport"))
	export.SetName(exportName)
	export.Spec.Resources = []apisv1alpha2.ResourceSchema{{
		Name:   ars.Spec.Names.Plural,
		Group:  ars.Spec.Group,
		Schema: ars.Name,
		Storage: apisv1alpha2.ResourceSchemaStorage{
			CRD: &apisv1alpha2.ResourceSchemaStorageCRD{},
		},
	}}
	export.Spec.PermissionClaims = claims

	// client.Client.Apply supersedes the deprecated client.Apply patch value: it
	// takes a runtime.ApplyConfiguration. The typed object is converted verbatim
	// to unstructured (identical JSON body), preserving the explicit GVK above.
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(export)
	if err != nil {
		return fmt.Errorf("encoding APIExport %q for apply: %w", exportName, err)
	}
	ac := client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: raw})
	if err := c.Apply(ctx, ac, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying APIExport %q: %w", exportName, err)
	}

	return nil
}
