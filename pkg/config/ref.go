package config

import (
	"os"
	"strings"
)

const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// ParseRef splits a "(namespace/)name" ConfigMap reference. An empty
// namespace means the caller should fall back to OwnNamespace.
func ParseRef(ref string) (namespace, name string) {
	if before, after, ok := strings.Cut(ref, "/"); ok {
		return before, after
	}
	return "", ref
}

// OwnNamespace returns the namespace the current process is running in, as
// seen by the in-cluster service account namespace file that Kubernetes
// projects into every pod. It defaults to "default" when that file can't be
// read, e.g. when running outside a cluster.
func OwnNamespace() string {
	if data, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return "default"
}
