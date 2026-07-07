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

// Package e2e is the full-stack deployment-path e2e for krop-controller: it runs
// the controller AS A DEPLOYED POD in a kind cluster, against real kcp (provided
// by kcp-operator), deployed via krop's own Helm chart, and validates the
// least-privilege permission model end to end.
//
// The harness is single-shard: RootShard + FrontProxy + one etcd, all workspaces
// on root. It is adapted from opendefensecloud/dependency-controller's proven
// kind+kcp SynchronizedBeforeSuite, stripped of the webhook binary, the
// system:admin per-shard RBAC bootstrap, the secondary shard, and multi-shard
// placement — krop has ONE controller binary and authorizes exclusively via a
// mounted, workspace-scoped kcp kubeconfig.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tool paths resolved from env vars with PATH fallback.
var (
	kindBin    string
	kubectlBin string
	helmBin    string
	dockerBin  string
)

const (
	kindClusterName = "krop-e2e"
	kcpNamespace    = "kcp-system"
	kropNamespace   = "krop-system"
	certManagerVer  = "v1.17.2"
	imageName       = "krop-controller:e2e"
	imageRepo       = "krop-controller"
	imageTag        = "e2e"
	helmTimeout     = "300s"

	// serviceAccountName is the controller pod's ServiceAccount. saIdentity is the
	// kcp identity string that ServiceAccount authenticates as through the mounted
	// kubeconfig; the kcp-native RBAC fixtures bind their ClusterRoles to it.
	serviceAccountName = "krop-controller"
	saIdentity         = "system:serviceaccount:" + kropNamespace + ":" + serviceAccountName

	// NodePort for the front-proxy service exposed via kind (host access).
	frontProxyNodePort = "31443"

	// exportName is the APIExport the deployed controller auto-publishes from the
	// KubernetesCluster blueprint.
	exportName = "kubernetesclusters.krop.opendefense.cloud"
)

// Workspace names under root (single-shard: everything on root).
const (
	wsProvider    = "krop-provider"
	wsConsumer    = "krop-consumer"
	wsConsumerNeg = "krop-consumer-neg"
)

var (
	rootDir     string
	fixturesDir string
	tmpDir      string

	// Host kubeconfig for kcp via front-proxy NodePort (admin, system:kcp:admin).
	kcpHostKubeconfig string

	// Workspace-scoped controller kubeconfig for the deployed pod (front-proxy
	// in-cluster URL + /clusters/root:krop-provider).
	controllerKubeconfigPath string

	// In-cluster front-proxy base URL (derived from the kcp-operator kubeconfig).
	inClusterFPURL string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "krop-controller E2E Suite")
}

func lookupTool(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	p, err := exec.LookPath(fallback)
	if err != nil {
		return fallback // let it fail later with a clear error
	}

	return p
}

func init() {
	kindBin = lookupTool("KIND", "kind")
	kubectlBin = lookupTool("KUBECTL", "kubectl")
	helmBin = lookupTool("HELM", "helm")
	dockerBin = lookupTool("DOCKER", "docker")
}

// run executes a command and returns combined output. Fails the test on non-zero exit.
func run(name string, args ...string) string {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		Fail(fmt.Sprintf("command failed: %s %s\n%s\n%v", name, strings.Join(args, " "), buf.String(), err))
	}

	return buf.String()
}

// runNoFail executes a command and returns output + error without failing.
func runNoFail(name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	return buf.String(), err
}

// kindctl runs kubectl against the kind (hosting) cluster.
func kindctl(args ...string) string {
	GinkgoHelper()
	return run(kubectlBin, append([]string{"--context", "kind-" + kindClusterName}, args...)...)
}

// kindctlNoFail runs kubectl against the kind cluster without failing.
func kindctlNoFail(args ...string) (string, error) {
	return runNoFail(kubectlBin, append([]string{"--context", "kind-" + kindClusterName}, args...)...)
}

