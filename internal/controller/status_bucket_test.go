// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	memory "k8s.io/client-go/discovery/cached/memory"
	restmapper "k8s.io/client-go/restmapper"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

var (
	bucketGVK        = schema.GroupVersionKind{Group: "bucket.opendefense.cloud", Version: "v1alpha1", Kind: "Bucket"}
	bucketProjectGVK = schema.GroupVersionKind{Group: "project.opendefense.cloud", Version: "v1alpha1", Kind: "Project"}
	bucketStorageGVK = schema.GroupVersionKind{Group: "storage.cnrm.cloud.google.com", Version: "v1beta1", Kind: "StorageBucket"}
)

// bucketRGD: project (consumer externalRef) -> bucket (host StorageBucket,
// ready on its Ready condition) -> credentials (consumer ConfigMap standing in
// for the Secret, carrying the bucket URL). status.url reads the last node.
// credentialsIncludeWhen, if set, gates the credentials node.
func bucketRGD(credentialsIncludeWhen ...string) *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"bucket",
		generator.WithSchema("Bucket", "v1alpha1",
			map[string]any{"name": "string", "projectRef": "string", "retentionDays": `string | default="0"`},
			map[string]any{"bucketId": "${bucket.spec.resourceID}", "url": "${credentials.data.url}"},
		),
		generator.WithExternalRef("project", &krov1alpha1.ExternalRef{
			APIVersion: bucketProjectGVK.GroupVersion().String(), Kind: bucketProjectGVK.Kind,
			Metadata: krov1alpha1.ExternalRefMetadata{Name: "${schema.spec.projectRef}"},
		}, nil, nil),
		generator.WithResource("bucket", map[string]any{
			"apiVersion": bucketStorageGVK.GroupVersion().String(), "kind": bucketStorageGVK.Kind,
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}",
				"namespace": "default",
				"labels":    map[string]any{"retention-days": "${string(int(schema.spec.retentionDays))}"},
			},
			"spec": map[string]any{"resourceID": `${project.status.projectId + "-" + schema.spec.name}`},
		}, []string{`${bucket.status.conditions.exists(c, c.type == "Ready" && c.status == "True")}`}, nil),
		generator.WithResource("credentials", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${schema.metadata.name}-credentials", "namespace": "default"},
			"data":     map[string]any{"url": "${bucket.status.url}"},
		}, nil, credentialsIncludeWhen),
	)
	rgd.Spec.Schema.Group = bucketGVK.Group

	return rgd
}

// bucketObjectSchema is the OpenAPI schema of a kind kro type-checks the
// blueprint against: typed metadata, spec and status properties.
func bucketObjectSchema(specProps, statusProps map[string]spec.Schema) *spec.Schema {
	return &spec.Schema{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			"metadata":   bucketMetadataSchema(),
			"spec":       {SchemaProps: spec.SchemaProps{Type: []string{"object"}, Properties: specProps}},
			"status":     {SchemaProps: spec.SchemaProps{Type: []string{"object"}, Properties: statusProps}},
		}}
}

// bucketMetadataSchema types the metadata fields the blueprint reads and sets.
func bucketMetadataSchema() spec.Schema {
	str := spec.Schema{Type: []string{"string"}}
	strMap := spec.Schema{Type: []string{"object"}, AdditionalProperties: &spec.SchemaOrBool{Allows: true, Schema: &str}}

	return spec.Schema{Type: []string{"object"}, Properties: map[string]spec.Schema{
		"name": str, "namespace": str, "labels": strMap, "annotations": strMap,
	}}
}

