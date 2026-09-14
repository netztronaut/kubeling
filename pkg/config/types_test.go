package config

import "testing"

func TestConfigValidate(t *testing.T) {
	t.Run("no rules is valid", func(t *testing.T) {
		cfg := Config{}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unset providerIDPattern is valid", func(t *testing.T) {
		cfg := Config{Labels: map[string]LabelRule{
			"a": {Match: Match{NodeSelector: map[string]string{"k": "v"}}, Labels: map[string]string{"x": "y"}},
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid pattern is valid", func(t *testing.T) {
		cfg := Config{
			ExternalIPs: map[string]ExternalIPRule{
				"a": {Match: Match{ProviderIDPattern: `^custom://edge-`}, ExternalIPs: []string{"203.0.113.1"}},
			},
			Labels: map[string]LabelRule{
				"b": {Match: Match{ProviderIDPattern: `^custom://edge-`}, Labels: map[string]string{"x": "y"}},
			},
			Annotations: map[string]AnnotationRule{
				"c": {Match: Match{ProviderIDPattern: `^custom://edge-`}, Annotations: map[string]string{"x": "y"}},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("invalid pattern in externalIPs is rejected", func(t *testing.T) {
		cfg := Config{ExternalIPs: map[string]ExternalIPRule{
			"broken": {Match: Match{ProviderIDPattern: `(`}},
		}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected an error for an unparseable regex")
		}
	})

	t.Run("invalid pattern in labels is rejected", func(t *testing.T) {
		cfg := Config{Labels: map[string]LabelRule{
			"broken": {Match: Match{ProviderIDPattern: `(`}},
		}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected an error for an unparseable regex")
		}
	})

	t.Run("invalid pattern in annotations is rejected", func(t *testing.T) {
		cfg := Config{Annotations: map[string]AnnotationRule{
			"broken": {Match: Match{ProviderIDPattern: `(`}},
		}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected an error for an unparseable regex")
		}
	})
}
