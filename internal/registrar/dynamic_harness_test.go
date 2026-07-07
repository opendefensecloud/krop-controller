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

package registrar_test

// dynamicHarness is the reusable wiring behind the dynamic-path envtest specs. It
// mirrors cmd/controller/main.go: a provider workspace authoring a blueprint, a
// Registrar that auto-publishes it, and a Supervisor that auto-starts an
// instance-serving manager per published export. The M4b capstone spec, the prune
// spec, the orphan-sweep spec, and the spec-change spec all build on it so the
// wiring lives in exactly ONE place (and is exercised — hence validated — by the
// already-passing capstone spec).

import (
	"context"
	"fmt"
	"time"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	kropkcp "go.opendefense.cloud/krop-controller/internal/kcp"
	"go.opendefense.cloud/krop-controller/internal/registrar"
	"go.opendefense.cloud/krop-controller/internal/supervisor"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// harnessOptions parameterizes the dynamic path for one blueprint.
type harnessOptions struct {
	blueprintFile string                  // the RGD fixture to author in the provider ws
	blueprintObj  string                  // the RGD metadata.name (the Reconcile request name)
	exportName    string                  // the export the Registrar is expected to publish
	instanceGVK   schema.GroupVersionKind // the generated instance kind
	instanceList  schema.GroupVersionKind // the instance List kind (for the served-type wait)
	bindingFile   string                  // an APIBinding fixture with a ${PROVIDER_PATH} placeholder
	bindingName   string                  // the APIBinding metadata.name to wait Bound
}

// dynamicHarness holds the live handles of a running dynamic path.
type dynamicHarness struct {
	opts           harnessOptions
	cli            clusterclient.ClusterClient
	providerPath   logicalcluster.Path
	providerClient client.Client
	cfg            *rest.Config
	sup            *supervisor.Supervisor
	reg            *registrar.Registrar
	registry       *graphRegistry
	rootCtx        context.Context
	cancel         context.CancelFunc

	// consumerPath/consumerWS are the FIRST bound consumer (bindConsumer appends more).
	consumerPath logicalcluster.Path
	consumerWS   *tenancyv1alpha1.Workspace
}

// startDynamicHarness stands up the whole dynamic path for one blueprint: a
// provider workspace with the blueprint + AgentRequest CRDs, a Registrar that
// direct-reconciles the blueprint once (driving the real OnPublished →
// supervisor.Ensure → startFn chain), and a first bound consumer. It mirrors the
// M4b capstone BeforeAll exactly.
func startDynamicHarness(ctx context.Context, opts harnessOptions) *dynamicHarness {
	GinkgoHelper()
	h := &dynamicHarness{opts: opts}

	var err error
	h.cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: clientgoscheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	// 1. Provider workspace + blueprint/AgentRequest CRDs (both Established before
	//    the graph build so kro can type-check cross-target CEL).
	_, h.providerPath = envtest.NewWorkspaceFixture(GinkgoT(), h.cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-provider"))
	applyFile(ctx, h.cli, h.providerPath, rgdCRDPath)
	applyFile(ctx, h.cli, h.providerPath, agentCRDPath)
	waitCRDEstablished(ctx, h.cli, h.providerPath, "resourcegraphdefinitions.krop.opendefense.cloud")
	waitCRDEstablished(ctx, h.cli, h.providerPath, "agentrequests.fulfil.krop.opendefense.cloud")

	// 2. Provider-workspace-scoped config + client + graph source.
	h.cfg = rest.CopyConfig(kcpConfig)
	h.cfg.Host += h.providerPath.RequestPath()
	h.providerClient, err = client.New(h.cfg, client.Options{Scheme: clientgoscheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	graphSource, err := kropengine.NewEndpointGraphSource(h.cfg)
	Expect(err).NotTo(HaveOccurred())

	// 3. Wire the REAL dynamic path (mirror cmd/controller/main.go).
	//nolint:gosec // h.cancel is stored on the harness and invoked by the spec's AfterAll.
	h.rootCtx, h.cancel = context.WithCancel(ctx)
	h.registry = &graphRegistry{m: map[string]servedGraph{}}
	h.sup = supervisor.New(h.startFn())

	h.reg = &registrar.Registrar{
		Client:    h.providerClient,
		Workspace: h.providerPath.String(),
		Cache:     registrar.NewGraphCache(),
		Source:    graphSource,
		OnPublished: func(exportName string, instanceGVK schema.GroupVersionKind, g *krograph.Graph, changed bool) {
			h.registry.Set(exportName, servedGraph{graph: g, gvk: instanceGVK})
			if changed {
				h.sup.Stop(exportName)
			}
			h.sup.Ensure(h.rootCtx, exportName)
		},
	}

	// 4. Author the blueprint, then drive publication via ONE direct reconcile.
	applyFile(ctx, h.cli, h.providerPath, opts.blueprintFile)
	_, err = h.reg.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: opts.blueprintObj}})
	Expect(err).NotTo(HaveOccurred())
	Expect(h.sup.Running(opts.exportName)).To(BeTrue(), "supervisor must have auto-started the instance manager for the published export")

	// 5. First bound consumer.
	h.consumerPath, h.consumerWS = h.bindConsumer(ctx)

	return h
}

// republish re-authors nothing but re-drives the Registrar for the blueprint (used
// after a live spec edit): Reconcile → OnPublished(changed) → Stop+Ensure.
func (h *dynamicHarness) republish(ctx context.Context) {
	GinkgoHelper()
	_, err := h.reg.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: h.opts.blueprintObj}})
	Expect(err).NotTo(HaveOccurred())
}

