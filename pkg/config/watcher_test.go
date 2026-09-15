package config

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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

func TestParseDecodesRules(t *testing.T) {
	raw := `
externalIPs:
  edge:
    nodeSelector:
      zone: edge
    providerIDPattern: '^custom://edge-'
    externalIPs: [203.0.113.10, "2001:db8::10"]
labels:
  zoned:
    selectorTerms:
      - matchExpressions:
          - key: topology.kubernetes.io/zone
            operator: In
            values: [antarctica-east1]
      - matchFields:
          - key: metadata.name
            operator: In
            values: [server-a]
    labels:
      environment: production
annotations:
  rack:
    annotations:
      example.com/rack: r42
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}

	eip := cfg.ExternalIPs["edge"]
	if eip.NodeSelector["zone"] != "edge" || eip.ProviderIDPattern != "^custom://edge-" {
		t.Errorf("externalIPs match = %+v", eip.Match)
	}
	if len(eip.ExternalIPs) != 2 || eip.ExternalIPs[1] != "2001:db8::10" {
		t.Errorf("externalIPs = %v", eip.ExternalIPs)
	}

	labels := cfg.Labels["zoned"]
	if len(labels.SelectorTerms) != 2 {
		t.Fatalf("selectorTerms = %+v", labels.SelectorTerms)
	}
	if req := labels.SelectorTerms[0].MatchExpressions[0]; req.Operator != corev1.NodeSelectorOpIn || req.Values[0] != "antarctica-east1" {
		t.Errorf("matchExpressions = %+v", req)
	}
	if req := labels.SelectorTerms[1].MatchFields[0]; req.Key != "metadata.name" || req.Values[0] != "server-a" {
		t.Errorf("matchFields = %+v", req)
	}
	if labels.Labels["environment"] != "production" {
		t.Errorf("labels = %v", labels.Labels)
	}

	if got := cfg.Annotations["rack"].Annotations["example.com/rack"]; got != "r42" {
		t.Errorf("annotation = %q", got)
	}
}

func TestParseAcceptsJSON(t *testing.T) {
	cfg, err := Parse([]byte(`{"labels": {"a": {"labels": {"x": "y"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Labels["a"].Labels["x"] != "y" {
		t.Errorf("labels = %+v", cfg.Labels)
	}
}

func TestParseReturnsZeroConfigOnError(t *testing.T) {
	cfg, err := Parse([]byte("labels:\n  a:\n    providerIDPattern: '('\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if cfg.Labels != nil || cfg.Annotations != nil || cfg.ExternalIPs != nil {
		t.Errorf("Parse returned a partial configuration alongside its error: %+v", cfg)
	}
}

func TestWatcherLoadIgnoresNonConfigMaps(t *testing.T) {
	w, err := NewWatcher(fake.NewClientset(), "kube-system", "kubeling", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	w.load(&corev1.ConfigMap{Data: map[string]string{Key: "labels:\n  a: {}\n"}})
	changes := 0
	w.OnChange = func() { changes++ }

	w.load(&corev1.Secret{})
	w.load("kube-system/kubeling")

	if len(w.Current().Labels) != 1 || changes != 0 {
		t.Errorf("non-ConfigMap object changed the configuration (changes = %d)", changes)
	}
}

func TestWatcherWithoutOnChange(t *testing.T) {
	w, err := NewWatcher(fake.NewClientset(), "kube-system", "kubeling", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := w.Current(); cfg.Labels != nil || cfg.Annotations != nil || cfg.ExternalIPs != nil {
		t.Fatalf("Current before any load = %+v, want zero value", cfg)
	}

	w.load(&corev1.ConfigMap{Data: map[string]string{Key: "labels:\n  a: {}\n"}})
	w.clear()

	if len(w.Current().Labels) != 0 {
		t.Errorf("clear did not reset the configuration")
	}
}

func TestWatcherRun(t *testing.T) {
	const namespace, name = "kube-system", "kubeling"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       map[string]string{Key: "labels:\n  a:\n    labels:\n      environment: production\n"},
	}
	client := fake.NewClientset(cm)
	w, err := NewWatcher(client, namespace, name, 0)
	if err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{}, 10)
	w.OnChange = func() { changed <- struct{}{} }

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	waitFor := func(t *testing.T, what string, cond func(Config) bool) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			if cond(w.Current()) {
				return
			}
			select {
			case <-changed:
			case <-timeout:
				t.Fatalf("timed out waiting for %s; current = %+v", what, w.Current())
			}
		}
	}

	waitFor(t, "initial load", func(c Config) bool {
		return c.Labels["a"].Labels["environment"] == "production"
	})

	for _, a := range client.Actions() {
		var fieldSelector string
		switch a := a.(type) {
		case clienttesting.ListAction:
			fieldSelector = a.GetListRestrictions().Fields.String()
		case clienttesting.WatchAction:
			fieldSelector = a.GetWatchRestrictions().Fields.String()
		default:
			continue
		}
		if fieldSelector != "metadata.name="+name {
			t.Errorf("%s field selector = %q, want metadata.name=%s", a.GetVerb(), fieldSelector, name)
		}
		if a.GetNamespace() != namespace {
			t.Errorf("%s namespace = %q, want %q", a.GetVerb(), a.GetNamespace(), namespace)
		}
	}

	cm.Data[Key] = "annotations:\n  b:\n    annotations:\n      example.com/rack: r42\n"
	if _, err := client.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "update", func(c Config) bool {
		return len(c.Labels) == 0 && c.Annotations["b"].Annotations["example.com/rack"] == "r42"
	})

	if err := client.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delete", func(c Config) bool {
		return len(c.Annotations) == 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was canceled")
	}
}
