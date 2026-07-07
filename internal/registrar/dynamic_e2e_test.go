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

// M4b full dynamic auto-publication e2e — the M4 capstone.
//
// This proves the ENTIRE dynamic path with NO hand-written APIExport/ARS/claim
// fixtures: a provider authors a blueprint → the Registrar auto-publishes an
// APIExport (+ ARS + auto-derived permissionClaims) → the Supervisor auto-starts
// an instance-serving manager → a consumer binds the auto-published export
// (accepting the auto-derived configmaps claim) → creating an instance
// materializes the dual-target + cross-target children.
//
// The wiring MIRRORS cmd/controller/main.go: a thread-safe registry the
// Registrar's OnPublished populates, a supervisor.New(startFn) whose startFn
// discovers the endpoint slice, builds an apiexport provider + manager, and
// registers the shared controller.Reconciler with the cached graph. The only
// difference from production is that the Registrar is direct-reconciled once
// (instead of run under a full controller-runtime manager) — but that direct
// call MUST drive the real OnPublished → supervisor.Ensure → startFn chain, so
// the instance manager is auto-started by the Supervisor, not hand-wired.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	kropkcp "go.opendefense.cloud/krop-controller/internal/kcp"
	"go.opendefense.cloud/krop-controller/internal/registrar"
	"go.opendefense.cloud/krop-controller/internal/supervisor"
)

const bindingPath = "../../test/fixtures/apibinding-kubernetescluster.yaml"

// servedGraph is what the Registrar records on publish and the per-export manager
// needs to serve: the compiled graph and the generated instance kind. It mirrors
// cmd/controller/main.go's servedBlueprint.
type servedGraph struct {
	graph *krograph.Graph
	gvk   schema.GroupVersionKind
}

// graphRegistry is a thread-safe registry keyed by export name (mirrors main.go's
// published): OnPublished writes it, the supervisor's startFn reads it.
type graphRegistry struct {
	mu sync.Mutex
	m  map[string]servedGraph
}

func (g *graphRegistry) Set(export string, sg servedGraph) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.m[export] = sg
}

func (g *graphRegistry) Get(export string) (servedGraph, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	sg, ok := g.m[export]
	return sg, ok
}

// applyFileSubst reads a YAML file, substitutes ${KEY} placeholders from vars, and
// creates the resulting object in the given workspace (the APIBinding needs the
// provider path substituted, which the plain applyFile helper does not do).
func applyFileSubst(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, file string, vars map[string]string) {
	GinkgoHelper()
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())
	text := string(raw)
	for k, v := range vars {
		text = strings.ReplaceAll(text, "${"+k+"}", v)
	}
	u := &unstructured.Unstructured{}
	Expect(yaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096).Decode(u)).To(Succeed())
	Expect(cli.Cluster(wsPath).Create(ctx, u)).To(Succeed())
}

