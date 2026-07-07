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
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var testGVK = schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesCluster"}

func TestReconciler_InstanceNotFound_NoError(t *testing.T) {
	consumer := fake.NewClientBuilder().Build()
	r := &Reconciler{Graph: nil, ProviderClient: consumer, InstanceGVK: testGVK}

	// No instance exists → IgnoreNotFound → no error, empty result.
	_, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "missing"})
	if err != nil {
		t.Fatalf("expected nil error on not-found, got %v", err)
	}
}
