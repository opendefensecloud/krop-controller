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
	"reflect"
	"testing"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"go.opendefense.cloud/krop-controller/internal/testlog"
)

func reviewScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := apisv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := apisv1alpha2.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}

func exportWithSchemas(name string, schemas ...string) *apisv1alpha2.APIExport {
	e := &apisv1alpha2.APIExport{}
	e.SetName(name)
	for _, s := range schemas {
		e.Spec.Resources = append(e.Spec.Resources, apisv1alpha2.ResourceSchema{Name: "widgets", Group: "example.com", Schema: s})
	}

	return e
}

func arsNamed(name string) *apisv1alpha1.APIResourceSchema {
	s := &apisv1alpha1.APIResourceSchema{}
	s.SetName(name)

	return s
}

func dumpLog(t *testing.T, sink *testlog.Sink) {
	t.Helper()
	for _, l := range sink.Lines() {
		t.Logf("log: %s", l.Raw)
	}
}

// a schema that was already gone must not be
// listed as deleted.
func TestDeletePublishedAPI_SchemasListsOnlyDeleted(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(reviewScheme(t)).
		WithObjects(exportWithSchemas("widgets.example.com", "v1.widgets.example.com", "v0.gone.example.com"),
			arsNamed("v1.widgets.example.com")). // v0.gone.example.com does not exist
		Build()

	lg, sink := testlog.New(0)
	if err := DeletePublishedAPI(log.IntoContext(context.Background(), lg), c, "widgets.example.com"); err != nil {
		t.Fatalf("DeletePublishedAPI: %v", err)
	}
	dumpLog(t, sink)

	lines := sink.Find("deleted APIExport")
	if len(lines) != 1 {
		t.Fatalf("want one deleted APIExport line, got %d", len(lines))
	}
	got := lines[0].Values["schemas"]
	want := []any{"v1.widgets.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schemas = %v, want %v (only the schema krop actually deleted)", got, want)
	}
}

// when the APIExport vanishes between Get and Delete,
// the schemas krop just deleted must still be logged.
func TestDeletePublishedAPI_SchemaDeletionLoggedWhenExportGone(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(reviewScheme(t)).
		WithObjects(exportWithSchemas("widgets.example.com", "v1.widgets.example.com"),
			arsNamed("v1.widgets.example.com")).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*apisv1alpha2.APIExport); ok {
					// Someone else deleted it after our Get.
					return apierrors.NewNotFound(apisv1alpha2.Resource("apiexports"), obj.GetName())
				}

				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	lg, sink := testlog.New(0)
	if err := DeletePublishedAPI(log.IntoContext(context.Background(), lg), c, "widgets.example.com"); err != nil {
		t.Fatalf("DeletePublishedAPI: %v", err)
	}
	dumpLog(t, sink)

	if err := c.Get(context.Background(), client.ObjectKey{Name: "v1.widgets.example.com"}, arsNamed("")); !apierrors.IsNotFound(err) {
		t.Fatalf("precondition: schema should be deleted, Get err = %v", err)
	}
	lines := sink.Find("deleted APIResourceSchemas")
	if len(lines) != 1 || lines[0].Level != 0 || lines[0].Values["export"] != "widgets.example.com" ||
		!reflect.DeepEqual(lines[0].Values["schemas"], []any{"v1.widgets.example.com"}) {
		t.Errorf("want one default-level deleted APIResourceSchemas line listing v1.widgets.example.com, got: %s", sink.Raw())
	}
	if n := len(sink.Find("deleted APIExport")); n != 0 {
		t.Errorf("krop did not delete the APIExport, but %d deleted APIExport line(s) were logged", n)
	}
}

// A failing schema delete aborts the withdrawal, but the schemas deleted before it
// are gone and must still be logged.
func TestDeletePublishedAPI_LogsSchemasDeletedBeforeError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(reviewScheme(t)).
		WithObjects(exportWithSchemas("widgets.example.com", "v1.widgets.example.com", "v2.widgets.example.com"),
			arsNamed("v1.widgets.example.com"), arsNamed("v2.widgets.example.com")).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "v2.widgets.example.com" {
					return apierrors.NewForbidden(apisv1alpha1.Resource("apiresourceschemas"), obj.GetName(), nil)
				}

				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	lg, sink := testlog.New(0)
	if err := DeletePublishedAPI(log.IntoContext(context.Background(), lg), c, "widgets.example.com"); err == nil {
		t.Fatal("DeletePublishedAPI: want the forbidden schema delete to fail")
	}
	dumpLog(t, sink)

	lines := sink.Find("deleted APIResourceSchemas")
	if len(lines) != 1 || !reflect.DeepEqual(lines[0].Values["schemas"], []any{"v1.widgets.example.com"}) ||
		lines[0].Values["export"] != "widgets.example.com" {
		t.Fatalf("want one deleted APIResourceSchemas line listing v1.widgets.example.com, got: %s", sink.Raw())
	}
}