// kcpServer returns the front-proxy NodePort URL for a workspace under root.
// The empty string or "root" targets the root workspace itself.
func kcpServer(ws string) string {
	if ws == "" || ws == "root" {
		return fmt.Sprintf("https://localhost:%s/clusters/root", frontProxyNodePort)
	}

	return fmt.Sprintf("https://localhost:%s/clusters/root:%s", frontProxyNodePort, ws)
}

// kcpctl runs kubectl against kcp at a given workspace (relative to root).
func kcpctl(ws string, args ...string) {
	GinkgoHelper()
	run(kubectlBin, append([]string{
		"--kubeconfig", kcpHostKubeconfig,
		"--server", kcpServer(ws),
	}, args...)...)
}

// kcpctlNoFail runs kubectl against kcp without failing.
func kcpctlNoFail(ws string, args ...string) (string, error) {
	return runNoFail(kubectlBin, append([]string{
		"--kubeconfig", kcpHostKubeconfig,
		"--server", kcpServer(ws),
	}, args...)...)
}

// applyFixtureToWS applies a YAML fixture to a kcp workspace with placeholder
// substitution. Retries on transient kcp authorization errors that surface while
// APIExports / permissionClaims propagate.
func applyFixtureToWS(ws, file string, substitutions map[string]string) {
	GinkgoHelper()
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())

	content := string(raw)
	for k, v := range substitutions {
		content = strings.ReplaceAll(content, "${"+k+"}", v)
	}

	waitFor(2*time.Minute, fmt.Sprintf("apply %s to %s", file, ws), func() error {
		cmd := exec.CommandContext(context.Background(), kubectlBin,
			"--kubeconfig", kcpHostKubeconfig,
			"--server", kcpServer(ws),
			"apply", "-f", "-",
		)
		cmd.Stdin = strings.NewReader(content)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, buf.String())
		}

		return nil
	})
}

// waitFor retries a check function until it succeeds or the timeout is reached.
func waitFor(timeout time.Duration, desc string, check func() error) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		if err := check(); err == nil {
			return
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("timed out waiting for: %s (last error: %v)", desc, lastErr))
		case <-ticker.C:
		}
	}
}

// kindctlSecret extracts the kubeconfig from a secret in the kcp-system namespace.
func kindctlSecret(name string) string {
	GinkgoHelper()
	return kindctl("-n", kcpNamespace, "get", "secret", name, "-o", "jsonpath={.data.kubeconfig}")
}

var _ = SynchronizedBeforeSuite(func() {
	var err error
	rootDir, err = filepath.Abs("../..")
	Expect(err).NotTo(HaveOccurred())
	fixturesDir = filepath.Join(rootDir, "test", "fixtures")

	tmpDir, err = os.MkdirTemp("", "krop-e2e-*")
	Expect(err).NotTo(HaveOccurred())
	kcpHostKubeconfig = filepath.Join(tmpDir, "kcp-host.kubeconfig")

	By("creating kind cluster")
	createKindCluster()

	By("installing cert-manager")
	installCertManager()

	By("deploying kcp via kcp-operator")
	deployKCPOperator()

	By("deploying etcd")
	deployEtcd()

	By("creating kcp RootShard + FrontProxy")
	createKCPResources()

	By("generating admin kubeconfig")
	buildAdminKubeconfig()

	By("building the controller kubeconfig")
	buildControllerKubeconfig()

	By("building and loading the controller image")
	buildAndLoadImage()

	By("setting up kcp workspaces, CRDs, RBAC, and the blueprint")
	setupWorkspaces()

	By("deploying the krop-controller Helm chart")
	deployChart()
}, func() {})

var _ = SynchronizedAfterSuite(func() {}, func() {
	if os.Getenv("E2E_SKIP_CLEANUP") != "" {
		return
	}
	out, err := runNoFail(kindBin, "delete", "cluster", "--name", kindClusterName)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "kind delete: %s %v\n", out, err)
	}
	if tmpDir != "" {
		_ = os.RemoveAll(tmpDir)
	}
})