// buildBucketGraph compiles bucketRGD against kro's fake resolver, extended with
// the Project and StorageBucket kinds the blueprint uses.
func buildBucketGraph(t *testing.T, credentialsIncludeWhen ...string) *krograph.Graph {
	t.Helper()
	str := spec.Schema{Type: []string{"string"}}
	cond := spec.Schema{Type: []string{"object"}, Properties: map[string]spec.Schema{
		"type": str, "status": str, "reason": str, "message": str,
	}}
	res, disc := testk8s.NewFakeResolver()
	res.AddSchema(bucketProjectGVK, bucketObjectSchema(nil, map[string]spec.Schema{"projectId": str}))
	res.AddSchema(bucketStorageGVK, bucketObjectSchema(
		map[string]spec.Schema{"resourceID": str},
		map[string]spec.Schema{
			"url":        str,
			"conditions": {SchemaProps: spec.SchemaProps{Type: []string{"array"}, Items: &spec.SchemaOrArray{Schema: &cond}}},
		}))
	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	disc.Resources = append(disc.Resources,
		&metav1.APIResourceList{GroupVersion: bucketProjectGVK.GroupVersion().String(), APIResources: []metav1.APIResource{
			{Name: "projects", Namespaced: true, Kind: "Project", Verbs: verbs},
		}},
		&metav1.APIResourceList{GroupVersion: bucketStorageGVK.GroupVersion().String(), APIResources: []metav1.APIResource{
			{Name: "storagebuckets", Namespaced: true, Kind: "StorageBucket", Verbs: verbs},
		}},
	)
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	b := &krograph.Builder{}
	setBuilderField(b, "schemaResolver", res)
	setBuilderField(b, "restMapper", meta.RESTMapper(rm))
	g, err := b.NewResourceGraphDefinition(bucketRGD(credentialsIncludeWhen...), krograph.Config{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}

	return g
}

// kccReady is a Config Connector status with only a Ready condition.
func kccReady(status, reason, message string) map[string]any {
	return map[string]any{"conditions": []any{map[string]any{
		"type": "Ready", "status": status, "reason": reason, "message": message,
	}}}
}

// TestReconcile_BucketStatus drives a Bucket-shaped blueprint through the
// failure cases of #49 and checks the status the consumer sees: the Ready reason
// and message, the progress and the state. Nothing from the host may reach the
// message; run with -v to see each status.
func TestReconcile_BucketStatus(t *testing.T) {
	g := buildBucketGraph(t)
	// The same blueprint with credentials gated on the bucket: a not-ready bucket
	// makes the gate itself pend, which the engine returns as an error.
	gGated := buildBucketGraph(t, `${bucket.status.url != ""}`)
	kyverno := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 400, Reason: metav1.StatusReasonBadRequest,
		Message: `admission webhook "vpol.validate.kyverno.svc-fail-finegrained-exclusive-storage-buckets" denied the request: ` +
			`Policy exclusive-storage-buckets failed: Another StorageBucket already uses the GCS name acme-prod-demo.`,
	}}

	bucketReady := map[string]any{"url": "gs://acme-prod-demo", "conditions": kccReady("True", "UpToDate", "")["conditions"]}

	cases := []struct {
		name          string
		gated         bool // credentials behind includeWhen (gGated)
		generation    int64
		retentionDays string
		noProject     bool
		projectErr    error
		bucketStatus  map[string]any
		applyErr      error
		consumerErr   error
		prevStatus    map[string]any // the instance's status before the pass
		wantStatus    map[string]any // blueprint status fields after it; nil = absent
		wantReason    string
		wantMessage   string
		wantProgress  string
		wantState     string
	}{
		{name: "bucket still creating", generation: 1,
			bucketStatus: kccReady("False", "Updating", "Update in progress"),
			wantReason:   reasonInProgress, wantMessage: "bucket: not ready (Updating), ref uid-1", wantProgress: "33%", wantState: stateInProgress},
		{name: "cloud rejects bucket (KCC UpdateFailed)", generation: 1,
			bucketStatus: kccReady("False", "UpdateFailed", "Update call failed: error creating bucket: googleapi: Error 409: Your previous request to create the named bucket succeeded and you already own it., conflict"),
			wantReason:   reasonInProgress, wantMessage: "bucket: not ready (UpdateFailed), ref uid-1", wantProgress: "33%", wantState: stateInProgress},
		{name: "host admission denies bucket (Kyverno)", generation: 2,
			applyErr:   kyverno,
			wantReason: reasonApplyFailed, wantMessage: "bucket: the provider could not apply the resource (BadRequest), ref uid-1", wantProgress: "33%", wantState: stateError},
		{name: "host API server times out", generation: 1,
			applyErr:   apierrors.NewServerTimeout(schema.GroupResource{Group: "storage.cnrm.cloud.google.com", Resource: "storagebuckets"}, "apply", 1),
			wantReason: reasonApplyFailed, wantMessage: "bucket: the provider could not apply the resource (ServerTimeout), ref uid-1", wantProgress: "33%", wantState: stateInProgress},
		{name: "referenced project missing", generation: 1, noProject: true,
			wantReason: reasonReferenceNotFound, wantMessage: `project: projects.project.opendefense.cloud "acme" not found`, wantProgress: "0%", wantState: stateInProgress},
		{name: "project claim not usable (forbidden)", generation: 1,
			projectErr: apierrors.NewForbidden(schema.GroupResource{Group: bucketProjectGVK.Group, Resource: "projects"}, "acme", fmt.Errorf("permission claim not accepted")),
			wantReason: reasonReferenceFailed, wantMessage: `project: projects.project.opendefense.cloud "acme" is forbidden: permission claim not accepted`, wantProgress: "0%", wantState: stateError},
		{name: "bad spec value (CEL type error)", generation: 3, retentionDays: "thirty",
			wantReason: reasonEvaluationFailed, wantMessage: "bucket: cannot evaluate the blueprint with spec.retentionDays, ref uid-1", wantProgress: "33%", wantState: stateError},
		{name: "consumer workspace refuses credentials (conflict)", generation: 1,
			bucketStatus: bucketReady,
			consumerErr: apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "demo-credentials",
				fmt.Errorf(`Apply failed with 1 conflict: conflict with "kubectl-client-side-apply": .data.url`)),
			wantReason:   reasonApplyFailed,
			wantMessage:  `credentials: Operation cannot be fulfilled on configmaps "demo-credentials": Apply failed with 1 conflict: conflict with "kubectl-client-side-apply": .data.url`,
			wantProgress: "66%", wantState: stateInProgress},
		{name: "failed pass keeps the fields of nodes it did not reach", generation: 1,
			bucketStatus: bucketReady,
			consumerErr:  apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "demo-credentials", fmt.Errorf("conflict")),
			prevStatus:   map[string]any{"bucketId": "stale", "url": "gs://acme-prod-demo"},
			wantStatus:   map[string]any{"bucketId": "acme-prod-demo", "url": "gs://acme-prod-demo"},
			wantReason:   reasonApplyFailed,
			wantMessage:  `credentials: Operation cannot be fulfilled on configmaps "demo-credentials": conflict`,
			wantProgress: "66%", wantState: stateInProgress},
		{name: "pending pass drops the fields of nodes it could not resolve", generation: 1,
			bucketStatus: kccReady("False", "Updating", "Update in progress"),
			prevStatus:   map[string]any{"bucketId": "stale", "url": "gs://acme-prod-demo"},
			wantStatus:   map[string]any{"bucketId": "acme-prod-demo", "url": nil},
			wantReason:   reasonInProgress, wantMessage: "bucket: not ready (Updating), ref uid-1", wantProgress: "33%", wantState: stateInProgress},
		{name: "pending includeWhen drops the fields of nodes it could not resolve", generation: 1, gated: true,
			bucketStatus: kccReady("False", "Updating", "Update in progress"),
			prevStatus:   map[string]any{"bucketId": "stale", "url": "gs://acme-prod-demo"},
			wantStatus:   map[string]any{"bucketId": "acme-prod-demo", "url": nil},
			wantReason:   reasonInProgress, wantMessage: "bucket: not ready (Updating), ref uid-1", wantProgress: "33%", wantState: stateInProgress},
		{name: "pending pass that resolves no field clears them all", generation: 1, noProject: true,
			prevStatus: map[string]any{"bucketId": "acme-prod-demo", "url": "gs://acme-prod-demo"},
			wantStatus: map[string]any{"bucketId": nil, "url": nil},
			wantReason: reasonReferenceNotFound, wantMessage: `project: projects.project.opendefense.cloud "acme" not found`, wantProgress: "0%", wantState: stateInProgress},
		{name: "all ready", generation: 1,
			bucketStatus: bucketReady,
			wantStatus:   map[string]any{"bucketId": "acme-prod-demo", "url": "gs://acme-prod-demo"},
			wantReason:   reasonReady, wantMessage: "all resources are ready", wantProgress: "100%", wantState: stateActive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			inst := &unstructured.Unstructured{}
			inst.SetGroupVersionKind(bucketGVK)
			inst.SetName("demo")
			inst.SetNamespace("default")
			inst.SetUID("uid-1")
			inst.SetGeneration(tc.generation)
			inst.SetFinalizers([]string{kropengine.Finalizer})
			_ = unstructured.SetNestedField(inst.Object, "demo", "spec", "name")
			_ = unstructured.SetNestedField(inst.Object, "acme", "spec", "projectRef")
			_ = unstructured.SetNestedField(inst.Object, tc.retentionDays, "spec", "retentionDays")
			if tc.retentionDays == "" {
				_ = unstructured.SetNestedField(inst.Object, "0", "spec", "retentionDays")
			}
			if tc.prevStatus != nil {
				inst.Object["status"] = tc.prevStatus
			}

			objs := []client.Object{inst}
			if !tc.noProject {
				p := &unstructured.Unstructured{}
				p.SetGroupVersionKind(bucketProjectGVK)
				p.SetName("acme")
				p.SetNamespace("default")
				_ = unstructured.SetNestedField(p.Object, "acme-prod", "status", "projectId")
				objs = append(objs, p)
			}
			consumer := fake.NewClientBuilder().WithObjects(objs...).WithStatusSubresource(inst).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if tc.projectErr != nil && obj.GetObjectKind().GroupVersionKind() == bucketProjectGVK {
							return tc.projectErr
						}

						return c.Get(ctx, key, obj, opts...)
					},
					Apply: func(ctx context.Context, c client.WithWatch, obj k8sruntime.ApplyConfiguration, opts ...client.ApplyOption) error {
						if tc.consumerErr != nil {
							return tc.consumerErr
						}

						return c.Apply(ctx, obj, opts...)
					},
				}).Build()

			// The host stands in for Config Connector: Apply either is denied or
			// succeeds, and Get returns the bucket with the applied spec and the
			// status KCC would set.
			host := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(context.Context, client.WithWatch, k8sruntime.ApplyConfiguration, ...client.ApplyOption) error {
					return tc.applyErr
				},
				Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
					u := obj.(*unstructured.Unstructured)
					u.SetName(key.Name)
					u.SetNamespace(key.Namespace)
					_ = unstructured.SetNestedField(u.Object, "acme-prod-demo", "spec", "resourceID")
					if tc.bucketStatus != nil {
						u.Object["status"] = tc.bucketStatus
					}

					return nil
				},
			}).Build()

			graph := g
			if tc.gated {
				graph = gGated
			}
			r := &Reconciler{
				Graph:          graph,
				ProviderClient: fake.NewClientBuilder().Build(),
				HostClient:     host,
				InstanceGVK:    bucketGVK,
				BlueprintName:  "buckets",
				Routing: map[string]kropengine.Target{
					"project": kropengine.TargetConsumer, "bucket": kropengine.TargetHost, "credentials": kropengine.TargetConsumer,
				},
			}
			res, err := r.Reconcile(ctx, consumer, "cluster1", client.ObjectKeyFromObject(inst))

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(bucketGVK)
			if gerr := consumer.Get(ctx, client.ObjectKeyFromObject(inst), got); gerr != nil {
				t.Fatalf("get instance: %v", gerr)
			}
			state, _, _ := unstructured.NestedString(got.Object, "status", "state")
			ready := meta.FindStatusCondition(readConditions(got), conditionReady)
			progressing := meta.FindStatusCondition(readConditions(got), conditionProgressing)
			out, _ := yaml.Marshal(got.Object["status"])
			t.Logf("outcome=%s stage=%s err=%v status:\n%s", res.Outcome, res.Stage, err, out)

			if ready.Reason != tc.wantReason || ready.Message != tc.wantMessage {
				t.Errorf("Ready = %s %q, want %s %q", ready.Reason, ready.Message, tc.wantReason, tc.wantMessage)
			}
			if progressing.Message != tc.wantProgress || state != tc.wantState {
				t.Errorf("progress %q state %q, want %q %q", progressing.Message, state, tc.wantProgress, tc.wantState)
			}
			// The reconcile error is logged without the instance's fields; it names
			// the UID so the ref in the Ready message finds it.
			if err != nil && !strings.Contains(err.Error(), "instance uid-1: ") {
				t.Errorf("reconcile error does not name the instance UID: %v", err)
			}
			// Nothing from the host may reach the consumer (#49).
			for _, hostText := range []string{"kyverno", "GCS name", "googleapi", "acme-prod", "host"} {
				if strings.Contains(ready.Message, hostText) {
					t.Errorf("Ready message leaks host text %q: %s", hostText, ready.Message)
				}
			}
			for field, want := range tc.wantStatus {
				v, found, _ := unstructured.NestedFieldNoCopy(got.Object, "status", field)
				if want == nil && found {
					t.Errorf("status.%s = %v, want absent", field, v)
				}
				if want != nil && v != want {
					t.Errorf("status.%s = %v, want %v", field, v, want)
				}
			}
			if ready.ObservedGeneration != tc.generation {
				t.Errorf("observedGeneration=%d, want %d", ready.ObservedGeneration, tc.generation)
			}
		})
	}
}
