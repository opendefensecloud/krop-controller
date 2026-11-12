// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

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
		// conditionMessage is the free-text message of the AgentRequest's Ready
		// condition; krop logs only the condition's reason code (ADR 0013).
		conditionMessage = "e2e-condition-message-must-not-be-logged"
	)
	var (
		// The demo instance's UID and its provider AgentRequest, for the log checks.
		instanceUID string
		agentName   string
	)

	It("publishes the blueprint's APIExport from the deployed controller", func() {
		// The deployed pod watches krop-provider and publishes the compiled
		// APIExport — this proves its provider-workspace RBAC works end to end.
		waitFor(4*time.Minute, "APIExport published in provider by deployed controller", func() error {
			_, err := kcpctlNoFail(wsProvider, "get", "apiexport", exportName)
			return err
		})

		By("asserting the publish and the instance manager start are logged at the default level")
		waitForLog(time.Minute, levelInfo, "published APIExport",
			map[string]string{"blueprint": "kubernetescluster", "export": exportName})
		waitForLog(time.Minute, levelInfo, "starting instance manager", map[string]string{"export": exportName})
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

		uid, err := jsonpath(wsConsumer, "kubernetescluster", "demo", "default", "{.metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		instanceUID = uid

		By("asserting the applied AgentRequest is logged at V(2)")
		applied := waitForLog(2*time.Minute, levelV2, "node applied",
			map[string]string{"name": "demo", "node": "agentRequest", "child": agentName})
		Expect(applied.Fields).To(HaveKeyWithValue("target", "provider"))
		Expect(applied.Fields).To(HaveKeyWithValue("gvk", "fulfil.krop.opendefense.cloud/v1alpha1/AgentRequest"))
		Expect(applied.Fields).To(HaveKeyWithValue("instanceUID", instanceUID))

		By("reporting the AgentRequest not Ready, as a cloud controller would (#72)")
		kcpctl(wsProvider, "patch", "agentrequest", agentName, "-n", "default",
			"--subresource=status", "--type=merge",
			"-p", fmt.Sprintf(`{"status":{"conditions":[{"type":"Ready","status":"False","reason":"Provisioning","message":%q}]}}`,
				conditionMessage))

		By("asserting the pass ends Pending on the not-ready AgentRequest with its Ready reason at V(1)")
		pending := waitForLog(2*time.Minute, levelV1, "pass ended", map[string]string{
			"name": "demo", "outcome": "Pending", "node": "config", "dependency": "agentRequest", "readyReason": "Provisioning",
		})
		Expect(pending.str("reason")).To(ContainSubstring("agentRequest"))

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

	It("stays quiet at the default level while the instance is steady", func() {
		// The instance is Complete; its 30 s heartbeat passes must show at V(1) and
		// write nothing at the default level (ADR 0013).
		// kubectl logs --since-time has whole-second precision: start the window at
		// the next full second and wait for it, so it holds no earlier line.
		since := time.Now().Truncate(time.Second).Add(time.Second)
		time.Sleep(time.Until(since))
		waitFor(2*time.Minute, "a Complete heartbeat pass of the steady instance", func() error {
			lines, _ := controllerLog(since)
			if len(findLog(lines, "pass ended", map[string]string{"name": "demo", "outcome": "Complete"})) == 0 {
				return fmt.Errorf("no Complete pass since %s", since.Format(time.RFC3339))
			}

			return nil
		})
		lines, _ := controllerLog(since)
		var loud []string
		for _, l := range lines {
			if l.defaultLevel() {
				loud = append(loud, l.Raw)
			}
		}
		Expect(loud).To(BeEmpty(), "a steady instance must not log at the default level")
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

		By("asserting each target's deleted children are logged as one default-level line")
		provider := waitForLog(time.Minute, levelInfo, "deleted children",
			map[string]string{"instanceUID": instanceUID, "target": "provider"})
		Expect(provider.strs("children")).To(ConsistOf("fulfil.krop.opendefense.cloud/v1alpha1/AgentRequest default/" + agentName))
		consumer := waitForLog(time.Minute, levelInfo, "deleted children",
			map[string]string{"instanceUID": instanceUID, "target": "consumer"})
		Expect(consumer.strs("children")).To(ConsistOf("/v1/ConfigMap default/eu-cluster-config"))
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

	It("writes well-formed log lines throughout", func() {
		lines, other := controllerLog(time.Time{})
		Expect(lines).NotTo(BeEmpty())
		for _, o := range other {
			_, _ = fmt.Fprintf(GinkgoWriter, "non-JSON log line: %s\n", o)
		}
		for _, l := range lines {
			Expect([]string{levelInfo, levelError, levelV1, levelV2}).To(ContainElement(l.Level), "unknown level: %s", l.Raw)
			Expect(duplicateKey(l.Raw)).To(BeEmpty(), "duplicate key: %s", l.Raw)
			// Error lines are single lines: no stack trace by default.
			Expect(l.Fields).NotTo(HaveKey("stacktrace"), "stack trace: %s", l.Raw)
			Expect(l.Raw).NotTo(ContainSubstring(conditionMessage), "condition message logged: %s", l.Raw)
		}
	})
})
