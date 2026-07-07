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

package controller_test

import (
	"os"
	"testing"

	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The in-process envtest suite boots a real kcp from the binary that
// `make test` downloads (TEST_KCP_ASSETS points at bin/), then drives the M2
// dual-target reconcile end-to-end through the APIExport virtual workspace
// (see dualtarget_test.go). It skips cleanly when TEST_KCP_ASSETS is unset so
// plain `go test ./...` stays hermetic.
var (
	env       *envtest.Environment
	kcpConfig *rest.Config
)

func init() {
	// The multicluster-provider default ObjectToWatch is apis/v1alpha1 APIBinding
	// and the provider lists apis/v1alpha1 APIExportEndpointSlices, so both apis
	// versions plus core (LogicalCluster) and tenancy (Workspace) are required.
	// apiextensions lets the workspace-scoped client install the instance CRD.
	runtime.Must(clientgoscheme.AddToScheme(clientgoscheme.Scheme))
	runtime.Must(apisv1alpha1.AddToScheme(clientgoscheme.Scheme))
	runtime.Must(apisv1alpha2.AddToScheme(clientgoscheme.Scheme))
	runtime.Must(corev1alpha1.AddToScheme(clientgoscheme.Scheme))
	runtime.Must(tenancyv1alpha1.AddToScheme(clientgoscheme.Scheme))
	runtime.Must(apiextensionsv1.AddToScheme(clientgoscheme.Scheme))
}

func TestControllerIntegration(t *testing.T) {
	if os.Getenv("TEST_KCP_ASSETS") == "" {
		t.Skip("set TEST_KCP_ASSETS (make test downloads the kcp binary) to run the controller envtest e2e")
	}
	RegisterFailHandler(Fail)

	var err error
	env = &envtest.Environment{}
	kcpConfig, err = env.Start()
	if err != nil {
		t.Fatalf("starting kcp envtest environment: %v", err)
	}
	defer func() { _ = env.Stop() }()

	RunSpecs(t, "Controller Integration Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	metricsserver.DefaultBindAddress = "0"
})

var _ = AfterSuite(func() {
	metricsserver.DefaultBindAddress = ":8080"
})
