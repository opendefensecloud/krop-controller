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

// M4a publication e2e — the Registrar auto-produces a bindable APIExport.
//
// Against real kcp (no supervisor / virtual workspace): install the blueprint CRD
// + the AgentRequest CRD in a provider workspace, build the compiled graph via the
// live EndpointGraphSource, run the Registrar's Reconcile ONCE (direct-reconcile
// style), and assert the published objects — the APIExport, its referenced ARS
// (with BOTH auto-generated status fields, proving the M3 pruning drift is fixed),
// the auto-derived configmaps permissionClaim, and the blueprint status.

import (
	"context"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kcp-dev/logicalcluster/v3"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	"go.opendefense.cloud/krop-controller/internal/registrar"
)

const (
	rgdCRDPath    = "../../config/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml"
	agentCRDPath  = "../../test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml"
	blueprintPath = "../../test/fixtures/blueprint-kubernetescluster-rgd.yaml"

	expectedExportName = "kubernetesclusters.krop.opendefense.cloud"
)

// applyFile reads a YAML file and creates the resulting object in the given
// workspace via the cluster client.
func applyFile(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, file string) {
	GinkgoHelper()
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())
	u := &unstructured.Unstructured{}
	Expect(yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096).Decode(u)).To(Succeed())
	Expect(cli.Cluster(wsPath).Create(ctx, u)).To(Succeed())
}

// waitCRDEstablished blocks until the named CRD reports Established=True in ws.
func waitCRDEstablished(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, name string) {
	GinkgoHelper()
	envtest.Eventually(GinkgoT(), func() (bool, string) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := cli.Cluster(wsPath).Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
			return false, err.Error()
		}
		for _, c := range crd.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return true, ""
			}
		}
		return false, name + " not Established"
	}, wait.ForeverTestTimeout, 200*time.Millisecond, name+" CRD not established")
}

var _ = Describe("M4a Registrar publication", Ordered, func() {
	var (
		ctx          = context.Background()
		cli          clusterclient.ClusterClient
		providerPath logicalcluster.Path
		wsClient     client.Client
	)

	BeforeAll(func() {
		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// Provider workspace.
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-provider"))

		// Install the blueprint CRD and the AgentRequest CRD; wait both Established.
		// The AgentRequest CRD must be served before the graph build so kro can
		// type-check ${agentRequest.status.token} against its OpenAPI schema.
		applyFile(ctx, cli, providerPath, rgdCRDPath)
		applyFile(ctx, cli, providerPath, agentCRDPath)
		waitCRDEstablished(ctx, cli, providerPath, "resourcegraphdefinitions.krop.opendefense.cloud")
		waitCRDEstablished(ctx, cli, providerPath, "agentrequests.fulfil.krop.opendefense.cloud")

		// Provider-workspace-scoped client + graph source (live discovery/OpenAPI).
		cfg := rest.CopyConfig(kcpConfig)
		cfg.Host += providerPath.RequestPath()
		wsClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		graphSource, err := kropengine.NewEndpointGraphSource(cfg)
		Expect(err).NotTo(HaveOccurred())

		// Apply the blueprint into the provider workspace.
		applyFile(ctx, cli, providerPath, blueprintPath)

		// Run the Registrar once (direct-reconcile).
		reg := &registrar.Registrar{
			Client:    wsClient,
			Workspace: providerPath.String(),
			Cache:     registrar.NewGraphCache(),
			Source:    graphSource,
		}
		// The blueprint CRD is cluster-scoped (workspace-level); no namespace.
		_, err = reg.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "kubernetescluster"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("publishes a correct APIExport + ARS + claims and writes blueprint status", func() {
		// 1. The APIExport exists in the provider workspace.
		export := &apisv1alpha2.APIExport{}
		Expect(wsClient.Get(ctx, client.ObjectKey{Name: expectedExportName}, export)).To(Succeed())

		// 2. It references an ARS named v<hash>.kubernetesclusters.krop.opendefense.cloud.
		Expect(export.Spec.Resources).To(HaveLen(1))
		arsName := export.Spec.Resources[0].Schema
		Expect(arsName).To(MatchRegexp(`^v[0-9a-f]+\.kubernetesclusters\.krop\.opendefense\.cloud$`))

		ars := &apisv1alpha1.APIResourceSchema{}
		Expect(wsClient.Get(ctx, client.ObjectKey{Name: arsName}, ars)).To(Succeed())
		Expect(ars.Spec.Group).To(Equal("krop.opendefense.cloud"))
		Expect(ars.Spec.Names.Kind).To(Equal("KubernetesCluster"))

		// 3. The instance ARS schema declares BOTH auto-generated status fields.
		Expect(ars.Spec.Versions).NotTo(BeEmpty())
		props, err := ars.Spec.Versions[0].GetSchema()
		Expect(err).NotTo(HaveOccurred())
		Expect(props).NotTo(BeNil())
		status, ok := props.Properties["status"]
		Expect(ok).To(BeTrue(), "instance schema must declare a status object")
		Expect(status.Properties).To(HaveKey("agentToken"))
		Expect(status.Properties).To(HaveKey("configMapName"))

		// 4. The auto-derived core claim {group: "", resource: configmaps} is present.
		var claimResources []string
		haveConfigmaps := false
		for _, pc := range export.Spec.PermissionClaims {
			claimResources = append(claimResources, pc.Group+"/"+pc.Resource)
			if pc.Group == "" && pc.Resource == "configmaps" {
				haveConfigmaps = true
			}
		}
		Expect(haveConfigmaps).To(BeTrue(), "expected a {group:\"\", resource:configmaps} claim, got %v", claimResources)

		// 5. The blueprint status reflects the publication.
		bp := &kropv1alpha1.ResourceGraphDefinition{}
		Expect(wsClient.Get(ctx, client.ObjectKey{Name: "kubernetescluster"}, bp)).To(Succeed())
		Expect(bp.Status.ExportedAPI).To(Equal(expectedExportName))
		Expect(bp.Status.ObservedSpecHash).NotTo(BeEmpty())

		var ready *metav1.Condition
		for i := range bp.Status.Conditions {
			if bp.Status.Conditions[i].Type == "Ready" {
				ready = &bp.Status.Conditions[i]
			}
		}
		Expect(ready).NotTo(BeNil(), "blueprint must have a Ready condition")
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	})
})
