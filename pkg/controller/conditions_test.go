package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConditionUpToDate(t *testing.T) {
	pending := pendingCondition(LabeledConditionType, []string{"a"})
	applied := appliedCondition(LabeledConditionType, []string{"a"})

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

	t.Run("differing status/reason is not up to date", func(t *testing.T) {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{pending}}}
		if conditionUpToDate(node, applied) {
			t.Fatalf("expected not up to date")
		}
	})

	t.Run("differing matched rule set changes the message and is not up to date", func(t *testing.T) {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{pending}}}
		if conditionUpToDate(node, pendingCondition(LabeledConditionType, []string{"a", "b"})) {
			t.Fatalf("expected not up to date")
		}
	})
}

func TestSetCondition(t *testing.T) {
	t.Run("appends when absent", func(t *testing.T) {
		node := &corev1.Node{}
		pending := pendingCondition(LabeledConditionType, []string{"a"})
		setCondition(node, pending)
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
			pendingCondition(LabeledConditionType, []string{"a"}),
		}}}
		setCondition(node, appliedCondition(LabeledConditionType, []string{"a"}))

		if len(node.Status.Conditions) != 2 {
			t.Fatalf("got %d conditions, want 2", len(node.Status.Conditions))
		}
		if node.Status.Conditions[0].Type != corev1.NodeReady {
			t.Errorf("unrelated condition was disturbed: %+v", node.Status.Conditions[0])
		}
		if node.Status.Conditions[1].Reason != "Applied" {
			t.Errorf("reason = %q, want %q", node.Status.Conditions[1].Reason, "Applied")
		}
	})

	t.Run("preserves LastTransitionTime when status is unchanged", func(t *testing.T) {
		original := pendingCondition(LabeledConditionType, []string{"a"})
		original.LastTransitionTime = metav1.NewTime(metav1.Now().Add(-1))
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{original}}}

		setCondition(node, pendingCondition(LabeledConditionType, []string{"a"}))
		if !node.Status.Conditions[0].LastTransitionTime.Equal(&original.LastTransitionTime) {
			t.Errorf("LastTransitionTime changed even though status did not: got %v, want %v",
				node.Status.Conditions[0].LastTransitionTime, original.LastTransitionTime)
		}
	})

	t.Run("bumps LastTransitionTime when status changes", func(t *testing.T) {
		original := pendingCondition(LabeledConditionType, []string{"a"})
		original.LastTransitionTime = metav1.NewTime(metav1.Now().Add(-1))
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{original}}}

		setCondition(node, appliedCondition(LabeledConditionType, []string{"a"}))
		if node.Status.Conditions[0].LastTransitionTime.Equal(&original.LastTransitionTime) {
			t.Errorf("expected LastTransitionTime to change when status changes")
		}
	})
}

func TestRemoveCondition(t *testing.T) {
	t.Run("removes the matching condition and leaves others", func(t *testing.T) {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			pendingCondition(LabeledConditionType, []string{"a"}),
		}}}
		removeCondition(node, LabeledConditionType)

		if len(node.Status.Conditions) != 1 {
			t.Fatalf("got %d conditions, want 1", len(node.Status.Conditions))
		}
		if node.Status.Conditions[0].Type != corev1.NodeReady {
			t.Errorf("wrong condition removed")
		}
	})

	t.Run("no-op when absent", func(t *testing.T) {
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}}}
		removeCondition(node, LabeledConditionType)
		if len(node.Status.Conditions) != 1 {
			t.Fatalf("got %d conditions, want 1", len(node.Status.Conditions))
		}
	})
}

func TestHasCondition(t *testing.T) {
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		pendingCondition(LabeledConditionType, []string{"a"}),
	}}}
	if !hasCondition(node, LabeledConditionType) {
		t.Errorf("expected LabeledConditionType to be present")
	}
	if hasCondition(node, AnnotatedConditionType) {
		t.Errorf("expected AnnotatedConditionType to be absent")
	}
}
