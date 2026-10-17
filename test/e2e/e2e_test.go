//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	cluster       = "hatchet-controller-e2e"
	serviceUserID = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
)

var (
	root       = repoRoot()
	kubeconfig string
)

// manifestImages returns the images referenced by testdata/hatchet.yaml.
func manifestImages(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "test", "e2e", "testdata", "hatchet.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var images []string
	for _, line := range strings.Split(string(data), "\n") {
		if image, ok := strings.CutPrefix(strings.TrimSpace(line), "image:"); ok {
			images = append(images, strings.TrimSpace(image))
		}
	}
	return images
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// run executes a command in the repository root and fails the test on error.
func run(t *testing.T, stdin string, name string, args ...string) string {
	t.Helper()
	out, err := tryRun(stdin, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func tryRun(stdin string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tool(t *testing.T, args ...string) string {
	t.Helper()
	return run(t, "", "go", append([]string{"tool", "-modfile=tools/go.mod"}, args...)...)
}

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	return run(t, "", "kubectl", args...)
}

// eventually retries fn until it succeeds or the timeout expires.
func eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %v", timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func goVersion(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(root, ".tool-versions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "golang "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("golang missing from .tool-versions")
	return ""
}

// TestSmoke deploys the controller with kustomize into kind, wires hatchet-lite to the
// instance's keyset via secretKeyRef, waits for a tenant to become Ready and checks that
// owned Secrets are garbage collected.
func TestSmoke(t *testing.T) {
	img := os.Getenv("IMG")
	if img == "" {
		img = "hatchet-controller:e2e"
	}
	kubeconfig = filepath.Join(t.TempDir(), "kubeconfig")

	run(t, "", "docker", "build", "--build-arg", "golang_version="+goVersion(t), "-t", img, ".")

	// A cluster left behind by an interrupted run would make create fail.
	tool(t, "kind", "delete", "cluster", "--name", cluster)
	t.Cleanup(func() {
		if out, err := tryRun("", "go", "tool", "-modfile=tools/go.mod", "kind", "delete", "cluster", "--name", cluster); err != nil {
			t.Errorf("deleting kind cluster: %v\n%s", err, out)
		}
	})
	tool(t, "kind", "create", "cluster", "--name", cluster, "--kubeconfig", kubeconfig, "--wait", "2m")
	for _, image := range append([]string{img}, manifestImages(t)...) {
		if _, err := tryRun("", "docker", "image", "inspect", image); err != nil {
			run(t, "", "docker", "pull", image)
		}
		tool(t, "kind", "load", "docker-image", image, "--name", cluster)
	}

	// Deploy the default kustomization with the NetworkPolicy component and the image override.
	overlay, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name, tag, _ := strings.Cut(img, ":")
	// kustomize rejects absolute resource paths.
	rel := func(dir ...string) string {
		p, err := filepath.Rel(overlay, filepath.Join(append([]string{root, "config"}, dir...)...))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	kustomization := fmt.Sprintf("resources:\n  - %s\ncomponents:\n  - %s\n"+
		"images:\n  - name: ghcr.io/abinnovision/hatchet-controller\n    newName: %s\n    newTag: %s\n",
		rel("default"), rel("components", "networkpolicy"), name, tag)
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(kustomization), 0o600); err != nil {
		t.Fatal(err)
	}
	manifests := tool(t, "kustomize", "build", overlay)
	kubectl(t, "create", "namespace", "hatchet-controller-system")
	run(t, manifests, "kubectl", "apply", "--server-side", "-f", "-")
	kubectl(t, "-n", "hatchet-controller-system", "rollout", "status", "deployment/hatchet-controller-manager", "--timeout=3m")

	// hatchet-lite waits on the keyset Secret the instance generates.
	kubectl(t, "apply", "-f", "test/e2e/testdata/hatchet.yaml")
	kubectl(t, "-n", "hatchet", "wait", "hatchetinstance/hatchet", "--for=condition=Ready", "--timeout=2m")
	kubectl(t, "-n", "hatchet", "rollout", "status", "deployment/postgres", "--timeout=3m")
	kubectl(t, "-n", "hatchet", "rollout", "status", "deployment/hatchet-lite", "--timeout=5m")

	// The service user must exist in Hatchet before tenants are reconciled.
	kubectl(t, "-n", "hatchet", "exec", "deployment/postgres", "--", "psql", "-U", "hatchet", "-d", "hatchet", "-v", "ON_ERROR_STOP=1", "-c",
		fmt.Sprintf(`INSERT INTO "User" (id, email, "emailVerified") VALUES ('%s', 'hatchet-controller@example.com', true)`, serviceUserID))

	kubectl(t, "apply", "-f", "test/e2e/testdata/tenant.yaml")
	if _, err := tryRun("", "kubectl", "-n", "team-a", "wait", "hatchettenant/orders", "--for=condition=Ready", "--timeout=3m"); err != nil {
		t.Fatalf("tenant not Ready:\n%s\n%s", kubectl(t, "-n", "team-a", "get", "hatchettenant/orders", "-o", "yaml"),
			kubectl(t, "-n", "hatchet-controller-system", "logs", "deployment/hatchet-controller-manager", "--tail=50"))
	}
	if token := kubectl(t, "-n", "team-a", "get", "secret", "orders-hatchet", "-o", "jsonpath={.data.HATCHET_CLIENT_TOKEN}"); token == "" {
		t.Fatal("worker secret has no token")
	}
	// Requires the networkpolicy component's RBAC and flag.
	kubectl(t, "-n", "team-a", "get", "networkpolicy/hatchet-worker-egress-orders")

	// Owned Secrets are garbage collected once their owners are gone.
	kubectl(t, "-n", "team-a", "delete", "hatchettenant/orders", "--wait=true", "--timeout=2m")
	waitDeleted(t, "team-a", "secret/orders-hatchet")
	kubectl(t, "-n", "hatchet", "delete", "hatchetinstance/hatchet", "--wait=true", "--timeout=2m")
	waitDeleted(t, "hatchet", "secret/hatchet-signing-key")
	waitDeleted(t, "hatchet", "secret/hatchet-exchange-keyset")
}

func waitDeleted(t *testing.T, namespace, resource string) {
	t.Helper()
	eventually(t, 2*time.Minute, func() error {
		out, err := tryRun("", "kubectl", "-n", namespace, "get", resource, "--ignore-not-found", "-o", "name")
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("%s still exists", resource)
		}
		return nil
	})
}
