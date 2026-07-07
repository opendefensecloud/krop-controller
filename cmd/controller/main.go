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

// Command controller runs the krop control plane over a provider workspace. It
// runs two things against the same workspace-scoped kubeconfig:
//
//   - A provider-workspace controller-runtime manager running the Registrar,
//     which watches cluster-scoped ResourceGraphDefinition blueprints, compiles
//     each into an APIExport, and (on publish) records the compiled graph +
//     instance GVK and asks the Supervisor to serve it.
//   - The Supervisor's per-blueprint instance-serving managers: for each
//     published APIExport it discovers the endpoint slice, builds an apiexport
//     multicluster provider + manager, and reconciles the generated instance
//     kind (one reconcile per consumer workspace) with the cached graph.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kcp-dev/multicluster-provider/apiexport"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	kropkcp "go.opendefense.cloud/krop-controller/internal/kcp"
	"go.opendefense.cloud/krop-controller/internal/registrar"
	"go.opendefense.cloud/krop-controller/internal/supervisor"
)

// requeueInterval bounds convergence when a child is applied but not yet ready:
// the engine reports a requeue and we re-drive after this interval rather than
// relying on the (deprecated) rate-limited Requeue flag.
const requeueInterval = 30 * time.Second

// endpointSlicePollInterval / endpointSliceTimeout bound the wait for the
// APIExportEndpointSlice, which kcp auto-creates shortly after the Registrar
// publishes the APIExport. The slice does not exist at the instant OnPublished
// fires, so the per-export start polls for it.
const (
	endpointSlicePollInterval = 2 * time.Second
	endpointSliceTimeout      = 30 * time.Second
)

// servedBlueprint is what the Registrar records on publish and the per-export
// manager needs to serve: the compiled graph and the generated instance kind.
type servedBlueprint struct {
	graph *krograph.Graph
	gvk   schema.GroupVersionKind
}

// published is a thread-safe registry of served blueprints keyed by export name.
// The Registrar's OnPublished writes it; the Supervisor's start reads it.
type published struct {
	mu sync.Mutex
	m  map[string]servedBlueprint
}

func (p *published) Get(export string) (servedBlueprint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.m[export]
	return sb, ok
}

func (p *published) Set(export string, sb servedBlueprint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[export] = sb
}

// version is the build version, injected at link time via
// -ldflags "-X main.version=...". It defaults to "dev" for local builds.
var version = "dev"

func main() {
	if err := run(); err != nil {
		ctrl.Log.WithName("entrypoint").Error(err, "fatal")
		os.Exit(1)
	}
}

