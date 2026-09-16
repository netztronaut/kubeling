package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// metadataRule is a domain-neutral rule, turned into a config.LabelRule or
// config.AnnotationRule by a metadataDomainCase.
type metadataRule struct {
	match  config.Match
	values map[string]string
}

// metadataDomainCase lets every MetadataController scenario run against
// both the label and the annotation domain.
type metadataDomainCase struct {
	name          string
	conditionType corev1.NodeConditionType
	newReconcile  func(*harness, ConfigSource) func(context.Context, string) error
	config        func(map[string]metadataRule) config.Config
	get           func(*corev1.Node) map[string]string
	set           func(*corev1.Node, map[string]string)
	other         func(*corev1.Node) map[string]string
}

var metadataDomains = []metadataDomainCase{
	{
		name:          "labels",
		conditionType: LabeledConditionType,
		newReconcile: func(h *harness, source ConfigSource) func(context.Context, string) error {
			c, err := NewLabelController(h.client, h.nodes, source)
			if err != nil {
				h.t.Fatal(err)
			}
			h.queue = c.nodeQueue
			return c.reconcile
		},
		config: func(rules map[string]metadataRule) config.Config {
			cfg := config.Config{Labels: map[string]config.LabelRule{}}
			for id, r := range rules {
				cfg.Labels[id] = config.LabelRule{Match: r.match, Labels: r.values}
			}
			return cfg
		},
		get:   func(n *corev1.Node) map[string]string { return n.Labels },
		set:   func(n *corev1.Node, v map[string]string) { n.Labels = v },
		other: func(n *corev1.Node) map[string]string { return n.Annotations },
	},
	{
		name:          "annotations",
		conditionType: AnnotatedConditionType,
		newReconcile: func(h *harness, source ConfigSource) func(context.Context, string) error {
			c, err := NewAnnotationController(h.client, h.nodes, source)
			if err != nil {
				h.t.Fatal(err)
			}
			h.queue = c.nodeQueue
			return c.reconcile
		},
		config: func(rules map[string]metadataRule) config.Config {
			cfg := config.Config{Annotations: map[string]config.AnnotationRule{}}
			for id, r := range rules {
				cfg.Annotations[id] = config.AnnotationRule{Match: r.match, Annotations: r.values}
			}
			return cfg
		},
		get:   func(n *corev1.Node) map[string]string { return n.Annotations },
		set:   func(n *corev1.Node, v map[string]string) { n.Annotations = v },
		other: func(n *corev1.Node) map[string]string { return n.Labels },
	},
}

// edgeNode is matched by edgeSelector in the metadata scenarios below. It
// carries no labels of its own, so matching uses its providerID and the
// label domain starts out empty.
func edgeNode(d metadataDomainCase, values map[string]string) *corev1.Node {
	n := node("edge-1", nil)
	n.Spec.ProviderID = "custom://edge-1"
	d.set(n, values)
	return n
}

var edgeSelector = config.Match{ProviderIDPattern: `^custom://edge-`}

func TestMetadataControllerScenarios(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(*testing.T, metadataDomainCase)
	}{
		{"missing node is ignored", testMetadataMissingNode},
		{"values already present only need the Applied condition", testMetadataAlreadyPresent},
		{"unrelated keys and the other domain are preserved", testMetadataPreservesUnrelated},
		{"merges matching rules and leaves conflicting keys untouched", testMetadataMergesAndSkipsConflicts},
		{"rules that only conflict are reported as Applied without writing values", testMetadataOnlyConflicts},
		{"matching rule without values is Applied", testMetadataRuleWithoutValues},
		{"newly matching rule with satisfied values only updates the message", testMetadataNewlyMatchingRule},
		{"values changed after holding are restored while staying Applied", testMetadataRestoresDrift},
		{"values changed right after being applied are restored after a cooldown", testMetadataCoolsDownFlaps},
		{"changed rules go through Pending instead of a cooldown", testMetadataRuleChangeIsNoFlap},
		{"an already Pending node goes straight to writing values", testMetadataAlreadyPending},
		{"rule removed while Pending clears the condition", testMetadataRemovedWhilePending},
		{"write errors", testMetadataWriteErrors},
	}
	for _, d := range metadataDomains {
		t.Run(d.name, func(t *testing.T) {
			for _, s := range scenarios {
				t.Run(s.name, func(t *testing.T) { s.run(t, d) })
			}
		})
	}
}

func testMetadataMissingNode(t *testing.T, d metadataDomainCase) {
	h := newHarness(t)
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{"all": {values: map[string]string{"k": "v"}}})})
	if err := reconcile(context.Background(), "gone"); err != nil {
		t.Errorf("reconcile: %v", err)
	}
	if h.writes() != 0 {
		t.Errorf("writes = %d, want 0", h.writes())
	}
}

