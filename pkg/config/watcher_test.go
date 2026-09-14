package config

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "empty document", raw: ``},
		{
			name: "all three rule maps",
			raw: `
externalIPs:
  edge:
    nodeSelector:
      zone: edge
    externalIPs: [203.0.113.10]
labels:
  edge:
    providerIDPattern: '^custom://edge-'
    labels:
      environment: production
annotations:
  edge:
    annotations:
      example.com/rack: r42
`,
		},
		{
			name: "retired policies schema is rejected",
			raw: `
policies:
  edge:
    nodeSelector:
      zone: edge
    externalIPs: [203.0.113.10]
`,
			wantErr: `unknown field "policies"`,
		},
		{
			name: "unknown rule field is rejected",
			raw: `
labels:
  edge:
    nodeSelectr:
      zone: edge
`,
			wantErr: `unknown field "nodeSelectr"`,
		},
		{
			name:    "malformed yaml is rejected",
			raw:     "labels: [",
			wantErr: "parsing config.yaml",
		},
		{
			name: "invalid pattern is rejected",
			raw: `
annotations:
  broken:
    providerIDPattern: '('
`,
			wantErr: `annotations rule "broken"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.raw))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestWatcherLoad(t *testing.T) {
	configMap := func(data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{Data: data}
	}
	valid := configMap(map[string]string{Key: "labels:\n  a:\n    labels:\n      environment: production\n"})

	w, err := NewWatcher(fake.NewClientset(), "kube-system", "kubeling", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	changes := 0
	w.OnChange = func() { changes++ }

	w.load(valid)
	if got := w.Current().Labels["a"].Labels["environment"]; got != "production" || changes != 1 {
		t.Fatalf("after valid load: label = %q, changes = %d", got, changes)
	}

	w.load(configMap(map[string]string{Key: "policies: {}\n"}))
	if len(w.Current().Labels) != 1 || changes != 1 {
		t.Fatalf("invalid load replaced the configuration or fired OnChange (changes = %d)", changes)
	}

	w.load(configMap(map[string]string{"other.yaml": ""}))
	if len(w.Current().Labels) != 0 || changes != 2 {
		t.Fatalf("missing key did not clear the configuration (changes = %d)", changes)
	}

	w.load(valid)
	w.clear()
	if len(w.Current().Labels) != 0 || changes != 4 {
		t.Fatalf("clear did not reset the configuration (changes = %d)", changes)
	}
}
