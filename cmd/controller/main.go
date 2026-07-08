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

	"github.com/kcp-dev/multicluster-provider/apiexport"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
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

// Orphan-sweep tuning (idea.md §11). A live instance refreshes its provider-side
// liveness record on every complete pass, which the reconciler requeues every
// requeueInterval (~30s). The sweeper deems a record's instance orphaned once its
// lastReconciled is older than orphanStaleAfter and reclaims its provider
// children. orphanStaleAfter MUST comfortably exceed requeueInterval so a live
// instance whose refresh merely lagged is never swept — 5min gives ~10x margin.
const (
	orphanStaleAfter    = 5 * time.Minute
	orphanSweepInterval = 1 * time.Minute
)

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
	graph   *krograph.Graph
	gvk     schema.GroupVersionKind
	routing map[string]kropengine.Target
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

// Delete drops the served blueprint for export (blueprint withdrawal cleanup).
func (p *published) Delete(export string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, export)
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
	// Operability flags for the deployed controller (wired through the Helm
	// chart). Register them on the default flag set BEFORE flag.Parse().
	var (
		healthProbeAddr = flag.String("health-probe-bind-address", ":8081",
			"The address the health probe (healthz/readyz) endpoint binds to.")
		// Only the single PROVIDER manager serves metrics. The per-blueprint
		// instance managers keep metrics disabled (BindAddress "0") to avoid a
		// port collision — see the instance manager construction below.
		metricsAddr = flag.String("metrics-bind-address", ":8080",
			"The address the provider manager's metrics endpoint binds to. Set to '0' to disable.")
		// Leader election lets HA (replicaCount>1) run a single active manager.
		// It creates a coordination.k8s.io Lease in the provider workspace, in
		// the configured leader-election-namespace. Leaving it off (the default)
		// needs no Lease RBAC.
		leaderElect = flag.Bool("leader-elect", false,
			"Enable leader election, ensuring only one active controller manager. Required for HA (replicaCount>1).")
		// controller-runtime needs an explicit namespace for the Lease when the
		// client is scoped to a kcp workspace (no in-cluster namespace to infer).
		// Default to the pod namespace via POD_NAMESPACE, falling back to "default".
		leaderElectNamespace = flag.String("leader-election-namespace", podNamespace(),
			"The namespace in which the leader-election Lease is created (in the provider workspace).")
		// hostKubeconfig points at the physical host cluster into which host-target
		// children are provisioned. Empty falls back to in-cluster config; when
		// neither is available the host target is disabled (blueprints routing to
		// host then fail loudly via the engine's missing-applier error).
		hostKubeconfig = flag.String("host-kubeconfig", "",
			"Path to a kubeconfig for the host cluster to provision host-target children into. Defaults to in-cluster config; empty + no in-cluster config disables the host target.")
	)

	// zap logging flags (--zap-log-level, --zap-devel, --zap-encoder, ...). The
	// zap.Options zero value defaults to production JSON encoding, which is the
	// right default for a deployed controller; --zap-devel opts into dev mode.
	var zapOpts zap.Options
	zapOpts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	rootCtx := signals.SetupSignalHandler()
	entryLog := log.Log.WithName("entrypoint")
	entryLog.Info("starting krop-controller",
		"version", version,
		"healthProbeBindAddress", *healthProbeAddr,
		"metricsBindAddress", *metricsAddr,
		"leaderElect", *leaderElect,
		"leaderElectionNamespace", *leaderElectNamespace,
	)

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

	// Only the provider manager serves metrics (Metrics.BindAddress); the
	// per-blueprint instance managers keep metrics disabled to avoid a port
	// collision (see the instance manager construction in startFn). Leader
	// election, when enabled, creates a Lease in the provider workspace in
	// leaderElectionNamespace — required for HA (replicaCount>1).
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Scheme:                  providerScheme,
		HealthProbeBindAddress:  *healthProbeAddr,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		LeaderElection:          *leaderElect,
		LeaderElectionID:        "krop-controller-leader",
		LeaderElectionNamespace: *leaderElectNamespace,
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

	// Resolve the host-cluster client used for host-target children. An explicit
	// --host-kubeconfig is loaded (and a load error is fatal); otherwise fall back
	// to in-cluster config. When there is no in-cluster config either (a dev run
	// outside a pod), the host target is DISABLED — hostClient stays nil so the
	// Reconciler omits the host applier/reader/GC entries and never dials a host.
	var hostClient client.Client
	var hostCfg *rest.Config
	if *hostKubeconfig != "" {
		hostCfg, err = clientcmd.BuildConfigFromFlags("", *hostKubeconfig)
		if err != nil {
			return fmt.Errorf("loading host kubeconfig %q: %w", *hostKubeconfig, err)
		}
	} else {
		hostCfg, err = rest.InClusterConfig()
		if err != nil {
			// Not in a pod and no explicit kubeconfig: host target is optional, so
			// treat this as disabled rather than fatal.
			hostCfg = nil
			entryLog.Info("host target disabled (no host kubeconfig / not in-cluster)")
		}
	}
	if hostCfg != nil {
		// Host children are applied as unstructured, so clientgoscheme (core types)
		// is sufficient for the host client — mirror the provider scheme's registration.
		hostScheme := runtime.NewScheme()
		if serr := clientgoscheme.AddToScheme(hostScheme); serr != nil {
			return fmt.Errorf("registering host scheme: %w", serr)
		}
		hostClient, err = client.New(hostCfg, client.Options{Scheme: hostScheme})
		if err != nil {
			return fmt.Errorf("host client: %w", err)
		}
		entryLog.Info("host target enabled")
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
	// A live blueprint SPEC CHANGE is handled by OnPublished: on a changed
	// specHash it Stops the running manager before Ensure restarts it, so this
	// startFn re-reads the registry and picks up the new graph (registry.Set runs
	// before the Stop+Ensure, so the restarted startFn observes the update).
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

		// Disable the instance manager's metrics server: the provider manager
		// already binds the default metrics port (:8080), and there is one
		// instance manager PER published blueprint — if they all defaulted to
		// :8080 every one after the first would fail to bind, its Start would
		// return an error the Supervisor swallows, and the instance-serving path
		// would silently die (surfaced by the full-stack deployment e2e). The
		// instance managers expose no metrics of their own, so disable it.
		imgr, err := mcmanager.New(cfg, provider, manager.Options{
			Scheme:  instanceScheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		if err != nil {
			return fmt.Errorf("setting up instance manager for %q: %w", exportName, err)
		}

		reconciler := &kropctrl.Reconciler{
			Graph:          sb.graph,
			ProviderClient: providerClient,
			HostClient:     hostClient,
			InstanceGVK:    sb.gvk,
			BlueprintName:  exportName,
			Routing:        sb.routing,
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

		// A blueprint spec change restarts this per-export manager (Supervisor
		// Stop+Ensure) with a fresh graph — re-running this builder under the SAME
		// controller name. controller-runtime keeps a PROCESS-GLOBAL registry of
		// controller names (to catch duplicate metric labels) with no deregistration
		// on manager stop, so the second build would fail with "controller with name
		// ... already exists". The old manager is fully cancelled before the new one
		// serves, so the name reuse is intentional: skip the uniqueness check (the
		// documented escape hatch for this pattern).
		skipNameValidation := true
		if err := mcbuilder.ControllerManagedBy(imgr).
			Named("krop-instance-" + exportName).
			WithOptions(mccontroller.Options{SkipNameValidation: &skipNameValidation}).
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
		OnPublished: func(exportName string, instanceGVK schema.GroupVersionKind, g *krograph.Graph, routing map[string]kropengine.Target, changed bool) {
			// Always update the served graph so a restarted startFn reads the latest.
			registry.Set(exportName, servedBlueprint{graph: g, gvk: instanceGVK, routing: routing})
			// When the compiled graph CHANGED (new blueprint or spec edit), stop the
			// running manager first so Ensure restarts it and its reconciler closure
			// re-reads the updated registry graph — otherwise a live spec edit would
			// keep serving the OLD graph forever (Ensure is a no-op while running).
			// Stop is safe even when nothing runs (first publish). Gate on changed so
			// the 5m unchanged resync does NOT tear the manager down every 5 minutes.
			if changed {
				sup.Stop(exportName)
			}
			sup.Ensure(rootCtx, exportName)
		},
		OnDeleted: func(export string) {
			if export != "" {
				sup.Stop(export)
				registry.Delete(export)
			}
		},
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&kropv1alpha1.ResourceGraphDefinition{}).
		Named("krop-registrar").
		Complete(reconcile.Func(reg.Reconcile)); err != nil {
		return fmt.Errorf("building registrar controller: %w", err)
	}

	// Orphan sweeper: reclaims provider-target children whose consumer instance
	// unbound the APIExport mid-life (so its finalizer never fired). It runs on
	// the provider-workspace manager since it needs provider-workspace access,
	// reconciling against the liveness records the instance reconciler refreshes.
	if err := mgr.Add(&kropctrl.Sweeper{
		ProviderClient: providerClient,
		HostClient:     hostClient,
		StaleAfter:     orphanStaleAfter,
		SweepInterval:  orphanSweepInterval,
	}); err != nil {
		return fmt.Errorf("adding orphan sweeper: %w", err)
	}

	entryLog.Info("starting provider manager", "workspace", workspace)
	if err := mgr.Start(rootCtx); err != nil {
		return fmt.Errorf("running provider manager: %w", err)
	}

	return nil
}

// podNamespace returns the namespace the controller pod runs in, read from the
// downward-API-injectable POD_NAMESPACE env var, falling back to "default". It
// is the default namespace for the leader-election Lease (created in the
// provider workspace), where controller-runtime needs an explicit namespace
// because the workspace-scoped client has none to infer.
func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}

	return "default"
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
	_, after, ok := strings.Cut(host, marker)
	if !ok {
		return host
	}
	name := after
	if j := strings.IndexByte(name, '/'); j >= 0 {
		name = name[:j]
	}
	if name == "" {
		return host
	}

	return name
}
