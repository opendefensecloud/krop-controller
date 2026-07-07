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

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// applyManifestToWS applies an inline YAML manifest to a kcp workspace.
func applyManifestToWS(ws, manifest string) {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--kubeconfig", kcpHostKubeconfig,
		"--server", kcpServer(ws),
		"apply", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(manifest)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	Expect(cmd.Run()).To(Succeed(), "applying manifest to %s: %s", ws, buf.String())
}

// jsonpath fetches a jsonpath value from a resource in a workspace, trimmed. An
// empty name lists the kind (use an {.items[...]} expr); a non-empty name gets
// that object.
func jsonpath(ws, kind, name, ns, expr string) (string, error) {
	args := []string{"get", kind}
	if name != "" {
		args = append(args, name)
	}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	args = append(args, "-o", "jsonpath="+expr)
	out, err := kcpctlNoFail(ws, args...)

	return strings.TrimSpace(out), err
}

// instanceManifest renders a KubernetesCluster instance for a region.
func instanceManifest(name, region string) string {
	return fmt.Sprintf(`apiVersion: krop.opendefense.cloud/v1alpha1
kind: KubernetesCluster
metadata:
  name: %s
  namespace: default
spec:
  region: %s`, name, region)
}

var _ = Describe("krop-controller full-stack e2e (deployed pod)", Ordered, func() {
	const (
		token    = "tok-e2e-eu-42"
		negToken = "tok-e2e-us-99"
	)

	It("publishes the blueprint's APIExport from the deployed controller", func() {
		// The deployed pod watches krop-provider and publishes the compiled
		// APIExport — this proves its provider-workspace RBAC works end to end.
		waitFor(4*time.Minute, "APIExport published in provider by deployed controller", func() error {
			_, err := kcpctlNoFail(wsProvider, "get", "apiexport", exportName)
			return err
		})
	})

	It("binds the export in the consumer accepting the configmaps claim", func() {
		ensureDefaultNamespace(wsConsumer)
		applyFixtureToWS(wsConsumer, filepath.Join(fixturesDir, "apibinding-kubernetescluster.yaml"),
			map[string]string{"PROVIDER_PATH": "root:" + wsProvider})

		waitFor(3*time.Minute, "consumer APIBinding Bound", func() error {
			phase, err := jsonpath(wsConsumer, "apibinding", "kubernetesclusters", "", "{.status.phase}")
			if err != nil {
				return err
			}
			if phase != "Bound" {
				return fmt.Errorf("binding phase: %s", phase)
			}

			return nil
		})

		// The served instance kind must be List-able before we create one.
		waitFor(2*time.Minute, "KubernetesCluster kind served in consumer", func() error {
			_, err := kcpctlNoFail(wsConsumer, "get", "kubernetesclusters", "-A")
			return err
		})
	})

	It("materializes cross-workspace children through the least-privilege claim", func() {
		By("creating a KubernetesCluster{region: eu} in the consumer")
		applyManifestToWS(wsConsumer, instanceManifest("demo", "eu"))

		By("asserting the provider AgentRequest appears (provider-side, controller identity)")
		var agentName string
		waitFor(3*time.Minute, "provider AgentRequest created", func() error {
			name, err := jsonpath(wsProvider, "agentrequests", "", "default", "{.items[0].metadata.name}")
			if err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("no AgentRequest yet")
			}
			agentName = name

			return nil
		})

		By("asserting the consumer ConfigMap pends until the provider status is set")
		Consistently(func() error {
			_, err := kcpctlNoFail(wsConsumer, "get", "configmap", "eu-cluster-config", "-n", "default")
			return err
		}, 3*time.Second, 500*time.Millisecond).Should(HaveOccurred(),
			"consumer ConfigMap must pend until the AgentRequest token is set")

		By("patching the AgentRequest status.token (simulating downstream fulfilment)")
		kcpctl(wsProvider, "patch", "agentrequest", agentName, "-n", "default",
			"--subresource=status", "--type=merge",
			"-p", fmt.Sprintf(`{"status":{"token":%q}}`, token))

		By("asserting the consumer ConfigMap appears with the propagated token (written through the vw, authorized by the accepted claim)")
		waitFor(3*time.Minute, "consumer ConfigMap has propagated token", func() error {
			tok, err := jsonpath(wsConsumer, "configmap", "eu-cluster-config", "default", "{.data.token}")
			if err != nil {
				return err
			}
			if tok != token {
				return fmt.Errorf("configmap data.token=%q", tok)
			}

			return nil
		})

		By("asserting the instance status.agentToken maps the provider child status")
		waitFor(2*time.Minute, "instance status.agentToken mapped", func() error {
			tok, err := jsonpath(wsConsumer, "kubernetescluster", "demo", "default", "{.status.agentToken}")
			if err != nil {
				return err
			}
			if tok != token {
				return fmt.Errorf("status.agentToken=%q", tok)
			}

			return nil
		})
	})

	It("garbage-collects cross-workspace children on instance delete", func() {
		By("deleting the instance in the consumer")
		kcpctl(wsConsumer, "delete", "kubernetescluster", "demo", "-n", "default", "--wait=false")

		By("asserting the provider AgentRequest is GC'd")
		waitFor(2*time.Minute, "provider AgentRequest gone", func() error {
			out, err := kcpctlNoFail(wsProvider, "get", "agentrequests", "-n", "default",
				"-o", "jsonpath={.items[*].metadata.name}")
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) != "" {
				return fmt.Errorf("AgentRequests still present: %s", out)
			}

			return nil
		})

		By("asserting the consumer ConfigMap is GC'd")
		waitFor(2*time.Minute, "consumer ConfigMap gone", func() error {
			out, err := kcpctlNoFail(wsConsumer, "get", "configmap", "eu-cluster-config", "-n", "default")
			if err != nil && strings.Contains(out, "NotFound") {
				return nil
			}
			if err != nil {
				return nil // treat any get error (incl. NotFound) as gone
			}

			return fmt.Errorf("consumer ConfigMap still present")
		})

		By("asserting the instance itself is gone (finalizer removed)")
		waitFor(2*time.Minute, "instance gone", func() error {
			out, err := kcpctlNoFail(wsConsumer, "get", "kubernetescluster", "demo", "-n", "default")
			if err != nil {
				return nil // NotFound
			}

			return fmt.Errorf("instance still present: %s", out)
		})
	})

	It("NEGATIVE: never writes the consumer ConfigMap without the accepted claim", func() {
		// Second consumer binds the SAME export but REJECTS the configmaps claim.
		// The AgentRequest (provider-side) is still created and we set its token,
		// so the ONLY thing preventing the consumer ConfigMap is the missing claim
		// — proving cross-workspace writes require the accepted permissionClaim.
		By("binding the export in the second consumer with the configmaps claim REJECTED")
		ensureDefaultNamespace(wsConsumerNeg)
		applyFixtureToWS(wsConsumerNeg, filepath.Join(fixturesDir, "apibinding-kubernetescluster-noclaim.yaml"),
			map[string]string{"PROVIDER_PATH": "root:" + wsProvider})

		waitFor(3*time.Minute, "neg consumer APIBinding Bound", func() error {
			phase, err := jsonpath(wsConsumerNeg, "apibinding", "kubernetesclusters", "", "{.status.phase}")
			if err != nil {
				return err
			}
			if phase != "Bound" {
				return fmt.Errorf("binding phase: %s", phase)
			}

			return nil
		})

		waitFor(2*time.Minute, "KubernetesCluster kind served in neg consumer", func() error {
			_, err := kcpctlNoFail(wsConsumerNeg, "get", "kubernetesclusters", "-A")
			return err
		})

		By("creating a KubernetesCluster{region: us} in the second consumer")
		applyManifestToWS(wsConsumerNeg, instanceManifest("demo-neg", "us"))

		By("waiting for the provider AgentRequest and setting its token")
		var negAgent string
		waitFor(3*time.Minute, "neg provider AgentRequest created", func() error {
			name, err := jsonpath(wsProvider, "agentrequests", "", "default", "{.items[0].metadata.name}")
			if err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("no AgentRequest yet")
			}
			negAgent = name

			return nil
		})
		kcpctl(wsProvider, "patch", "agentrequest", negAgent, "-n", "default",
			"--subresource=status", "--type=merge",
			"-p", fmt.Sprintf(`{"status":{"token":%q}}`, negToken))

		By("asserting the consumer ConfigMap is NEVER created (claim-gated cross-workspace write is denied)")
		Consistently(func() error {
			_, err := kcpctlNoFail(wsConsumerNeg, "get", "configmap", "us-cluster-config", "-n", "default")
			return err
		}, 6*time.Second, 500*time.Millisecond).Should(HaveOccurred(),
			"consumer ConfigMap must NEVER appear without the accepted configmaps claim")
	})
})