// bindConsumer creates a fresh consumer workspace bound to the published export
// (accepting the auto-derived configmaps claim) and waits until the instance kind
// is served there. Returns the consumer path and Workspace (whose Spec.Cluster is
// the logical-cluster name used to qualify provider-child names).
func (h *dynamicHarness) bindConsumer(ctx context.Context) (logicalcluster.Path, *tenancyv1alpha1.Workspace) {
	GinkgoHelper()
	consumerWS, consumerPath := envtest.NewWorkspaceFixture(GinkgoT(), h.cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-consumer"))
	applyFileSubst(ctx, h.cli, consumerPath, h.opts.bindingFile, map[string]string{"PROVIDER_PATH": h.providerPath.String()})

	envtest.Eventually(GinkgoT(), func() (bool, string) {
		binding := &apisv1alpha2.APIBinding{}
		if err := h.cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Name: h.opts.bindingName}, binding); err != nil {
			return false, err.Error()
		}
		if binding.Status.Phase != apisv1alpha2.APIBindingPhaseBound {
			return false, "binding phase " + string(binding.Status.Phase)
		}

		return true, ""
	}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer APIBinding not Bound")

	envtest.Eventually(GinkgoT(), func() (bool, string) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(h.opts.instanceList)
		if err := h.cli.Cluster(consumerPath).List(ctx, list); err != nil {
			return false, err.Error()
		}

		return true, ""
	}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance type not served in consumer workspace")

	return consumerPath, consumerWS
}

// unbindConsumer deletes the consumer's APIBinding, disengaging its logical cluster
// from the virtual workspace (the mid-life unbind that orphans provider children).
func (h *dynamicHarness) unbindConsumer(ctx context.Context, consumerPath logicalcluster.Path) {
	GinkgoHelper()
	binding := &apisv1alpha2.APIBinding{}
	binding.SetName(h.opts.bindingName)
	Expect(h.cli.Cluster(consumerPath).Delete(ctx, binding)).To(Succeed())
}

// startFn returns the supervisor StartFunc that builds and runs one instance-serving
// manager for a published export (mirrors cmd/controller/main.go's startFn).
func (h *dynamicHarness) startFn() supervisor.StartFunc {
	return func(ctx context.Context, exportName string) error {
		sg, ok := h.registry.Get(exportName)
		if !ok {
			return fmt.Errorf("no served graph recorded for export %q", exportName)
		}

		// The endpoint slice URL only populates once a binding consumes it; poll
		// until FindEndpointSlice resolves.
		var sliceName string
		if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				name, ferr := kropkcp.FindEndpointSlice(ctx, h.providerClient, exportName)
				if ferr != nil {
					//nolint:nilerr // a lookup error means the slice/URL isn't populated yet; keep polling.
					return false, nil
				}
				sliceName = name

				return true, nil
			}); err != nil {
			return fmt.Errorf("waiting for APIExportEndpointSlice for %q: %w", exportName, err)
		}

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

		provider, err := apiexport.New(h.cfg, sliceName, apiexport.Options{Scheme: instanceScheme})
		if err != nil {
			return fmt.Errorf("constructing apiexport provider for %q: %w", exportName, err)
		}
		imgr, err := mcmanager.New(h.cfg, provider, mcmanager.Options{Scheme: instanceScheme})
		if err != nil {
			return fmt.Errorf("setting up instance manager for %q: %w", exportName, err)
		}

		reconciler := &kropctrl.Reconciler{
			Graph:          sg.graph,
			ProviderClient: h.providerClient,
			InstanceGVK:    sg.gvk,
			BlueprintName:  exportName,
		}

		// Mirror cmd/controller/main.go: a spec-change restart re-runs this builder
		// under the same controller name, which controller-runtime's process-global
		// name registry rejects unless name validation is skipped (the old manager is
		// fully cancelled before the new one serves).
		skipNameValidation := true
		if err := mcbuilder.ControllerManagedBy(imgr).
			Named("krop-instance-" + exportName).
			WithOptions(mccontroller.Options{SkipNameValidation: &skipNameValidation}).
			For(newInstanceObj(sg.gvk)).
			Complete(mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
				cl, err := imgr.GetCluster(ctx, req.ClusterName)
				if err != nil {
					return ctrl.Result{}, err
				}
				res, err := reconciler.Reconcile(ctx, cl.GetClient(), string(req.ClusterName), req.NamespacedName)
				if err != nil {
					return ctrl.Result{}, err
				}
				if res.Requeue {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}

				return ctrl.Result{}, nil
			})); err != nil {
			return fmt.Errorf("building krop-instance controller for %q: %w", exportName, err)
		}

		return imgr.Start(ctx)
	}
}

// getInstanceUID reads the instance's metadata.uid from the consumer workspace.
func (h *dynamicHarness) getInstanceUID(ctx context.Context, consumerPath logicalcluster.Path, name string) string {
	GinkgoHelper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(h.opts.instanceGVK)
	Expect(h.cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, got)).To(Succeed())

	return string(got.GetUID())
}

// providerChildExists reports whether a labeled/named provider child of the given
// GVK is present in the provider workspace.
func (h *dynamicHarness) providerChildExists(ctx context.Context, gvk schema.GroupVersionKind, name string) bool {
	GinkgoHelper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := h.providerClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, u)
	if err == nil {
		return true
	}
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "unexpected error getting provider child: %v", err)

	return false
}
