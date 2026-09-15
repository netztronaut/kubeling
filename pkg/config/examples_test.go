package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// TestExamples keeps the example configurations shipped with the chart and
// the plain manifests in sync with the schema Parse accepts.
func TestExamples(t *testing.T) {
	chartValues, err := filepath.Glob("../../charts/kubeling/ci/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range chartValues {
		t.Run(strings.TrimPrefix(path, "../../"), func(t *testing.T) {
			var values struct {
				Config map[string]any `json:"config"`
			}
			readYAML(t, path, &values)
			raw, err := yaml.Marshal(values.Config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(raw); err != nil {
				t.Errorf("chart config: %v", err)
			}
		})
	}

	t.Run("deploy/examples/configmap.yaml", func(t *testing.T) {
		var cm corev1.ConfigMap
		readYAML(t, "../../deploy/examples/configmap.yaml", &cm)
		cfg, err := Parse([]byte(cm.Data[Key]))
		if err != nil {
			t.Fatalf("configmap %s: %v", Key, err)
		}
		if len(cfg.Initialization) == 0 || len(cfg.ExternalIPs) == 0 || len(cfg.Labels) == 0 || len(cfg.Annotations) == 0 {
			t.Errorf("example should demonstrate every rule map, got %+v", cfg)
		}
		if len(cfg.Labels["edge"].SelectorTerms) == 0 {
			t.Errorf("example should demonstrate selectorTerms, got %+v", cfg.Labels)
		}
	})
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
