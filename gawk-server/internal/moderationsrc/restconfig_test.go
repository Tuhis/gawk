package moderationsrc

import (
	"os"
	"path/filepath"
	"testing"
)

// The kubeconfig fallback (docs/41's compose lane): outside a pod, KUBECONFIG
// is how the relay's k8s ban source reaches an API server. The in-cluster
// path cannot be exercised here — it needs the serviceaccount mount — but the
// fallback's two outcomes can.
func TestRestConfigFallsBackToKubeconfig(t *testing.T) {
	// Blanked explicitly, the source_test precedent: gawk's own CI runners
	// are pods, where the in-cluster path would win and the fallback under
	// test would never run.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: dev
    cluster:
      server: https://kube-apiserver:6443
      insecure-skip-tls-verify: true
contexts:
  - name: dev
    context: {cluster: dev, user: dev}
current-context: dev
users:
  - name: dev
    user: {token: not-a-real-token}
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)

	cfg, err := restConfig()
	if err != nil {
		t.Fatalf("restConfig with KUBECONFIG set: %v", err)
	}
	if cfg.Host != "https://kube-apiserver:6443" {
		t.Fatalf("resolved server = %q, want the kubeconfig's", cfg.Host)
	}
	if cfg.BearerToken != "not-a-real-token" {
		t.Fatalf("resolved credential did not come from the kubeconfig")
	}
}

func TestRestConfigFailsPlainlyWithNeitherSource(t *testing.T) {
	// No in-cluster env (the CI runners are pods — see above), and a
	// KUBECONFIG naming nothing.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	if _, err := restConfig(); err == nil {
		t.Fatal("restConfig succeeded with neither in-cluster config nor a kubeconfig")
	}
}