var _ = Describe("M4b full dynamic auto-publication", Ordered, func() {
	var (
		ctx          = context.Background()
		cli          clusterclient.ClusterClient
		providerPath logicalcluster.Path
		consumerPath logicalcluster.Path
		consumerWS   *tenancyv1alpha1.Workspace
		rootCtx      context.Context
		cancel       context.CancelFunc
		sup          *supervisor.Supervisor
	)

	BeforeAll(func() {
		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: clientgoscheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// 1. Provider workspace + blueprint/AgentRequest CRDs (both Established
		//    before the graph build so kro can type-check ${agentRequest.status.token}).
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-provider"))
		applyFile(ctx, cli, providerPath, rgdCRDPath)
		applyFile(ctx, cli, providerPath, agentCRDPath)
		waitCRDEstablished(ctx, cli, providerPath, "resourcegraphdefinitions.krop.opendefense.cloud")
		waitCRDEstablished(ctx, cli, providerPath, "agentrequests.fulfil.krop.opendefense.cloud")

		// 2. Provider-workspace-scoped config + client + graph source.
		cfg := rest.CopyConfig(kcpConfig)
		cfg.Host += providerPath.RequestPath()
		providerClient, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		graphSource, err := kropengine.NewEndpointGraphSource(cfg)
		Expect(err).NotTo(HaveOccurred())

		// 3. Wire the REAL dynamic path (mirror cmd/controller/main.go).
		rootCtx, cancel = context.WithCancel(ctx)
		registry := &graphRegistry{m: map[string]servedGraph{}}

		startFn := func(ctx context.Context, exportName string) error {
			sg, ok := registry.Get(exportName)
			if !ok {
				return fmt.Errorf("no served graph recorded for export %q", exportName)
			}

			// The endpoint slice is auto-created shortly after the APIExport is
			// published (and its URL only populates once a binding consumes it);
			// poll until FindEndpointSlice resolves.
			var sliceName string
			if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, true,
				func(ctx context.Context) (bool, error) {
					name, ferr := kropkcp.FindEndpointSlice(ctx, providerClient, exportName)
					if ferr != nil {
						return false, nil
					}
					sliceName = name
					return true, nil
				}); err != nil {
				return fmt.Errorf("waiting for APIExportEndpointSlice for %q: %w", exportName, err)
			}

			// Instance-serving scheme (mirror main.go): the instance kind is
			// unstructured, but the apiexport provider reads APIBinding /
			// LogicalCluster / Workspace off the virtual workspace.
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
			imgr, err := mcmanager.New(cfg, provider, mcmanager.Options{Scheme: instanceScheme})
			if err != nil {
				return fmt.Errorf("setting up instance manager for %q: %w", exportName, err)
			}

			reconciler := &kropctrl.Reconciler{
				Graph:          sg.graph,
				ProviderClient: providerClient,
				InstanceGVK:    sg.gvk,
				BlueprintName:  exportName,
			}

			if err := mcbuilder.ControllerManagedBy(imgr).
				Named("krop-instance-"+exportName).
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

		sup = supervisor.New(startFn)

		reg := &registrar.Registrar{
			Client:    providerClient,
			Workspace: providerPath.String(),
			Cache:     registrar.NewGraphCache(),
			Source:    graphSource,
			OnPublished: func(exportName string, instanceGVK schema.GroupVersionKind, g *krograph.Graph) {
				registry.Set(exportName, servedGraph{graph: g, gvk: instanceGVK})
				sup.Ensure(rootCtx, exportName)
			},
		}

		// 4. Author the blueprint in the provider workspace (cluster-scoped).
		applyFile(ctx, cli, providerPath, blueprintPath)

		// 5. Drive publication via a single DIRECT reconcile — this MUST trigger the
		//    real OnPublished → supervisor.Ensure → startFn chain, so the instance
		//    manager is auto-started by the Supervisor.
		_, err = reg.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "kubernetescluster"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(sup.Running(expectedExportName)).To(BeTrue(), "supervisor must have auto-started the instance manager for the published export")

		// 6. Consumer workspace + APIBinding to the AUTO-PUBLISHED export, accepting
		//    the auto-derived configmaps permissionClaim. Created BEFORE the endpoint
		//    slice URL populates (bind-first rule); the startFn's FindEndpointSlice
		//    retry engages the consumer cluster once the binding populates the slice.
		consumerWS, consumerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-consumer"))
		applyFileSubst(ctx, cli, consumerPath, bindingPath, map[string]string{"PROVIDER_PATH": providerPath.String()})

		// Wait for the binding to bind and the instance kind to be List-able in the
		// consumer workspace (this also refreshes the client's RESTMapper).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			binding := &apisv1alpha2.APIBinding{}
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Name: "kubernetesclusters"}, binding); err != nil {
				return false, err.Error()
			}
			if binding.Status.Phase != apisv1alpha2.APIBindingPhaseBound {
				return false, "binding phase " + string(binding.Status.Phase)
			}
			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer APIBinding not Bound")

		envtest.Eventually(GinkgoT(), func() (bool, string) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesClusterList"})
			if err := cli.Cluster(consumerPath).List(ctx, list); err != nil {
				return false, err.Error()
			}
			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "KubernetesCluster type not served in consumer workspace")
	})

	AfterAll(func() {
		if cancel != nil {
			cancel()
		}
	})

	It("materializes the cross-target children through the auto-published export", func() {
		// Create the instance in the consumer workspace.
		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
			"metadata": map[string]interface{}{"name": "demo", "namespace": "default"},
			"spec":     map[string]interface{}{"region": "eu"},
		}}
		Expect(cli.Cluster(consumerPath).Create(ctx, instance)).To(Succeed())

		// The provider-target AgentRequest is created in the provider ws (collision-named).
		agentName := kropengine.ProviderChildName(consumerWS.Spec.Cluster, "demo", "eu-agent")
		agentKey := client.ObjectKey{Namespace: "default", Name: agentName}
		agentGVK := schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"}
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(agentGVK)
			err := cli.Cluster(providerPath).Get(ctx, agentKey, ar)
			return err == nil, "agentrequest not created yet"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider AgentRequest not created (instance manager not serving the consumer cluster?)")

		// The consumer ConfigMap must PEND until the AgentRequest status.token is set.
		Consistently(func() bool {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)
			return err != nil
		}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(), "consumer child must pend until the provider status is set")

		// Simulate the downstream fulfilment controller: patch the AgentRequest status.
		ar := &unstructured.Unstructured{}
		ar.SetGroupVersionKind(agentGVK)
		Expect(cli.Cluster(providerPath).Get(ctx, agentKey, ar)).To(Succeed())
		Expect(unstructured.SetNestedField(ar.Object, "tok-xyz789", "status", "token")).To(Succeed())
		Expect(cli.Cluster(providerPath).Status().Update(ctx, ar)).To(Succeed())

		// The consumer ConfigMap now appears with the propagated token — written
		// THROUGH the auto-published vw, authorized by the auto-derived claim.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(cm.Object, "data", "token")
			return tok == "tok-xyz789", "consumer cm data.token=" + tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "cross-target token did not propagate to the consumer child")

		// The instance status maps agentToken from the provider child status.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(got.Object, "status", "agentToken")
			return tok == "tok-xyz789", "status.agentToken=" + tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status.agentToken not mapped")
	})
})

// newInstanceObj returns a fresh unstructured object typed to the instance GVK, so
// the multicluster builder derives the watched type from the embedded GVK.
func newInstanceObj(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}
