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
	// An empty dir: no in-cluster mount, and a KUBECONFIG naming nothing.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	if _, err := restConfig(); err == nil {
		t.Fatal("restConfig succeeded with neither in-cluster config nor a kubeconfig")
	}
}
