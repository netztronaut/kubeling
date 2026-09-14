package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/steigr/kubeling/pkg/config"
)

func TestMatchOnboardingRule(t *testing.T) {
	rules := map[string]config.OnboardingRule{
		"managed": {ProviderIDPattern: `^custom://managed-`, Label: "kubeling.io/onboarded", LabelValue: "true"},
		"edge":    {ProviderIDPattern: `^custom://edge-`, Label: "kubeling.io/onboarded", LabelValue: "true"},
	}

	t.Run("empty providerID never matches", func(t *testing.T) {
		_, _, matched := matchOnboardingRule(rules, "")
		if matched {
			t.Fatalf("expected no match for empty providerID")
		}
	})

	t.Run("no rule matches an unrelated providerID", func(t *testing.T) {
		_, _, matched := matchOnboardingRule(rules, "custom://other-node")
		if matched {
			t.Fatalf("expected no match")
		}
	})

	t.Run("matches the rule whose pattern applies", func(t *testing.T) {
		rule, id, matched := matchOnboardingRule(rules, "custom://edge-node-1")
		if !matched {
			t.Fatalf("expected a match")
		}
		if id != "edge" {
			t.Errorf("id = %q, want %q", id, "edge")
		}
		if rule.LabelValue != "true" {
			t.Errorf("rule.LabelValue = %q, want %q", rule.LabelValue, "true")
		}
	})

	t.Run("invalid pattern is skipped rather than matched", func(t *testing.T) {
		bad := map[string]config.OnboardingRule{
			"broken": {ProviderIDPattern: `(`, Label: "x", LabelValue: "y"},
		}
		_, _, matched := matchOnboardingRule(bad, "custom://anything")
		if matched {
			t.Fatalf("expected an invalid pattern to never match")
		}
	})
}

func TestConditionUpToDate(t *testing.T) {
	pending := pendingCondition()
	onboarded := onboardedCondition("edge")

	t.Run("absent condition is not up to date", func(t *testing.T) {
		node := &corev1.Node{}
		if conditionUpToDate(node, pending) {
			t.Fatalf("expected not up to date")
		}
	})

	t.Run("matching condition is up to date", func(t *testing.T) {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{pending}}}
		if !conditionUpToDate(node, pending) {
			t.Fatalf("expected up to date")
		}
	})

	t.Run("differing reason is not up to date", func(t *testing.T) {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{pending}}}
		if conditionUpToDate(node, onboarded) {
			t.Fatalf("expected not up to date")
		}
	})
}

func TestSetCondition(t *testing.T) {
	t.Run("appends when absent", func(t *testing.T) {
		node := &corev1.Node{}
		setCondition(node, pendingCondition())
		if len(node.Status.Conditions) != 1 {
			t.Fatalf("got %d conditions, want 1", len(node.Status.Conditions))
		}
		if node.Status.Conditions[0].Reason != "Pending" {
			t.Errorf("reason = %q, want %q", node.Status.Conditions[0].Reason, "Pending")
		}
	})

	t.Run("replaces in place and preserves other conditions", func(t *testing.T) {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			pendingCondition(),
		}}}
		setCondition(node, onboardedCondition("edge"))

		if len(node.Status.Conditions) != 2 {
			t.Fatalf("got %d conditions, want 2", len(node.Status.Conditions))
		}
		if node.Status.Conditions[0].Type != corev1.NodeReady {
			t.Errorf("unrelated condition was disturbed: %+v", node.Status.Conditions[0])
		}
		if node.Status.Conditions[1].Reason != "Onboarded" {
			t.Errorf("reason = %q, want %q", node.Status.Conditions[1].Reason, "Onboarded")
		}
	})

	t.Run("preserves LastTransitionTime when status is unchanged", func(t *testing.T) {
		original := pendingCondition()
		original.LastTransitionTime = metav1.NewTime(metav1.Now().Add(-1))
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{original}}}

		// Re-applying the same Pending condition (e.g. a re-enqueue with no
		// rule change) must not bump LastTransitionTime.
		setCondition(node, pendingCondition())
		if !node.Status.Conditions[0].LastTransitionTime.Equal(&original.LastTransitionTime) {
			t.Errorf("LastTransitionTime changed even though status did not: got %v, want %v",
				node.Status.Conditions[0].LastTransitionTime, original.LastTransitionTime)
		}
	})

	t.Run("bumps LastTransitionTime when status changes", func(t *testing.T) {
		original := pendingCondition()
		original.LastTransitionTime = metav1.NewTime(metav1.Now().Add(-1))
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{original}}}

		setCondition(node, onboardedCondition("edge"))
		if node.Status.Conditions[0].LastTransitionTime.Equal(&original.LastTransitionTime) {
			t.Errorf("expected LastTransitionTime to change when status changes")
		}
	})
}
