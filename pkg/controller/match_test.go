package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/netztronaut/kubeling/pkg/config"
)

func nodeWithProviderID(providerID string) *corev1.Node {
	return &corev1.Node{Spec: corev1.NodeSpec{ProviderID: providerID}}
}

func nodeNamed(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func expr(key string, op corev1.NodeSelectorOperator, values ...string) corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{Key: key, Operator: op, Values: values}
}

func TestMatches(t *testing.T) {
	const zone = "topology.kubernetes.io/zone"
	zoneIn := corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
		expr(zone, corev1.NodeSelectorOpIn, "antarctica-east1", "antarctica-west1"),
	}}

	tests := []struct {
		name  string
		node  *corev1.Node
		match config.Match
		want  bool
	}{
		{
			name:  "no constraints matches anything",
			node:  &corev1.Node{},
			match: config.Match{},
			want:  true,
		},
		{
			name:  "nodeSelector matches",
			node:  &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"env": "prod"}}},
			match: config.Match{NodeSelector: map[string]string{"env": "prod"}},
			want:  true,
		},
		{
			name:  "nodeSelector does not match",
			node:  &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"env": "staging"}}},
			match: config.Match{NodeSelector: map[string]string{"env": "prod"}},
			want:  false,
		},
		{
			name:  "providerIDPattern matches",
			node:  nodeWithProviderID("custom://edge-1"),
			match: config.Match{ProviderIDPattern: `^custom://edge-`},
			want:  true,
		},
		{
			name:  "providerIDPattern does not match",
			node:  nodeWithProviderID("custom://core-1"),
			match: config.Match{ProviderIDPattern: `^custom://edge-`},
			want:  false,
		},
		{
			name:  "empty providerID never matches a set pattern",
			node:  &corev1.Node{},
			match: config.Match{ProviderIDPattern: `^custom://edge-`},
			want:  false,
		},
		{
			name: "both constraints must hold",
			node: func() *corev1.Node {
				n := nodeWithProviderID("custom://edge-1")
				n.Labels = map[string]string{"env": "prod"}
				return n
			}(),
			match: config.Match{NodeSelector: map[string]string{"env": "prod"}, ProviderIDPattern: `^custom://edge-`},
			want:  true,
		},
		{
			name: "one of two constraints failing is a non-match",
			node: func() *corev1.Node {
				n := nodeWithProviderID("custom://core-1")
				n.Labels = map[string]string{"env": "prod"}
				return n
			}(),
			match: config.Match{NodeSelector: map[string]string{"env": "prod"}, ProviderIDPattern: `^custom://edge-`},
			want:  false,
		},
		{
			name:  "selectorTerms In matches",
			node:  nodeNamed("n1", map[string]string{zone: "antarctica-west1"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{zoneIn}},
			want:  true,
		},
		{
			name:  "selectorTerms In does not match",
			node:  nodeNamed("n1", map[string]string{zone: "europe-central1"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{zoneIn}},
			want:  false,
		},
		{
			name: "selectorTerms NotIn and DoesNotExist",
			node: nodeNamed("n1", map[string]string{zone: "europe-central1"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				expr(zone, corev1.NodeSelectorOpNotIn, "antarctica-east1"),
				expr("example.com/opt-out", corev1.NodeSelectorOpDoesNotExist),
			}}}},
			want: true,
		},
		{
			name: "selectorTerms Exists fails on missing label",
			node: nodeNamed("n1", nil),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				expr(zone, corev1.NodeSelectorOpExists),
			}}}},
			want: false,
		},
		{
			name: "requirements within a term are ANDed",
			node: nodeNamed("n1", map[string]string{zone: "antarctica-east1"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				expr(zone, corev1.NodeSelectorOpIn, "antarctica-east1"),
				expr("env", corev1.NodeSelectorOpIn, "prod"),
			}}}},
			want: false,
		},
		{
			name: "terms are ORed",
			node: nodeNamed("n1", map[string]string{"env": "prod"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{
				zoneIn,
				{MatchExpressions: []corev1.NodeSelectorRequirement{expr("env", corev1.NodeSelectorOpIn, "prod")}},
			}},
			want: true,
		},
		{
			name: "matchFields on metadata.name",
			node: nodeNamed("server-a", nil),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
				expr("metadata.name", corev1.NodeSelectorOpIn, "server-b"),
			}}}},
			want: false,
		},
		{
			name: "nodeSelector and selectorTerms must both hold",
			node: nodeNamed("n1", map[string]string{zone: "antarctica-east1", "env": "staging"}),
			match: config.Match{
				NodeSelector:  map[string]string{"env": "prod"},
				SelectorTerms: []corev1.NodeSelectorTerm{zoneIn},
			},
			want: false,
		},
		{
			name: "nodeSelector and selectorTerms both holding matches",
			node: nodeNamed("n1", map[string]string{zone: "antarctica-east1", "env": "prod"}),
			match: config.Match{
				NodeSelector:  map[string]string{"env": "prod"},
				SelectorTerms: []corev1.NodeSelectorTerm{zoneIn},
			},
			want: true,
		},
		{
			name: "invalid selectorTerms never match",
			node: nodeNamed("n1", map[string]string{zone: "antarctica-east1"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				expr(zone, "Bogus", "antarctica-east1"),
			}}}},
			want: false,
		},
		{
			name:  "invalid pattern never matches",
			node:  nodeWithProviderID("custom://edge-1"),
			match: config.Match{ProviderIDPattern: `(`},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matches(tt.node, tt.match); got != tt.want {
				t.Errorf("matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchingIDs(t *testing.T) {
	rules := map[string]config.LabelRule{
		"edge": {Match: config.Match{ProviderIDPattern: `^custom://edge-`}},
		"core": {Match: config.Match{ProviderIDPattern: `^custom://core-`}},
		"all":  {},
	}
	match := func(r config.LabelRule) config.Match { return r.Match }

	got := matchingIDs(nodeWithProviderID("custom://edge-1"), rules, match)
	want := []string{"all", "edge"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMatchesMoreCases(t *testing.T) {
	tests := []struct {
		name  string
		node  *corev1.Node
		match config.Match
		want  bool
	}{
		{
			name:  "nodeSelector requires every key",
			node:  nodeNamed("n1", map[string]string{"env": "prod"}),
			match: config.Match{NodeSelector: map[string]string{"env": "prod", "zone": "edge"}},
			want:  false,
		},
		{
			name:  "nodeSelector against a node without labels",
			node:  nodeNamed("n1", nil),
			match: config.Match{NodeSelector: map[string]string{"env": "prod"}},
			want:  false,
		},
		{
			name:  "empty nodeSelector imposes no constraint",
			node:  nodeNamed("n1", nil),
			match: config.Match{NodeSelector: map[string]string{}},
			want:  true,
		},
		{
			name:  "unanchored pattern matches a substring",
			node:  nodeWithProviderID("custom://rack-1/edge-7"),
			match: config.Match{ProviderIDPattern: `edge-\d+`},
			want:  true,
		},
		{
			name:  "pattern that allows an empty providerID",
			node:  &corev1.Node{},
			match: config.Match{ProviderIDPattern: `^$`},
			want:  true,
		},
		{
			name: "matchFields on metadata.name matches",
			node: nodeNamed("server-a", nil),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
				expr("metadata.name", corev1.NodeSelectorOpIn, "server-a"),
			}}}},
			want: true,
		},
		{
			name: "matchFields NotIn",
			node: nodeNamed("server-a", nil),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
				expr("metadata.name", corev1.NodeSelectorOpNotIn, "server-b"),
			}}}},
			want: true,
		},
		{
			name: "matchExpressions and matchFields within one term are ANDed",
			node: nodeNamed("server-a", map[string]string{"env": "staging"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{expr("env", corev1.NodeSelectorOpIn, "prod")},
				MatchFields:      []corev1.NodeSelectorRequirement{expr("metadata.name", corev1.NodeSelectorOpIn, "server-a")},
			}}},
			want: false,
		},
		{
			name: "selectorTerms Gt",
			node: nodeNamed("n1", map[string]string{"example.com/cores": "32"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				expr("example.com/cores", corev1.NodeSelectorOpGt, "16"),
			}}}},
			want: true,
		},
		{
			name: "selectorTerms Lt",
			node: nodeNamed("n1", map[string]string{"example.com/cores": "32"}),
			match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				expr("example.com/cores", corev1.NodeSelectorOpLt, "16"),
			}}}},
			want: false,
		},
		{
			name: "every constraint type holding",
			node: func() *corev1.Node {
				n := nodeNamed("edge-1", map[string]string{"env": "prod"})
				n.Spec.ProviderID = "custom://edge-1"
				return n
			}(),
			match: config.Match{
				NodeSelector: map[string]string{"env": "prod"},
				SelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
					expr("metadata.name", corev1.NodeSelectorOpIn, "edge-1"),
				}}},
				ProviderIDPattern: `^custom://edge-`,
			},
			want: true,
		},
		{
			name: "providerIDPattern failing with every other constraint holding",
			node: func() *corev1.Node {
				n := nodeNamed("edge-1", map[string]string{"env": "prod"})
				n.Spec.ProviderID = "metal://edge-1"
				return n
			}(),
			match: config.Match{
				NodeSelector: map[string]string{"env": "prod"},
				SelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
					expr("metadata.name", corev1.NodeSelectorOpIn, "edge-1"),
				}}},
				ProviderIDPattern: `^custom://edge-`,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matches(tt.node, tt.match); got != tt.want {
				t.Errorf("matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompilePattern(t *testing.T) {
	const pattern = `^custom://compile-pattern-test-`

	first, err := compilePattern(pattern)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compilePattern(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("compilePattern did not return the cached *regexp.Regexp")
	}
	if !first.MatchString("custom://compile-pattern-test-1") {
		t.Error("compiled pattern does not match")
	}

	const invalid = `compile-pattern-test-(`
	if _, err := compilePattern(invalid); err == nil {
		t.Fatal("expected an error for an invalid pattern")
	}
	if _, cached := patterns.Load(invalid); cached {
		t.Error("an invalid pattern was cached")
	}
}

func TestMatchingIDsWithoutMatches(t *testing.T) {
	match := func(r config.AnnotationRule) config.Match { return r.Match }

	if got := matchingIDs(nodeNamed("n1", nil), map[string]config.AnnotationRule(nil), match); got != nil {
		t.Errorf("nil rules: got %v, want nil", got)
	}
	rules := map[string]config.AnnotationRule{
		"edge": {Match: config.Match{NodeSelector: map[string]string{"zone": "edge"}}},
	}
	if got := matchingIDs(nodeNamed("n1", nil), rules, match); got != nil {
		t.Errorf("no matching rules: got %v, want nil", got)
	}
}

func TestMatchingIDsIsSorted(t *testing.T) {
	rules := map[string]config.ExternalIPRule{}
	for _, id := range []string{"zeta", "alpha", "mu", "beta", "10", "9"} {
		rules[id] = config.ExternalIPRule{}
	}
	match := func(r config.ExternalIPRule) config.Match { return r.Match }

	want := []string{"10", "9", "alpha", "beta", "mu", "zeta"}
	for range 20 { // map iteration order is randomized
		if got := matchingIDs(&corev1.Node{}, rules, match); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