func createKindCluster() {
	// Reuse if it already exists.
	out, _ := runNoFail(kindBin, "get", "clusters")
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == kindClusterName {
			run(kindBin, "export", "kubeconfig", "--name", kindClusterName)
			return
		}
	}

	run(kindBin, "create", "cluster",
		"--name", kindClusterName,
		"--config", filepath.Join(fixturesDir, "kind-config.yaml"),
		"--wait", "60s",
	)
}

func installCertManager() {
	kindctl("apply", "-f",
		fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", certManagerVer))

	waitFor(3*time.Minute, "cert-manager webhook available", func() error {
		_, err := kindctlNoFail("-n", "cert-manager", "wait", "deployment", "cert-manager-webhook",
			"--for=condition=Available", "--timeout=1s")

		return err
	})

	waitFor(time.Minute, "self-signed ClusterIssuer created", func() error {
		_, err := kindctlNoFail("apply", "-f", filepath.Join(fixturesDir, "cert-manager-selfsigned-issuer.yaml"))
		return err
	})
}

func deployKCPOperator() {
	_, _ = runNoFail(helmBin, "repo", "add", "kcp", "https://kcp-dev.github.io/helm-charts")
	run(helmBin, "repo", "update", "kcp")

	run(helmBin, "upgrade", "--install", "kcp-operator", "kcp/kcp-operator",
		"--namespace", kcpNamespace,
		"--create-namespace",
		"--wait", "--timeout", helmTimeout,
	)
}

func deployEtcd() {
	applyEtcd("etcd-root")
	waitFor(3*time.Minute, "etcd-root ready", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "wait", "statefulset", "etcd-root",
			"--for=jsonpath={.status.readyReplicas}=1", "--timeout=1s")

		return err
	})
}

// applyEtcd creates a minimal single-node etcd instance in the kcp namespace.
func applyEtcd(name string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: etcd
    app.kubernetes.io/instance: %[1]s
  ports:
    - name: client
      port: 2379
      targetPort: client
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s-headless
  namespace: %[2]s
  annotations:
    service.alpha.kubernetes.io/tolerate-unready-endpoints: "true"
spec:
  type: ClusterIP
  clusterIP: None
  publishNotReadyAddresses: true
  selector:
    app.kubernetes.io/name: etcd
    app.kubernetes.io/instance: %[1]s
  ports:
    - name: client
      port: 2379
      targetPort: client
    - name: peer
      port: 2380
      targetPort: peer
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: etcd
      app.kubernetes.io/instance: %[1]s
  serviceName: %[1]s-headless
  template:
    metadata:
      labels:
        app.kubernetes.io/name: etcd
        app.kubernetes.io/instance: %[1]s
    spec:
      automountServiceAccountToken: false
      containers:
        - name: etcd
          image: quay.io/coreos/etcd:v3.5.21
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/etcd"]
          args:
            - --name=$(HOSTNAME)
            - --data-dir=/data
            - --listen-peer-urls=http://0.0.0.0:2380
            - --listen-client-urls=http://0.0.0.0:2379
            - --advertise-client-urls=http://$(HOSTNAME).%[1]s-headless.%[2]s.svc.cluster.local:2379
            - --initial-cluster-state=new
            - --initial-cluster-token=$(HOSTNAME)
            - --initial-cluster=$(HOSTNAME)=http://$(HOSTNAME).%[1]s-headless.%[2]s.svc.cluster.local:2380
            - --initial-advertise-peer-urls=http://$(HOSTNAME).%[1]s-headless.%[2]s.svc.cluster.local:2380
            - --listen-metrics-urls=http://0.0.0.0:8080
          env:
            - name: HOSTNAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
          ports:
            - name: client
              containerPort: 2379
            - name: peer
              containerPort: 2380
            - name: metrics
              containerPort: 8080
          livenessProbe:
            httpGet:
              path: /livez
              port: metrics
            initialDelaySeconds: 15
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: metrics
            initialDelaySeconds: 10
            periodSeconds: 5
            failureThreshold: 30
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              memory: 256Mi
          volumeMounts:
            - name: data
              mountPath: /data
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 1Gi
`, name, kcpNamespace)

	applyToKind(manifest)
}

func createKCPResources() {
	fpHostname := fmt.Sprintf("kcp-front-proxy.%s.svc.cluster.local", kcpNamespace)

	// cert-manager Issuer for kcp-operator PKI.
	applyToKind(fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned
  namespace: %s
spec:
  selfSigned: {}`, kcpNamespace))

	// RootShard. localhost/127.0.0.1 SANs on the server cert allow direct access;
	// hostAliases map the front-proxy hostname to the fixed front-proxy ClusterIP so
	// the shard's advertised virtual-workspace URL resolves in-cluster.
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: RootShard
metadata:
  name: root
  namespace: %[1]s
