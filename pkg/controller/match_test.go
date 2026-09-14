package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/steigr/kubeling/pkg/config"
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