func testMetadataAlreadyPresent(t *testing.T, d metadataDomainCase) {
	h := newHarness(t, edgeNode(d, map[string]string{"environment": "production"}))
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{
		"edge": {match: edgeSelector, values: map[string]string{"environment": "production"}},
	})})

	writes, got := h.converge(reconcile, "edge-1")

	if writes != 1 {
		t.Errorf("writes = %d, want 1", writes)
	}
	if c := condition(got, d.conditionType); c == nil || c.Reason != "Applied" {
		t.Errorf("condition = %+v, want Applied", c)
	}
}

func testMetadataPreservesUnrelated(t *testing.T, d metadataDomainCase) {
	n := edgeNode(d, map[string]string{"keep": "me", "environment": "staging"})
	if d.name == "labels" {
		n.Annotations = map[string]string{"other": "domain"}
	} else {
		n.Labels = map[string]string{"other": "domain"}
	}
	h := newHarness(t, n)
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{
		"edge": {match: edgeSelector, values: map[string]string{"environment": "production", "rack": "r42"}},
	})})

	_, got := h.converge(reconcile, "edge-1")

	want := map[string]string{"keep": "me", "environment": "production", "rack": "r42"}
	if !reflect.DeepEqual(d.get(got), want) {
		t.Errorf("%s = %v, want %v", d.name, d.get(got), want)
	}
	if !reflect.DeepEqual(d.other(got), map[string]string{"other": "domain"}) {
		t.Errorf("other domain changed: %v", d.other(got))
	}
}

func testMetadataMergesAndSkipsConflicts(t *testing.T, d metadataDomainCase) {
	h := newHarness(t, edgeNode(d, map[string]string{"environment": "original"}))
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{
		"a":     {match: edgeSelector, values: map[string]string{"environment": "production", "rack": "r42", "shared": "same"}},
		"b":     {values: map[string]string{"environment": "staging", "row": "7", "shared": "same"}},
		"other": {match: config.Match{ProviderIDPattern: `^metal://`}, values: map[string]string{"ignored": "true"}},
	})})

	writes, got := h.converge(reconcile, "edge-1")

	if writes != 3 {
		t.Errorf("writes = %d, want 3", writes)
	}
	want := map[string]string{"environment": "original", "rack": "r42", "row": "7", "shared": "same"}
	if !reflect.DeepEqual(d.get(got), want) {
		t.Errorf("%s = %v, want %v", d.name, d.get(got), want)
	}
	c := condition(got, d.conditionType)
	if c == nil || c.Reason != "Applied" || c.Message != "Applied from matching rule(s): a, b." {
		t.Errorf("condition = %+v", c)
	}
}

func testMetadataOnlyConflicts(t *testing.T, d metadataDomainCase) {
	h := newHarness(t, edgeNode(d, nil))
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{
		"a": {values: map[string]string{"environment": "production"}},
		"b": {values: map[string]string{"environment": "staging"}},
	})})

	writes, got := h.converge(reconcile, "edge-1")

	if writes != 1 {
		t.Errorf("writes = %d, want 1", writes)
	}
	if len(d.get(got)) != 0 {
		t.Errorf("%s = %v, want none", d.name, d.get(got))
	}
	if c := condition(got, d.conditionType); c == nil || c.Reason != "Applied" {
		t.Errorf("condition = %+v, want Applied", c)
	}
}

func testMetadataRuleWithoutValues(t *testing.T, d metadataDomainCase) {
	h := newHarness(t, edgeNode(d, nil))
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{"empty": {match: edgeSelector}})})

	writes, got := h.converge(reconcile, "edge-1")

	if writes != 1 || condition(got, d.conditionType) == nil {
		t.Errorf("writes = %d, condition = %+v", writes, condition(got, d.conditionType))
	}
}

func testMetadataNewlyMatchingRule(t *testing.T, d metadataDomainCase) {
	h := newHarness(t, edgeNode(d, nil))
	rules := map[string]metadataRule{"a": {match: edgeSelector, values: map[string]string{"environment": "production"}}}
	source := &staticConfig{d.config(rules)}
	reconcile := d.newReconcile(h, source)
	_, before := h.converge(reconcile, "edge-1")
	transition := condition(before, d.conditionType).LastTransitionTime

	rules["b"] = metadataRule{values: map[string]string{"environment": "production"}}
	source.cfg = d.config(rules)
	writes, got := h.converge(reconcile, "edge-1")

	if writes != 1 {
		t.Errorf("writes = %d, want 1", writes)
	}
	c := condition(got, d.conditionType)
	if c == nil || c.Message != "Applied from matching rule(s): a, b." {
		t.Fatalf("condition = %+v", c)
	}
	if !c.LastTransitionTime.Equal(&transition) {
		t.Errorf("LastTransitionTime changed without a status change")
	}
}

