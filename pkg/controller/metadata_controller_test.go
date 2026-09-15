package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/netztronaut/kubeling/pkg/config"
)

func TestApplyOverwrite(t *testing.T) {
	tests := []struct {
		name        string
		existing    map[string]string
		desired     map[string]string
		want        map[string]string
		wantChanged bool
	}{
		{
			name:        "no desired keys is a no-op",
			existing:    map[string]string{"a": "1"},
			desired:     nil,
			want:        map[string]string{"a": "1"},
			wantChanged: false,
		},
		{
			name:        "adds a missing key",
			existing:    map[string]string{"a": "1"},
			desired:     map[string]string{"b": "2"},
			want:        map[string]string{"a": "1", "b": "2"},
			wantChanged: true,
		},
		{
			name:        "overwrites a differing value",
			existing:    map[string]string{"a": "1"},
			desired:     map[string]string{"a": "2"},
			want:        map[string]string{"a": "2"},
			wantChanged: true,
		},
		{
			name:        "already matching is a no-op",
			existing:    map[string]string{"a": "1"},
			desired:     map[string]string{"a": "1"},
			want:        map[string]string{"a": "1"},
			wantChanged: false,
		},
		{
			name:        "nil existing map",
			existing:    nil,
			desired:     map[string]string{"a": "1"},
			want:        map[string]string{"a": "1"},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := applyOverwrite(tt.existing, tt.desired)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveValues(t *testing.T) {
	values := func(r config.LabelRule) map[string]string { return r.Labels }

	t.Run("agreeing rules both contribute", func(t *testing.T) {
		rules := map[string]config.LabelRule{
			"a": {Labels: map[string]string{"env": "prod"}},
			"b": {Labels: map[string]string{"rack": "r1"}},
		}
		got := resolveValues("labels", "node-1", []string{"a", "b"}, rules, values)
		want := map[string]string{"env": "prod", "rack": "r1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("agreeing on the same key is fine", func(t *testing.T) {
		rules := map[string]config.LabelRule{
			"a": {Labels: map[string]string{"env": "prod"}},
			"b": {Labels: map[string]string{"env": "prod"}},
		}
		got := resolveValues("labels", "node-1", []string{"a", "b"}, rules, values)
		want := map[string]string{"env": "prod"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("conflicting values drop the key entirely", func(t *testing.T) {
		rules := map[string]config.LabelRule{
			"a": {Labels: map[string]string{"env": "prod", "rack": "r1"}},
			"b": {Labels: map[string]string{"env": "staging"}},
		}
		got := resolveValues("labels", "node-1", []string{"a", "b"}, rules, values)
		want := map[string]string{"rack": "r1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
}

func TestLabelControllerReconcile(t *testing.T) {
	edgeRule := config.LabelRule{
		Match:  config.Match{NodeSelector: map[string]string{"zone": "edge"}},
		Labels: map[string]string{"environment": "production"},
	}

	t.Run("applies matching rule from cold", func(t *testing.T) {
		h := newHarness(t, node("edge-1", map[string]string{"zone": "edge"}))
		c, err := NewLabelController(h.client, h.nodes, &staticConfig{config.Config{
			Labels: map[string]config.LabelRule{"edge": edgeRule},
		}})
		if err != nil {
			t.Fatal(err)
		}

		writes, got := h.converge(c.reconcile, "edge-1")

		// Pending condition, labels, Applied condition.
		if writes != 3 {
			t.Errorf("writes = %d, want 3", writes)
		}
		if got.Labels["environment"] != "production" {
			t.Errorf("labels = %v, want environment=production", got.Labels)
		}
		cond := condition(got, LabeledConditionType)
		if cond == nil || cond.Status != corev1.ConditionTrue || cond.Reason != "Applied" {
			t.Errorf("condition = %+v, want Applied", cond)
		}
	})

	t.Run("overwrites a differing value", func(t *testing.T) {
		h := newHarness(t, node("edge-1", map[string]string{"zone": "edge", "environment": "staging"}))
		c, err := NewLabelController(h.client, h.nodes, &staticConfig{config.Config{
			Labels: map[string]config.LabelRule{"edge": edgeRule},
		}})
		if err != nil {
			t.Fatal(err)
		}

		_, got := h.converge(c.reconcile, "edge-1")

		if got.Labels["environment"] != "production" {
			t.Errorf("labels = %v, want environment=production", got.Labels)
		}
	})

	t.Run("leaves non-matching node untouched", func(t *testing.T) {
		h := newHarness(t, node("core-1", map[string]string{"zone": "core"}))
		c, err := NewLabelController(h.client, h.nodes, &staticConfig{config.Config{
			Labels: map[string]config.LabelRule{"edge": edgeRule},
		}})
		if err != nil {
			t.Fatal(err)
		}

		writes, got := h.converge(c.reconcile, "core-1")

		if writes != 0 {
			t.Errorf("writes = %d, want 0", writes)
		}
		if condition(got, LabeledConditionType) != nil {
			t.Errorf("unexpected condition on non-matching node")
		}
	})

	for name, cfg := range map[string]config.Config{
		"rule no longer matches": {Labels: map[string]config.LabelRule{"other": {
			Match:  config.Match{NodeSelector: map[string]string{"zone": "core"}},
			Labels: map[string]string{"environment": "production"},
		}}},
		"all rules removed": {},
	} {
		t.Run("clears condition but keeps labels when "+name, func(t *testing.T) {
			h := newHarness(t, node("edge-1", map[string]string{"zone": "edge"}))
			source := &staticConfig{config.Config{Labels: map[string]config.LabelRule{"edge": edgeRule}}}
			c, err := NewLabelController(h.client, h.nodes, source)
			if err != nil {
				t.Fatal(err)
			}
			h.converge(c.reconcile, "edge-1")

			source.cfg = cfg
			writes, got := h.converge(c.reconcile, "edge-1")

			if writes != 1 {
				t.Errorf("writes = %d, want 1", writes)
			}
			if condition(got, LabeledConditionType) != nil {
				t.Errorf("condition still present after rule stopped matching")
			}
			if got.Labels["environment"] != "production" {
				t.Errorf("applied label was removed: %v", got.Labels)
			}
		})
	}
}

func TestAnnotationControllerReconcile(t *testing.T) {
	h := newHarness(t, nodeWithName(nodeWithProviderID("custom://edge-1"), "edge-1"))
	c, err := NewAnnotationController(h.client, h.nodes, &staticConfig{config.Config{
		Annotations: map[string]config.AnnotationRule{"edge": {
			Match:       config.Match{ProviderIDPattern: `^custom://edge-`},
			Annotations: map[string]string{"example.com/rack": "r42"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	writes, got := h.converge(c.reconcile, "edge-1")

	if writes != 3 {
		t.Errorf("writes = %d, want 3", writes)
	}
	if got.Annotations["example.com/rack"] != "r42" {
		t.Errorf("annotations = %v, want example.com/rack=r42", got.Annotations)
	}
	if len(got.Labels) != 0 {
		t.Errorf("annotation controller touched labels: %v", got.Labels)
	}
	cond := condition(got, AnnotatedConditionType)
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Errorf("condition = %+v, want Applied", cond)
	}
}

func nodeWithName(n *corev1.Node, name string) *corev1.Node {
	n.Name = name
	return n
}