func run() error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	rootCtx := signals.SetupSignalHandler()
	entryLog := log.Log.WithName("entrypoint")
	entryLog.Info("starting krop-controller", "version", version)

	flag.Parse()

	cfg := ctrl.GetConfigOrDie()
	if err := kropkcp.ValidateKubeconfig(cfg.Host); err != nil {
		return fmt.Errorf("kubeconfig must be workspace-scoped: %w", err)
	}

	// Provider-workspace scheme: the blueprint CRD (kropv1alpha1) the Registrar
	// watches, plus the kcp apis groups it upserts (APIResourceSchema /
	// APIExportEndpointSlice live in v1alpha1; APIExport / APIBinding /
	// PermissionClaim in v1alpha2), plus the core client-go types.
	providerScheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		kropv1alpha1.AddToScheme,
		apisv1alpha1.AddToScheme,
		apisv1alpha2.AddToScheme,
	} {
		if err := add(providerScheme); err != nil {
			return fmt.Errorf("registering provider scheme: %w", err)
		}
	}

	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:                 providerScheme,
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		return fmt.Errorf("setting up provider manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}

	// providerClient is a direct (non-cached) client against the provider
	// workspace. It is the Registrar's Client, the instance Reconciler's
	// ProviderClient (provider-target children), and the endpoint-slice lister.
	// Direct so it works before the manager's cache starts and needs no informer
	// for the kcp apis types.
	providerClient, err := client.New(cfg, client.Options{Scheme: providerScheme})
	if err != nil {
		return fmt.Errorf("provider client: %w", err)
	}

	graphSource, err := kropengine.NewEndpointGraphSource(cfg)
	if err != nil {
		return fmt.Errorf("graph source: %w", err)
	}

	registry := &published{m: map[string]servedBlueprint{}}

	// startFn is the per-blueprint instance-serving manager: the old single-
	// APIExport main body, parameterized by export name and driven off the
	// registry the Registrar populates on publish. It blocks until ctx (the
	// Supervisor's per-export context) is cancelled.
	//
	// M4b limitation: a live blueprint SPEC CHANGE keeps serving the OLD graph
	// until this manager is restarted — the registry Set + supervisor Ensure are
	// both no-ops while the manager runs, so the reconciler holds its original
	// graph. Multi-version / hot-swap serving is deferred (see M4b notes).
	startFn := func(ctx context.Context, exportName string) error {
		exportLog := entryLog.WithValues("apiExport", exportName)

		sb, ok := registry.Get(exportName)
		if !ok {
			// OnPublished sets the registry before calling Ensure, so this
			// should never happen; treat it as a hard error.
			return fmt.Errorf("no served blueprint recorded for export %q", exportName)
		}

		// The endpoint slice is auto-created shortly after the APIExport is
		// published; poll until it appears (FindEndpointSlice returns a plain
		// error when no matching slice exists, so retry on any error).
		var sliceName string
		if err := wait.PollUntilContextTimeout(ctx, endpointSlicePollInterval, endpointSliceTimeout, true,
			func(ctx context.Context) (bool, error) {
				name, err := kropkcp.FindEndpointSlice(ctx, providerClient, exportName)
				if err != nil {
					exportLog.V(1).Info("waiting for APIExportEndpointSlice", "error", err.Error())
					return false, nil
				}
				sliceName = name
				return true, nil
			}); err != nil {
			return fmt.Errorf("waiting for APIExportEndpointSlice for %q: %w", exportName, err)
		}
		exportLog.Info("using APIExportEndpointSlice", "slice", sliceName)

		// Instance-serving scheme: the instance kind is unstructured, but the
		// apiexport provider reads kcp APIBinding / LogicalCluster / Workspace
		// off the virtual workspace, so register those groups too.
		instanceScheme := runtime.NewScheme()
		for _, add := range []func(*runtime.Scheme) error{
			clientgoscheme.AddToScheme,
			apisv1alpha1.AddToScheme,
			corev1alpha1.AddToScheme,
			tenancyv1alpha1.AddToScheme,
		} {
			if err := add(instanceScheme); err != nil {
				return fmt.Errorf("registering instance scheme: %w", err)
			}
		}

		provider, err := apiexport.New(cfg, sliceName, apiexport.Options{Scheme: instanceScheme})
		if err != nil {
			return fmt.Errorf("constructing apiexport provider for %q: %w", exportName, err)
		}

		imgr, err := mcmanager.New(cfg, provider, manager.Options{Scheme: instanceScheme})
		if err != nil {
			return fmt.Errorf("setting up instance manager for %q: %w", exportName, err)
		}

		reconciler := &kropctrl.Reconciler{
			Graph:          sb.graph,
			ProviderClient: providerClient,
			InstanceGVK:    sb.gvk,
			BlueprintName:  exportName,
		}

		reconcileFn := mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			l := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "name", req.Name)

			cl, err := imgr.GetCluster(ctx, req.ClusterName)
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

		if err := mcbuilder.ControllerManagedBy(imgr).
			Named("krop-instance-" + exportName).
			For(newInstance(sb.gvk)).
			Complete(reconcileFn); err != nil {
			return fmt.Errorf("building krop-instance controller for %q: %w", exportName, err)
		}

		exportLog.Info("starting instance manager")
		return imgr.Start(ctx)
	}

	sup := supervisor.New(startFn)

	// Workspace is the graph-cache key: derive it from the workspace-scoped host
	// (the /clusters/<name> segment). It only needs to be stable + unique per
	// provider workspace.
	workspace := providerWorkspaceName(cfg.Host)

	reg := &registrar.Registrar{
		Client:    providerClient,
		Workspace: workspace,
		Cache:     registrar.NewGraphCache(),
		Source:    graphSource,
		OnPublished: func(exportName string, instanceGVK schema.GroupVersionKind, g *krograph.Graph) {
			registry.Set(exportName, servedBlueprint{graph: g, gvk: instanceGVK})
			sup.Ensure(rootCtx, exportName)
		},
		OnDeleted: func(export string) {
			if export != "" {
				sup.Stop(export)
			}
		},
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&kropv1alpha1.ResourceGraphDefinition{}).
		Named("krop-registrar").
		Complete(reconcile.Func(reg.Reconcile)); err != nil {
		return fmt.Errorf("building registrar controller: %w", err)
	}

	entryLog.Info("starting provider manager", "workspace", workspace)
	if err := mgr.Start(rootCtx); err != nil {
		return fmt.Errorf("running provider manager: %w", err)
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

// providerWorkspaceName extracts the logical-cluster name from a workspace-scoped
// host (the segment after /clusters/, up to the next /). ValidateKubeconfig has
// already ensured the /clusters/ segment is present. Falls back to the full host
// so the graph-cache key stays stable + unique per workspace regardless.
func providerWorkspaceName(host string) string {
	const marker = "/clusters/"
	i := strings.Index(host, marker)
	if i < 0 {
		return host
	}
	name := host[i+len(marker):]
	if j := strings.IndexByte(name, '/'); j >= 0 {
		name = name[:j]
	}
	if name == "" {
		return host
	}
	return name
}