spec:
  external:
    hostname: %[2]s
    port: 6443
  certificates:
    issuerRef:
      group: cert-manager.io
      kind: Issuer
      name: selfsigned
  certificateTemplates:
    server:
      spec:
        dnsNames:
          - localhost
        ipAddresses:
          - "127.0.0.1"
  cache:
    embedded:
      enabled: true
  etcd:
    endpoints:
      - http://etcd-root.%[1]s.svc.cluster.local:2379
  auth:
    serviceAccount:
      enabled: true
  deploymentTemplate:
    spec:
      template:
        spec:
          hostAliases:
            - ip: "10.96.200.200"
              hostnames:
                - "%[2]s"`, kcpNamespace, fpHostname))

	// FrontProxy with a fixed ClusterIP + NodePort for host access.
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: FrontProxy
metadata:
  name: kcp
  namespace: %[1]s
spec:
  rootShard:
    ref:
      name: root
  auth:
    serviceAccount:
      enabled: true
  serviceTemplate:
    spec:
      type: NodePort
      clusterIP: "10.96.200.200"
  certificateTemplates:
    server:
      spec:
        dnsNames:
          - localhost
          - "%[2]s"
        ipAddresses:
          - "127.0.0.1"`, kcpNamespace, fpHostname))

	waitFor(4*time.Minute, "root shard running", func() error {
		out, err := kindctlNoFail("-n", kcpNamespace, "get", "rootshard", "root",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("root shard phase: %s", out)
		}

		return nil
	})

	waitFor(3*time.Minute, "front-proxy running", func() error {
		out, err := kindctlNoFail("-n", kcpNamespace, "get", "frontproxy", "kcp",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("front-proxy phase: %s", out)
		}

		return nil
	})

	// Pin the front-proxy Service to a fixed NodePort.
	kindctl("-n", kcpNamespace, "patch", "service", "kcp-front-proxy", "--type=json",
		fmt.Sprintf(`-p=[{"op":"replace","path":"/spec/ports/0/nodePort","value":%s}]`, frontProxyNodePort))
}

