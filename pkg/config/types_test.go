package config

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

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

	validTerms := []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"antarctica-east1", "antarctica-west1"}},
		}},
		{MatchFields: []corev1.NodeSelectorRequirement{
			{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"server-a"}},
		}},
	}

	t.Run("valid selectorTerms are valid", func(t *testing.T) {
		m := Match{SelectorTerms: validTerms}
		cfg := Config{
			ExternalIPs: map[string]ExternalIPRule{"a": {Match: m}},
			Labels:      map[string]LabelRule{"b": {Match: m}},
			Annotations: map[string]AnnotationRule{"c": {Match: m}},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	invalidTerms := map[string]corev1.NodeSelectorTerm{
		"empty term": {},
		"unknown operator": {MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "k", Operator: "Bogus", Values: []string{"v"}},
		}},
		"In without values": {MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "k", Operator: corev1.NodeSelectorOpIn},
		}},
		"Exists with values": {MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "k", Operator: corev1.NodeSelectorOpExists, Values: []string{"v"}},
		}},
		"Gt with non-integer": {MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "k", Operator: corev1.NodeSelectorOpGt, Values: []string{"many"}},
		}},
		"invalid label key": {MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "not a key!", Operator: corev1.NodeSelectorOpExists},
		}},
		"unsupported field key": {MatchFields: []corev1.NodeSelectorRequirement{
			{Key: "spec.providerID", Operator: corev1.NodeSelectorOpIn, Values: []string{"x"}},
		}},
		"matchFields In with two values": {MatchFields: []corev1.NodeSelectorRequirement{
			{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"a", "b"}},
		}},
	}
	for name, term := range invalidTerms {
		t.Run("selectorTerms with "+name+" is rejected", func(t *testing.T) {
			cfg := Config{Annotations: map[string]AnnotationRule{
				"broken": {Match: Match{SelectorTerms: []corev1.NodeSelectorTerm{term}}},
			}}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), `annotations rule "broken"`) {
				t.Errorf("error should name the rule, got %v", err)
			}
		})
	}
}

func TestParseRejectsUnknownSelectorTermKeys(t *testing.T) {
	raw := `
labels:
  typo:
    selectorTerms:
      - matchExpression:
          - key: k
            operator: Exists
    labels:
      x: y
`
	if _, err := Parse([]byte(raw)); err == nil {
		t.Fatalf("expected strict decoding to reject matchExpression")
	}
}

func TestConfigValidateNamesTheRule(t *testing.T) {
	broken := Match{ProviderIDPattern: `(`}
	tests := map[string]Config{
		`externalIPs rule "x"`: {ExternalIPs: map[string]ExternalIPRule{"x": {Match: broken}}},
		`labels rule "x"`:      {Labels: map[string]LabelRule{"x": {Match: broken}}},
		`annotations rule "x"`: {Annotations: map[string]AnnotationRule{"x": {Match: broken}}},
	}
	for want, cfg := range tests {
		t.Run(want, func(t *testing.T) {
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "invalid providerIDPattern") {
				t.Errorf("error = %v, want it to name %s and the pattern", err, want)
			}
		})
	}
}

func TestValidateSelectorTermsAcceptsEveryOperator(t *testing.T) {
	terms := []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "a", Operator: corev1.NodeSelectorOpIn, Values: []string{"1", "2"}},
			{Key: "b", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"1"}},
			{Key: "c", Operator: corev1.NodeSelectorOpExists},
			{Key: "d", Operator: corev1.NodeSelectorOpDoesNotExist},
			{Key: "e", Operator: corev1.NodeSelectorOpGt, Values: []string{"1"}},
			{Key: "f", Operator: corev1.NodeSelectorOpLt, Values: []string{"10"}},
		}},
		{MatchFields: []corev1.NodeSelectorRequirement{
			{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"server-a"}},
		}},
	}
	if err := validateSelectorTerms(terms); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateSelectorTermsErrorPositions(t *testing.T) {
	terms := []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "a", Operator: corev1.NodeSelectorOpExists}}},
		{MatchFields: []corev1.NodeSelectorRequirement{
			{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}},
			{Key: "spec.unschedulable", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
		}},
		{},
	}
	err := validateSelectorTerms(terms)
	if err == nil || !strings.Contains(err.Error(), "selectorTerms[1].matchFields[1]") {
		t.Errorf("error = %v, want it to point at selectorTerms[1].matchFields[1]", err)
	}
}