// tamper applies a rule to edge-1 and then changes its value behind the
// controller's back, the way another controller fighting over it would.
func tamper(t *testing.T, d metadataDomainCase) (*harness, func(context.Context, string) error) {
	t.Helper()
	h := newHarness(t, edgeNode(d, nil))
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{
		"edge": {match: edgeSelector, values: map[string]string{"environment": "production", "rack": "r42"}},
	})})
	_, got := h.converge(reconcile, "edge-1")

	d.set(got, map[string]string{"environment": "tampered"})
	if _, err := h.client.CoreV1().Nodes().Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.sync("edge-1")
	return h, reconcile
}

func testMetadataRestoresDrift(t *testing.T, d metadataDomainCase) {
	h, reconcile := tamper(t, d)
	after(h.queue, flapWindow+time.Second)

	writes, got := h.converge(reconcile, "edge-1")

	// Values only: the condition never leaves Applied.
	if writes != 1 {
		t.Errorf("writes = %d, want 1", writes)
	}
	if want := map[string]string{"environment": "production", "rack": "r42"}; !reflect.DeepEqual(d.get(got), want) {
		t.Errorf("%s = %v, want %v", d.name, d.get(got), want)
	}
	if c := condition(got, d.conditionType); c == nil || c.Reason != "Applied" {
		t.Errorf("condition = %+v, want Applied", c)
	}
}

func testMetadataCoolsDownFlaps(t *testing.T, d metadataDomainCase) {
	h, reconcile := tamper(t, d)

	writes, got := h.converge(reconcile, "edge-1")

	// Drifted condition, then nothing until the cooldown has passed.
	if writes != 1 || d.get(got)["environment"] != "tampered" {
		t.Errorf("writes = %d, %s = %v; want 1 write and values left alone", writes, d.name, d.get(got))
	}
	c := condition(got, d.conditionType)
	if c == nil || c.Status != corev1.ConditionFalse || c.Reason != "Drifted" ||
		!strings.HasSuffix(c.Message, " shortly after being applied (missing: rack; changed: environment); restoring them after a 500ms cooldown.") {
		t.Fatalf("condition = %+v, want Drifted", c)
	}
	if strings.Contains(c.Message, "production") || strings.Contains(c.Message, "tampered") {
		t.Errorf("condition message leaks values: %q", c.Message)
	}

	after(h.queue, minCooldown)
	writes, got = h.converge(reconcile, "edge-1")

	// Values, Applied condition.
	if writes != 2 {
		t.Errorf("writes = %d, want 2", writes)
	}
	if want := map[string]string{"environment": "production", "rack": "r42"}; !reflect.DeepEqual(d.get(got), want) {
		t.Errorf("%s = %v, want %v", d.name, d.get(got), want)
	}
	if c := condition(got, d.conditionType); c == nil || c.Reason != "Applied" {
		t.Errorf("condition = %+v, want Applied", c)
	}
}

func testMetadataRuleChangeIsNoFlap(t *testing.T, d metadataDomainCase) {
	h := newHarness(t, edgeNode(d, nil))
	source := &staticConfig{d.config(map[string]metadataRule{
		"edge": {match: edgeSelector, values: map[string]string{"environment": "production"}},
	})}
	reconcile := d.newReconcile(h, source)
	h.converge(reconcile, "edge-1")

	source.cfg = d.config(map[string]metadataRule{
		"edge": {match: edgeSelector, values: map[string]string{"environment": "staging"}},
	})
	if err := reconcile(context.Background(), "edge-1"); err != nil {
		t.Fatal(err)
	}
	if c := condition(h.sync("edge-1"), d.conditionType); c == nil || c.Reason != "Pending" {
		t.Fatalf("condition = %+v, want Pending", c)
	}
	writes, got := h.converge(reconcile, "edge-1")

	// Values, Applied condition, without waiting for a cooldown.
	if writes != 2 || d.get(got)["environment"] != "staging" {
		t.Errorf("writes = %d, %s = %v; want 2 writes and environment=staging", writes, d.name, d.get(got))
	}
	if c := condition(got, d.conditionType); c == nil || c.Reason != "Applied" {
		t.Errorf("condition = %+v, want Applied", c)
	}
}

func testMetadataAlreadyPending(t *testing.T, d metadataDomainCase) {
	n := edgeNode(d, nil)
	n.Status.Conditions = []corev1.NodeCondition{pendingCondition(d.conditionType, []string{"edge"})}
	h := newHarness(t, n)
	reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{
		"edge": {match: edgeSelector, values: map[string]string{"environment": "production"}},
	})})

	writes, got := h.converge(reconcile, "edge-1")

	if writes != 2 {
		t.Errorf("writes = %d, want 2", writes)
	}
	if d.get(got)["environment"] != "production" {
		t.Errorf("%s = %v", d.name, d.get(got))
	}
}