func buildAdminKubeconfig() {
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: e2e-admin
  namespace: %s
spec:
  username: kcp-admin
  groups:
    - "system:kcp:admin"
  validity: 8766h
  secretRef:
    name: e2e-admin-kubeconfig
  target:
    frontProxyRef:
      name: kcp`, kcpNamespace))

	waitFor(2*time.Minute, "admin kubeconfig secret created", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", "e2e-admin-kubeconfig",
			"-o", "jsonpath={.data.kubeconfig}")

		return err
	})

	kcRaw := kindctlSecret("e2e-admin-kubeconfig")
	kcBytes, err := decodeBase64(kcRaw)
	Expect(err).NotTo(HaveOccurred())

	adminServerURL := extractServerFromKubeconfig(kcBytes)
	rewritten := strings.ReplaceAll(string(kcBytes),
		adminServerURL,
		fmt.Sprintf("https://localhost:%s", frontProxyNodePort))

	Expect(os.WriteFile(kcpHostKubeconfig, []byte(rewritten), 0o600)).To(Succeed())

	waitFor(time.Minute, "kcp API reachable via front-proxy", func() error {
		_, err := runNoFail(kubectlBin, "--kubeconfig", kcpHostKubeconfig,
			"--server", kcpServer("root"),
			"get", "--raw", "/readyz")

		return err
	})
}

// buildControllerKubeconfig creates a Kubeconfig CR whose identity is EXACTLY the
// deployed pod's ServiceAccount, targeting the ROOT SHARD (so the client cert is
// signed by root-client-ca — trusted by both the front-proxy and the shard's
// virtual workspaces, which the multicluster provider connects to directly). The
// server URL is then rewritten to the in-cluster front-proxy + the provider
// workspace path.
func buildControllerKubeconfig() {
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: e2e-controller
  namespace: %[1]s
spec:
  username: %[2]q
  groups:
    - "system:authenticated"
    - "system:serviceaccounts"
    - "system:serviceaccounts:%[3]s"
  validity: 8766h
  secretRef:
    name: e2e-controller-kubeconfig
  target:
    rootShardRef:
      name: root`, kcpNamespace, saIdentity, kropNamespace))

	waitFor(2*time.Minute, "controller kubeconfig secret created", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", "e2e-controller-kubeconfig",
			"-o", "jsonpath={.data.kubeconfig}")

		return err
	})

	kcRaw := kindctlSecret("e2e-controller-kubeconfig")
	kcBytes, err := decodeBase64(kcRaw)
	Expect(err).NotTo(HaveOccurred())
	shardURL := extractServerFromKubeconfig(kcBytes)

	parsed, err := url.Parse(shardURL)
	Expect(err).NotTo(HaveOccurred())
	fpPort := parsed.Port()
	if fpPort == "" {
		fpPort = "6443"
	}
	fpHostname := fmt.Sprintf("kcp-front-proxy.%s.svc.cluster.local", kcpNamespace)
	inClusterFPURL = "https://" + net.JoinHostPort(fpHostname, fpPort)
	providerURL := inClusterFPURL + "/clusters/root:" + wsProvider

	// kcp-operator emits a "base" context (bare shard/front-proxy URL) and a
	// "default" context (URL + /clusters/root). Rewriting the base URL to include
	// the workspace path corrupts "default" with a doubled /clusters/ segment, so
	// switch current-context to "base".
	rewritten := strings.ReplaceAll(string(kcBytes), shardURL, providerURL)
	rewritten = strings.ReplaceAll(rewritten, "current-context: default", "current-context: base")

	controllerKubeconfigPath = filepath.Join(tmpDir, "kcp-controller.kubeconfig")
	Expect(os.WriteFile(controllerKubeconfigPath, []byte(rewritten), 0o600)).To(Succeed())
}

// decodeBase64 decodes a base64-encoded string.
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

// extractServerFromKubeconfig extracts the server URL from a kubeconfig YAML.
func extractServerFromKubeconfig(kubeconfig []byte) string {
	re := regexp.MustCompile(`server:\s*(https?://\S+)`)
	m := re.FindSubmatch(kubeconfig)
	if len(m) < 2 {
		Fail("could not extract server URL from kubeconfig")
	}

	return string(m[1])
}

func buildAndLoadImage() {
	run(dockerBin, "build", "-t", imageName, rootDir)
	run(kindBin, "load", "docker-image", imageName, "--name", kindClusterName)
}

