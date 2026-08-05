package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/forge/test/e2e/internal/kindtest"
	"golang.org/x/crypto/ssh"
)

const fluxInstallManifest = "https://github.com/fluxcd/flux2/releases/download/v2.4.0/install.yaml"

// runToolRunnerContract owns the cross-repository real-cluster contract for
// HOR-397. Component semantics stay in control-plane unit/integration tests;
// this fresh Kind scenario proves the released boundaries compose:
//
//	Flux GitRepository artifact -> materializer -> read-only generation volume
//	-> Node runner -> chart-issued mTLS -> gateway registration -> pinned drain.
//
// Forge's DigitalOcean Flux stage separately proves forge installs and gates on
// the exact source revision/digest. This Kind contract deliberately uses the
// shared kindtest harness rather than retaining a ticket-specific shell runner
// in the control-plane repository.
func runToolRunnerContract(t *testing.T) {
	controlPlaneRoot, chartsRoot := toolRunnerDependencyRoots(t)
	controlChart := filepath.Join(chartsRoot, "charts", "control-plane")
	certificateChart := filepath.Join(chartsRoot, "charts", "cert-manager-substrate")

	const (
		controlImage = "forge-tool-contract-control-plane:dev"
		runnerImage  = "forge-tool-contract-runner:dev"
		gitImage     = "forge-tool-contract-git:dev"
		namespace    = "iterabase-system"
		release      = "tool"
	)

	v1 := writeToolRevision(t, "1.0.0", "v1")
	v2 := writeToolRevision(t, "2.0.0", "v2")
	buildToolContractImages(t, controlPlaneRoot, controlImage, runnerImage, gitImage)
	command(t, "helm", "dependency", "build", controlChart)
	command(t, "helm", "dependency", "build", certificateChart)

	cluster := kindtest.CreateCluster(t, "forge-tool-runner-contract")
	cluster.LoadImage(t, controlImage)
	cluster.LoadImage(t, runnerImage)
	cluster.LoadImage(t, gitImage)

	// Install the same Flux version Forge pins, then seed an in-cluster reviewed
	// Git repository so source-controller produces a real status artifact.
	cluster.Kubectl(t, "apply", "-f", fluxInstallManifest)
	cluster.Kubectl(t, "wait", "-n", "flux-system", "--for=condition=Available",
		"deployment/source-controller", "deployment/kustomize-controller", "--timeout=180s")
	installToolGitServer(t, cluster, gitImage, v1)
	applyManifest(t, cluster, `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: overlay
  namespace: flux-system
spec:
  interval: 2s
  url: ssh://git@tool-git.default.svc/git/repo
  ref:
    branch: master
  secretRef:
    name: overlay-git-auth
`)
	cluster.Kubectl(t, "wait", "-n", "flux-system", "--for=condition=Ready", "gitrepository/overlay", "--timeout=180s")
	artifactRevision := strings.TrimSpace(cluster.Kubectl(t, "get", "gitrepository", "overlay", "-n", "flux-system", "-o", "jsonpath={.status.artifact.revision}"))
	artifactDigest := strings.TrimSpace(cluster.Kubectl(t, "get", "gitrepository", "overlay", "-n", "flux-system", "-o", "jsonpath={.status.artifact.digest}"))
	if artifactRevision == "" || !isCanonicalSHA256Digest(artifactDigest) {
		t.Fatalf("Flux did not publish an exact revision/canonical digest: revision=%q digest=%q", artifactRevision, artifactDigest)
	}
	applyManifest(t, cluster, `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-tool-materializer
  namespace: flux-system
spec:
  podSelector:
    matchLabels:
      app: source-controller
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: iterabase-system
          podSelector:
            matchLabels:
              app.kubernetes.io/component: tool-runner
      ports:
        - protocol: TCP
          port: 9090
`)

	// Use the chart-owned certificate substrate and tool-runner deployment. The
	// control-plane chart is installed without --wait because runner readiness is
	// intentionally gated on materializing and registering the first generation.
	cluster.HelmInstall(t, "tool-cert-manager", certificateChart, "", namespace, certificateChart, map[string]string{
		"cert-manager.prometheus.servicemonitor.enabled": "false",
	})
	cluster.HelmUpgrade(t, release, controlChart, "", namespace, controlChart, map[string]string{
		"postgresql.enabled":                   "true",
		"image.repository":                     "forge-tool-contract-control-plane",
		"image.tag":                            "dev",
		"image.pullPolicy":                     "Never",
		"gateway.enabled":                      "true",
		"gateway.tls.clusterResourceNamespace": namespace,
		"artifact.enabled":                     "false",
		"toolRunner.enabled":                   "true",
		"toolRunner.image.repository":          "forge-tool-contract-runner",
		"toolRunner.image.tag":                 "dev",
		"toolRunner.image.pullPolicy":          "Never",
		"toolRunner.allowedToolNamespaces":     "{platform}",
	})
	cluster.Kubectl(t, "wait", "-n", namespace, "--for=condition=Ready", "certificate/tool-tool-runner", "--timeout=180s")
	for _, workload := range []string{"deployment/tool-control-plane-api", "deployment/tool-control-plane-gateway"} {
		cluster.Kubectl(t, "rollout", "status", "-n", namespace, workload, "--timeout=300s")
	}
	if out, err := kubectlOutput(cluster, "rollout", "status", "-n", namespace, "deployment/tool-tool-runner", "--timeout=300s"); err != nil {
		pods, _ := kubectlOutput(cluster, "get", "pods", "-n", namespace, "-o", "wide")
		describe, _ := kubectlOutput(cluster, "describe", "pods", "-n", namespace, "-l", "app.kubernetes.io/component=tool-runner")
		logs, _ := kubectlOutput(cluster, "logs", "-n", namespace, "deployment/tool-tool-runner", "--all-containers", "--tail=200")
		network, _ := kubectlOutput(cluster, "get", "namespace", namespace, "-o", "yaml")
		fluxNetwork, _ := kubectlOutput(cluster, "get", "networkpolicy", "-n", "flux-system", "-o", "yaml")
		source, _ := kubectlOutput(cluster, "get", "pod,service,endpoints", "-n", "flux-system", "-l", "app.kubernetes.io/component=source-controller", "-o", "wide", "--show-labels")
		t.Fatalf("tool runner did not become ready: %v\n%s\n--- pods ---\n%s\n--- describe ---\n%s\n--- logs ---\n%s\n--- namespace ---\n%s\n--- Flux policies ---\n%s\n--- source endpoint ---\n%s", err, out, pods, describe, logs, network, fluxNetwork, source)
	}

	postgresPod := cluster.FirstPodName(t, namespace, "app.kubernetes.io/name=postgresql")
	password := decodeSecret(t, cluster.Kubectl(t, "get", "secret", "tool-postgresql", "-n", namespace, "-o", "jsonpath={.data.password}"))
	waitRegistration(t, cluster, namespace, postgresPod, password,
		fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.echo' AND tool_digest='%s' AND active AND accepting_new", v1.digest), "1")
	v1Attempt := createPinnedToolAttempt(t, cluster, namespace, postgresPod, password, v1.digest)

	// Publish a second exact revision while a real unfinished work attempt pins
	// v1. The runner must keep v1 loaded for that attempt while removing it from
	// new-attempt eligibility and registering v2 as the current generation.
	gitPod := cluster.FirstPodName(t, "default", "app=tool-git")
	cluster.Kubectl(t, "cp", "-n", "default", v2.bundlePath, gitPod+":/git/repo/tools/product/echo/index.mjs")
	cluster.Kubectl(t, "cp", "-n", "default", v2.manifestPath, gitPod+":/git/repo/tools/product/echo/manifest.json")
	if out, err := cluster.Exec(t, "default", gitPod, "git", "git config --global --add safe.directory /git/repo; cd /git/repo; git add .; git commit -m v2; chown -R git:git /git/repo"); err != nil {
		t.Fatalf("publish v2: %v\n%s", err, out)
	}
	cluster.Kubectl(t, "annotate", "gitrepository/overlay", "-n", "flux-system",
		fmt.Sprintf("reconcile.fluxcd.io/requestedAt=%d", time.Now().UnixNano()), "--overwrite")
	waitRegistration(t, cluster, namespace, postgresPod, password,
		fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.echo' AND tool_digest='%s' AND active AND accepting_new", v2.digest), "1")
	waitRegistration(t, cluster, namespace, postgresPod, password,
		fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.echo' AND tool_digest='%s' AND active AND NOT accepting_new", v1.digest), "1")
	waitRegistration(t, cluster, namespace, postgresPod, password,
		fmt.Sprintf("SELECT tool_version_digest FROM toolgateway.attempt_tool_pins WHERE attempt_id='%s' AND tool_name='platform.echo'", v1Attempt), v1.digest)
	_ = createPinnedToolAttempt(t, cluster, namespace, postgresPod, password, v2.digest)

	completeToolAttempt(t, cluster, namespace, postgresPod, password, v1Attempt)
	waitRegistration(t, cluster, namespace, postgresPod, password,
		fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='platform.echo' AND tool_digest='%s' AND active", v1.digest), "0")

	t.Logf("exact Flux artifact %s (%s) materialized; v1 stayed routable while pinned, v2 accepted new attempts, and v1 retired after its attempt completed", artifactRevision, artifactDigest)
}

type toolRevision struct {
	root, bundlePath, manifestPath, digest string
}

func writeToolRevision(t *testing.T, version, result string) toolRevision {
	t.Helper()
	root := t.TempDir()
	toolDir := filepath.Join(root, "tools", "product", "echo")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("mkdir tool fixture: %v", err)
	}
	bundle := []byte(fmt.Sprintf(`export const identity={name:"platform.echo",version:%q};
export async function invoke(_context,args){return {result:{generation:%q,args}};}
`, version, result))
	projection := map[string]any{
		"apiVersion": "iterabase.io/tool/v1", "name": "platform.echo", "version": version,
		"description": "Cluster contract echo", "bundle": "index.mjs",
		"inputSchema": map[string]any{"type": "object"}, "effectClass": "read_only", "timeoutMs": 5000,
	}
	canonical, err := json.Marshal(projection)
	if err != nil {
		t.Fatalf("marshal manifest projection: %v", err)
	}
	h := sha256.New()
	_, _ = h.Write(canonical)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(bundle)
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	manifest := make(map[string]any, len(projection)+1)
	for key, value := range projection {
		manifest[key] = value
	}
	manifest["digest"] = digest
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	bundlePath := filepath.Join(toolDir, "index.mjs")
	manifestPath := filepath.Join(toolDir, "manifest.json")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	if err := os.WriteFile(manifestPath, append(manifestJSON, '\n'), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return toolRevision{root: root, bundlePath: bundlePath, manifestPath: manifestPath, digest: digest}
}

func toolRunnerDependencyRoots(t *testing.T) (string, string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	forgeRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	workspace := filepath.Dir(forgeRoot)
	controlPlane := os.Getenv("ITERABASE_CONTROL_PLANE_ROOT")
	if controlPlane == "" {
		controlPlane = filepath.Join(workspace, "control-plane")
	}
	charts := os.Getenv("ITERABASE_CHARTS_ROOT")
	if charts == "" {
		charts = filepath.Join(workspace, "iterabase-charts")
	}
	for label, path := range map[string]string{"control-plane": controlPlane, "iterabase-charts": charts} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s checkout %q unavailable (set the corresponding ITERABASE_*_ROOT): %v", label, path, err)
		}
	}
	return controlPlane, charts
}

func buildToolContractImages(t *testing.T, controlPlaneRoot, controlImage, runnerImage, gitImage string) {
	t.Helper()
	command(t, "docker", "build", "-t", controlImage, controlPlaneRoot)
	command(t, "docker", "build", "-t", runnerImage, "-f", filepath.Join(controlPlaneRoot, "tool-runner", "Dockerfile"), filepath.Join(controlPlaneRoot, "tool-runner"))
	contextDir := t.TempDir()
	mustWriteFile(t, filepath.Join(contextDir, "Dockerfile"), "FROM alpine:3.20\nRUN apk add --no-cache git openssh\n")
	command(t, "docker", "build", "-t", gitImage, contextDir)
}

func installToolGitServer(t *testing.T, cluster *kindtest.Cluster, gitImage string, revision toolRevision) {
	t.Helper()
	clientPublic, clientPrivate := sshKeyPair(t)
	hostPublic, hostPrivate := sshKeyPair(t)
	keys := t.TempDir()
	clientPrivatePath := filepath.Join(keys, "identity")
	knownHostsPath := filepath.Join(keys, "known_hosts")
	hostPrivatePath := filepath.Join(keys, "ssh_host_ed25519_key")
	clientPublicPath := filepath.Join(keys, "authorized_keys")
	if err := os.WriteFile(clientPrivatePath, clientPrivate, 0o600); err != nil {
		t.Fatalf("write Flux identity: %v", err)
	}
	if err := os.WriteFile(knownHostsPath, []byte("tool-git.default.svc "+strings.TrimSpace(string(hostPublic))+"\n"), 0o644); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	if err := os.WriteFile(hostPrivatePath, hostPrivate, 0o600); err != nil {
		t.Fatalf("write git host key: %v", err)
	}
	if err := os.WriteFile(clientPublicPath, clientPublic, 0o644); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}

	cluster.Kubectl(t, "create", "configmap", "tool-repo-seed",
		"--from-file=index.mjs="+revision.bundlePath, "--from-file=manifest.json="+revision.manifestPath)
	cluster.Kubectl(t, "create", "configmap", "tool-git-authorized", "--from-file=authorized_keys="+clientPublicPath)
	cluster.Kubectl(t, "create", "secret", "generic", "tool-git-host", "--from-file=ssh_host_ed25519_key="+hostPrivatePath)
	cluster.Kubectl(t, "create", "secret", "generic", "overlay-git-auth", "-n", "flux-system",
		"--from-file=identity="+clientPrivatePath, "--from-file=known_hosts="+knownHostsPath)
	applyManifest(t, cluster, fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: tool-git
spec:
  replicas: 1
  selector:
    matchLabels:
      app: tool-git
  template:
    metadata:
      labels:
        app: tool-git
    spec:
      containers:
        - name: git
          image: %s
          imagePullPolicy: Never
          command: ["/bin/sh", "-ceu"]
          args:
            - |
              mkdir -p /git/repo/tools/product/echo
              cp /seed/* /git/repo/tools/product/echo/
              cd /git/repo
              git init -b master
              git config user.name test
              git config user.email test@example.com
              git add . && git commit -m v1
              adduser -D -h /home/git -s /usr/bin/git-shell git
              passwd -d git
              chown -R git:git /git/repo
              mkdir -p /run/sshd
              exec /usr/sbin/sshd -D -e -o HostKey=/host/ssh_host_ed25519_key -o AuthorizedKeysFile=/auth/authorized_keys -o PasswordAuthentication=no -o PermitRootLogin=no -o StrictModes=no -o AllowUsers=git
          ports:
            - containerPort: 22
          volumeMounts:
            - name: seed
              mountPath: /seed
              readOnly: true
            - name: repo
              mountPath: /git
            - name: host
              mountPath: /host
              readOnly: true
            - name: auth
              mountPath: /auth
              readOnly: true
      volumes:
        - name: seed
          configMap:
            name: tool-repo-seed
        - name: repo
          emptyDir: {}
        - name: host
          secret:
            secretName: tool-git-host
            defaultMode: 256
        - name: auth
          configMap:
            name: tool-git-authorized
            defaultMode: 292
---
apiVersion: v1
kind: Service
metadata:
  name: tool-git
spec:
  selector:
    app: tool-git
  ports:
    - port: 22
      targetPort: 22
`, gitImage))
	cluster.Kubectl(t, "rollout", "status", "deployment/tool-git", "--timeout=120s")
}

func sshKeyPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate SSH key: %v", err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("marshal SSH public key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatalf("marshal SSH private key: %v", err)
	}
	return ssh.MarshalAuthorizedKey(sshPublic), pem.EncodeToMemory(block)
}

func applyManifest(t *testing.T, cluster *kindtest.Cluster, manifest string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	mustWriteFile(t, path, manifest)
	cluster.Kubectl(t, "apply", "-f", path)
}

func createPinnedToolAttempt(t *testing.T, cluster *kindtest.Cluster, namespace, postgresPod, password, expectedDigest string) string {
	t.Helper()
	key := fmt.Sprintf("tool-contract-%d", time.Now().UnixNano())
	query := fmt.Sprintf(`
WITH identity_row AS (
  INSERT INTO identity.identities (key,kind,source,display_name)
  VALUES ('%[1]s','workflow','local','Tool contract') RETURNING id
), definition_row AS (
  INSERT INTO workflow.definitions (key,version,digest,spec_json,scope_identity_id,source_type,pool_key)
  SELECT '%[1]s','1','sha256:test','{}',id,'operator_artifact','tool-contract' FROM identity_row
  RETURNING id,scope_identity_id
), run_row AS (
  INSERT INTO runtime.workflow_runs (kind,definition_key,scope_identity_id,session_id,session_dir,state,started_at)
  SELECT 'workflow','%[1]s:1',scope_identity_id,'%[1]s','/sessions/%[1]s','running',now() FROM definition_row
  RETURNING id,scope_identity_id
), work_item_row AS (
  INSERT INTO work.work_items (workflow_key,scope_identity_id,title,start_identity_id,start_idempotency_key,start_payload_hash)
  SELECT '%[1]s',scope_identity_id,'Tool contract',scope_identity_id,'%[1]s','sha256:test' FROM run_row
  RETURNING id
), attempt_row AS (
  INSERT INTO work.attempts (id,work_item_id,number,definition_id,definition_key,definition_version,definition_digest,graph_snapshot)
  SELECT r.id,w.id,1,d.id,'%[1]s:1','1','sha256:test','{}'
  FROM run_row r CROSS JOIN work_item_row w CROSS JOIN definition_row d
  RETURNING id,work_item_id
), linked_item AS (
  UPDATE work.work_items w SET current_attempt_id=a.id FROM attempt_row a WHERE w.id=a.work_item_id
), pin_row AS (
  INSERT INTO toolgateway.attempt_tool_pins (attempt_id,tool_name,tool_version_digest)
  SELECT a.id::text,'platform.echo',v.digest
  FROM attempt_row a
  CROSS JOIN LATERAL (
    SELECT digest FROM toolgateway.available_tool_versions
    WHERE name='platform.echo' ORDER BY created_at DESC,id DESC LIMIT 1
  ) v
  RETURNING tool_version_digest
)
SELECT a.id::text || '|' || p.tool_version_digest FROM attempt_row a CROSS JOIN pin_row p`, key)
	out, err := postgresQuery(t, cluster, namespace, postgresPod, password, query)
	if err != nil {
		t.Fatalf("create pinned tool attempt: %v\n%s", err, out)
	}
	attemptID, digest, ok := strings.Cut(strings.TrimSpace(out), "|")
	if !ok || attemptID == "" || digest != expectedDigest {
		t.Fatalf("new attempt did not pin expected tool version %q: %q", expectedDigest, out)
	}
	return attemptID
}

func completeToolAttempt(t *testing.T, cluster *kindtest.Cluster, namespace, postgresPod, password, attemptID string) {
	t.Helper()
	query := fmt.Sprintf(`
WITH finished_attempt AS (
  UPDATE work.attempts SET finished_at=now()
  WHERE id='%[1]s'::uuid AND finished_at IS NULL RETURNING id
), finished_run AS (
  UPDATE runtime.workflow_runs r SET state='succeeded',finished_at=now(),updated_at=now()
  FROM finished_attempt a WHERE r.id=a.id RETURNING r.id
)
SELECT count(*) FROM finished_run`, attemptID)
	out, err := postgresQuery(t, cluster, namespace, postgresPod, password, query)
	if err != nil || strings.TrimSpace(out) != "1" {
		t.Fatalf("complete pinned tool attempt %s: %v (output %q)", attemptID, err, out)
	}
}

func waitRegistration(t *testing.T, cluster *kindtest.Cluster, namespace, postgresPod, password, query, expected string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		out, err := postgresQuery(t, cluster, namespace, postgresPod, password, query)
		last = strings.TrimSpace(out)
		if err == nil && last == expected {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("registration query did not reach %q (last %q): %s", expected, last, query)
}

func postgresQuery(t *testing.T, cluster *kindtest.Cluster, namespace, postgresPod, password, query string) (string, error) {
	t.Helper()
	return cluster.Exec(t, namespace, postgresPod, "", fmt.Sprintf("env PGPASSWORD=%s psql -U controlplane -d controlplane -Atqc %s", shellQuote(password), shellQuote(query)))
}

func decodeSecret(t *testing.T, value string) string {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return string(decoded)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func kubectlOutput(cluster *kindtest.Cluster, args ...string) (string, error) {
	full := append([]string{"--kubeconfig", cluster.Kubeconfig}, args...)
	cmd := exec.Command("kubectl", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func command(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
