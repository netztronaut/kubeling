package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://kubeling.example:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: not-a-real-token
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	// Keep rest.InClusterConfig from succeeding when the tests themselves
	// run inside a pod.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	t.Run("explicit kubeconfig", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		cfg, err := loadConfig(writeKubeconfig(t))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "https://kubeling.example:6443" {
			t.Errorf("host = %q", cfg.Host)
		}
		if cfg.BearerToken != "not-a-real-token" {
			t.Errorf("bearer token was not loaded from the kubeconfig")
		}
	})

	t.Run("missing explicit kubeconfig is an error", func(t *testing.T) {
		if _, err := loadConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("falls back to the default loading rules outside a cluster", func(t *testing.T) {
		t.Setenv("KUBECONFIG", writeKubeconfig(t))
		cfg, err := loadConfig("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "https://kubeling.example:6443" {
			t.Errorf("host = %q", cfg.Host)
		}
	})
}

func TestHealthHandler(t *testing.T) {
	srv := httptest.NewServer(healthHandler())
	defer srv.Close()

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		body := make([]byte, 16)
		n, _ := resp.Body.Read(body)
		if got := string(body[:n]); got != "ok" {
			t.Errorf("body = %q, want %q", got, "ok")
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}