func setupWorkspaces() {
	// Create the provider + consumer workspaces under root.
	for _, ws := range []string{wsProvider, wsConsumer, wsConsumerNeg} {
		createWorkspace(ws)
	}
	for _, ws := range []string{wsProvider, wsConsumer, wsConsumerNeg} {
		waitFor(2*time.Minute, fmt.Sprintf("workspace %s ready", ws), func() error {
			out, err := kcpctlNoFail("root", "get", "workspace", ws, "-o", "jsonpath={.status.phase}")
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) != "Ready" {
				return fmt.Errorf("workspace %s phase: %s", ws, out)
			}

			return nil
		})
	}

	// Provider workspace: install the blueprint CRD + the AgentRequest CRD, wait
	// both Established (the AgentRequest CRD must be served before the controller
	// builds the graph so kro can type-check ${agentRequest.status.token}).
	kcpctl(wsProvider, "apply", "-f",
		filepath.Join(rootDir, "config/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml"))
	kcpctl(wsProvider, "apply", "-f",
		filepath.Join(fixturesDir, "crd-agentrequests.fulfil.krop.opendefense.cloud.yaml"))
	for _, crd := range []string{
		"resourcegraphdefinitions.krop.opendefense.cloud",
		"agentrequests.fulfil.krop.opendefense.cloud",
	} {
		waitFor(2*time.Minute, fmt.Sprintf("CRD %s established", crd), func() error {
			out, err := kcpctlNoFail(wsProvider, "get", "crd", crd,
				"-o", `jsonpath={.status.conditions[?(@.type=="Established")].status}`)
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) != "True" {
				return fmt.Errorf("CRD %s Established=%s", crd, out)
			}

			return nil
		})
	}

	// Least-privilege RBAC: root-rbac into root, provider-rbac into the provider
	// workspace. These bind the controller's SA identity to the ClusterRoles it
	// authorizes against. Deliberately NO consumer-workspace RBAC.
	subs := map[string]string{
		"SA_IDENTITY":        saIdentity,
		"PROVIDER_WORKSPACE": "root:" + wsProvider,
	}
	applyFixtureToWS("root", filepath.Join(rootDir, "config/kcp/rbac/root-rbac.yaml"), subs)
	applyFixtureToWS(wsProvider, filepath.Join(rootDir, "config/kcp/rbac/provider-rbac.yaml"), subs)

	// Ensure the default namespace exists in the provider workspace (provider-target
	// children land there).
	ensureDefaultNamespace(wsProvider)

	// Author the blueprint in the provider workspace. The deployed controller
	// auto-publishes it once its pod is Ready.
	applyFixtureToWS(wsProvider, filepath.Join(fixturesDir, "blueprint-kubernetescluster-rgd.yaml"), nil)
}

// createWorkspace creates a kcp workspace under root (idempotent).
func createWorkspace(name string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: tenancy.kcp.io/v1alpha1
kind: Workspace
metadata:
  name: %s`, name)

	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--kubeconfig", kcpHostKubeconfig,
		"--server", kcpServer("root"),
		"apply", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(manifest)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	_ = cmd.Run() // ignore AlreadyExists
}

// ensureDefaultNamespace makes sure the default namespace exists in a workspace.
func ensureDefaultNamespace(ws string) {
	_, _ = kcpctlNoFail(ws, "create", "namespace", "default")
}

func deployChart() {
	_, _ = kindctlNoFail("create", "namespace", kropNamespace)

	// Mount the workspace-scoped controller kubeconfig as a Secret.
	_, _ = kindctlNoFail("-n", kropNamespace, "delete", "secret", "krop-kubeconfig", "--ignore-not-found")
	kindctl("-n", kropNamespace, "create", "secret", "generic", "krop-kubeconfig",
		"--from-file=kubeconfig="+controllerKubeconfigPath)

	run(helmBin, "upgrade", "--install", "krop",
		filepath.Join(rootDir, "charts/krop-controller"),
		"--namespace", kropNamespace,
		"--set", "image.repository="+imageRepo,
		"--set", "image.tag="+imageTag,
		"--set", "image.pullPolicy=Never",
		"--set", "kcp.kubeconfigSecret.name=krop-kubeconfig",
		"--set", "serviceAccount.name="+serviceAccountName,
		"--wait", "--timeout", "180s",
	)

	// Confirm the pod becomes Ready (helm --wait already blocks on the Deployment,
	// but assert it explicitly for a clear failure signal).
	waitFor(2*time.Minute, "krop-controller pod ready", func() error {
		_, err := kindctlNoFail("-n", kropNamespace, "wait", "pod",
			"-l", "app.kubernetes.io/name=krop-controller",
			"--for=condition=Ready", "--timeout=5s")

		return err
	})
}

// applyToKind applies a YAML manifest to the kind cluster.
func applyToKind(manifest string) {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--context", "kind-"+kindClusterName, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	Expect(cmd.Run()).To(Succeed(), "applying manifest: %s", buf.String())
}
