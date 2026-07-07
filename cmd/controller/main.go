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

// Command controller runs the krop instance reconciler over a blueprint's
// APIExport virtual workspace, one reconcile per consumer workspace served by
// the endpoint slice. M1 loads the single checked-in example blueprint at
// startup, reconciles the unstructured instance GVK, and routes children to the
// consumer (tenant) workspace only; the provider target arrives in M2.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/kcp-dev/multicluster-provider/apiexport"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	kropkcp "go.opendefense.cloud/krop-controller/internal/kcp"
)

// M1 fixed identifiers for the single hand-published blueprint. M4 replaces
// these with per-blueprint discovery via the Registrar.
const (
	apiExportName   = "kubernetesclusters.krop.opendefense.cloud"
	instanceGroup   = "krop.opendefense.cloud"
	instanceVersion = "v1alpha1"
	instanceKind    = "KubernetesCluster"
)

// requeueInterval bounds convergence when a child is applied but not yet ready:
// the engine reports a requeue and we re-drive after this interval rather than
// relying on the (deprecated) rate-limited Requeue flag.
const requeueInterval = 30 * time.Second

func main() {
	if err := run(); err != nil {
		ctrl.Log.WithName("entrypoint").Error(err, "fatal")
		os.Exit(1)
	}
}

func run() error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	ctx := signals.SetupSignalHandler()
	entryLog := log.Log.WithName("entrypoint")

	flag.Parse()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		// kcp SDK groups the apiexport provider reads: APIExportEndpointSlice /
		// APIBinding (apis), LogicalCluster (core), Workspace (tenancy).
		apisv1alpha1.AddToScheme,
		corev1alpha1.AddToScheme,
		tenancyv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return fmt.Errorf("registering scheme: %w", err)
		}
	}

	cfg := ctrl.GetConfigOrDie()
	if err := kropkcp.ValidateKubeconfig(cfg.Host); err != nil {
		return fmt.Errorf("kubeconfig must be workspace-scoped: %w", err)
	}

	// Build the compiled graph once at startup from the example blueprint.
	// (M4 replaces this with the Registrar; M1 loads the checked-in RGD.) The
	// graph source points at the same workspace config, which must serve
	// discovery/OpenAPI for every child GVK.
	graphSource, err := kropengine.NewEndpointGraphSource(cfg)
	if err != nil {
		return fmt.Errorf("graph source: %w", err)
	}
	rgd, err := kropengine.LoadExampleBlueprint()
	if err != nil {
		return fmt.Errorf("loading example blueprint: %w", err)
	}
	compiled, err := graphSource.Build(rgd)
	if err != nil {
		return fmt.Errorf("building graph: %w", err)
	}

	// Discover the endpoint slice for our APIExport using a one-shot client
	// against the configured workspace.
	bootstrapClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("creating bootstrap client: %w", err)
	}
	endpointSlice, err := kropkcp.FindEndpointSlice(ctx, bootstrapClient, apiExportName)
	if err != nil {
		return fmt.Errorf("discovering APIExportEndpointSlice: %w", err)
	}
	entryLog.Info("Using APIExportEndpointSlice", "name", endpointSlice, "apiExport", apiExportName)

	provider, err := apiexport.New(cfg, endpointSlice, apiexport.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("constructing apiexport provider: %w", err)
	}

	mgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		return fmt.Errorf("setting up manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}

	instanceGVK := schema.GroupVersionKind{Group: instanceGroup, Version: instanceVersion, Kind: instanceKind}

	// The provider workspace is the one this controller is configured against
	// (where the blueprint's APIExport lives); the engine's identity has RBAC
	// there, so provider-target children are written with a direct client.
	providerClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("provider client: %w", err)
	}
	reconciler := &kropctrl.Reconciler{
		Graph:          compiled,
		ProviderClient: providerClient,
		InstanceGVK:    instanceGVK,
	}

	reconcileFn := mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
		l := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "name", req.Name)

		cl, err := mgr.GetCluster(ctx, req.ClusterName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting cluster %q: %w", req.ClusterName, err)
		}

		res, err := reconciler.Reconcile(ctx, cl.GetClient(), string(req.ClusterName), req.NamespacedName)
		if err != nil {
			return ctrl.Result{}, err
		}

		if res.Requeue {
			l.Info("reconciled; requeueing", "ready", res.Ready)
			return ctrl.Result{RequeueAfter: requeueInterval}, nil
		}
		l.Info("reconciled", "ready", res.Ready)
		return ctrl.Result{}, nil
	})

	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("krop-instance").
		For(newInstance(instanceGVK)).
		Complete(reconcileFn); err != nil {
		return fmt.Errorf("building krop-instance controller: %w", err)
	}

	entryLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

// newInstance returns a fresh unstructured object typed to the instance GVK.
// The multicluster builder derives the watched type from the object's embedded
// GVK, so the instance kind needs no scheme registration.
func newInstance(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}
