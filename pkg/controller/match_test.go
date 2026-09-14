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

func TestMatches(t *testing.T) {
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