func testMetadataRemovedWhilePending(t *testing.T, d metadataDomainCase) {
	h := newHarness(t, edgeNode(d, nil))
	source := &staticConfig{d.config(map[string]metadataRule{
		"edge": {match: edgeSelector, values: map[string]string{"environment": "production"}},
	})}
	reconcile := d.newReconcile(h, source)
	if err := reconcile(context.Background(), "edge-1"); err != nil {
		t.Fatal(err)
	}
	if c := condition(h.sync("edge-1"), d.conditionType); c == nil || c.Reason != "Pending" {
		t.Fatalf("condition = %+v, want Pending", c)
	}

	source.cfg = config.Config{}
	_, got := h.converge(reconcile, "edge-1")

	if condition(got, d.conditionType) != nil {
		t.Error("condition still present")
	}
	if len(d.get(got)) != 0 {
		t.Errorf("%s = %v, want none", d.name, d.get(got))
	}
}

func testMetadataWriteErrors(t *testing.T, d metadataDomainCase) {
	for name, tc := range map[string]struct {
		err     error
		wantErr bool
	}{
		"conflict writing values is swallowed": {err: errConflict},
		"error writing values is returned":     {err: errInternal, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, edgeNode(d, nil))
			h.failUpdates("", tc.err)
			reconcile := d.newReconcile(h, &staticConfig{d.config(map[string]metadataRule{
				"edge": {match: edgeSelector, values: map[string]string{"environment": "production"}},
			})})
			if err := reconcile(context.Background(), "edge-1"); err != nil {
				t.Fatalf("setting Pending: %v", err)
			}
			h.sync("edge-1")

			err := reconcile(context.Background(), "edge-1")

			if !tc.wantErr {
				if err != nil {
					t.Errorf("error = %v, want nil", err)
				}
				return
			}
			if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), `updating node "edge-1" `+d.name) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

func TestResolveValuesMoreCases(t *testing.T) {
	values := func(r config.AnnotationRule) map[string]string { return r.Annotations }

	t.Run("one dissenting rule out of three drops the key", func(t *testing.T) {
		rules := map[string]config.AnnotationRule{
			"a": {Annotations: map[string]string{"env": "prod"}},
			"b": {Annotations: map[string]string{"env": "prod"}},
			"c": {Annotations: map[string]string{"env": "staging"}},
		}
		got := resolveValues("annotations", "node-1", []string{"a", "b", "c"}, rules, values)
		if len(got) != 0 {
			t.Errorf("got %+v, want empty", got)
		}
	})

	t.Run("empty value is a value like any other", func(t *testing.T) {
		rules := map[string]config.AnnotationRule{
			"a": {Annotations: map[string]string{"flag": ""}},
			"b": {Annotations: map[string]string{"flag": "set"}},
			"c": {Annotations: map[string]string{"empty": ""}},
		}
		got := resolveValues("annotations", "node-1", []string{"a", "b", "c"}, rules, values)
		if want := map[string]string{"empty": ""}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("only matched rules contribute", func(t *testing.T) {
		rules := map[string]config.AnnotationRule{
			"a": {Annotations: map[string]string{"env": "prod"}},
			"b": {Annotations: map[string]string{"env": "staging"}},
		}
		got := resolveValues("annotations", "node-1", []string{"a"}, rules, values)
		if want := map[string]string{"env": "prod"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("no matched rules", func(t *testing.T) {
		got := resolveValues("annotations", "node-1", nil, map[string]config.AnnotationRule{}, values)
		if got == nil || len(got) != 0 {
			t.Errorf("got %#v, want an empty non-nil map", got)
		}
	})
}

func TestApplyOverwriteDoesNotMutateExisting(t *testing.T) {
	existing := map[string]string{"a": "1"}
	got, changed := applyOverwrite(existing, map[string]string{"a": "2", "b": "3"})
	if !changed {
		t.Fatal("expected a change")
	}
	if !reflect.DeepEqual(existing, map[string]string{"a": "1"}) {
		t.Errorf("existing map was mutated: %v", existing)
	}
	if !reflect.DeepEqual(got, map[string]string{"a": "2", "b": "3"}) {
		t.Errorf("got %v", got)
	}
}

func TestApplyOverwriteEmptyValueOnMissingKey(t *testing.T) {
	got, changed := applyOverwrite(map[string]string{}, map[string]string{"flag": ""})
	if !changed || !reflect.DeepEqual(got, map[string]string{"flag": ""}) {
		t.Errorf("got %v, changed = %v; an empty value on a missing key must still be written", got, changed)
	}
}
