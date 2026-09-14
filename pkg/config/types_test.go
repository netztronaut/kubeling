package config

import "testing"

func TestConfigValidate(t *testing.T) {
	t.Run("no onboarding rules is valid", func(t *testing.T) {
		cfg := Config{}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid pattern is valid", func(t *testing.T) {
		cfg := Config{Onboarding: map[string]OnboardingRule{
			"edge": {ProviderIDPattern: `^custom://edge-`, Label: "kubeling.io/onboarded", LabelValue: "true"},
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("invalid pattern is rejected", func(t *testing.T) {
		cfg := Config{Onboarding: map[string]OnboardingRule{
			"broken": {ProviderIDPattern: `(`, Label: "x", LabelValue: "y"},
		}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected an error for an unparseable regex")
		}
	})
}
