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

func TestConditionBuilders(t *testing.T) {
	pending := pendingCondition(AnnotatedConditionType, []string{"a", "b"})
	want := corev1.NodeCondition{
		Type:    AnnotatedConditionType,
		Status:  corev1.ConditionFalse,
		Reason:  "Pending",
		Message: "Waiting to apply matching rule(s): a, b.",
	}
	if pending != want {
		t.Errorf("pendingCondition = %+v, want %+v", pending, want)
	}

	applied := appliedCondition(ExternalIPsAppliedConditionType, []string{"edge"})
	want = corev1.NodeCondition{
		Type:    ExternalIPsAppliedConditionType,
		Status:  corev1.ConditionTrue,
		Reason:  "Applied",
		Message: "Applied from matching rule(s): edge.",
	}
	if applied != want {
		t.Errorf("appliedCondition = %+v, want %+v", applied, want)
	}
}

func TestConditionUpToDateIgnoresOtherTypesAndTimestamps(t *testing.T) {
	applied := appliedCondition(LabeledConditionType, []string{"a"})
	stamped := applied
	stamped.LastHeartbeatTime = metav1.NewTime(metav1.Now().Add(-time.Hour))
	stamped.LastTransitionTime = stamped.LastHeartbeatTime
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "Applied", Message: applied.Message},
		appliedCondition(AnnotatedConditionType, []string{"a"}),
		stamped,
	}}}

	if !conditionUpToDate(node, applied) {
		t.Error("expected up to date despite other condition types and older timestamps")
	}
}

func TestSetConditionStampsHeartbeat(t *testing.T) {
	old := metav1.NewTime(metav1.Now().Add(-time.Hour))
	original := appliedCondition(LabeledConditionType, []string{"a"})
	original.LastHeartbeatTime = old
	original.LastTransitionTime = old
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{original}}}

	setCondition(node, appliedCondition(LabeledConditionType, []string{"a", "b"}))

	got := node.Status.Conditions[0]
	if !got.LastHeartbeatTime.After(old.Time) {
		t.Errorf("LastHeartbeatTime was not refreshed: %v", got.LastHeartbeatTime)
	}
	if !got.LastTransitionTime.Equal(&old) {
		t.Errorf("LastTransitionTime changed without a status change: %v", got.LastTransitionTime)
	}
	if got.Message != "Applied from matching rule(s): a, b." {
		t.Errorf("message = %q", got.Message)
	}
}

func TestSetConditionStampsNewCondition(t *testing.T) {
	node := &corev1.Node{}
	setCondition(node, pendingCondition(LabeledConditionType, []string{"a"}))

	got := node.Status.Conditions[0]
	if got.LastHeartbeatTime.IsZero() || got.LastTransitionTime.IsZero() {
		t.Errorf("timestamps not set on a new condition: %+v", got)
	}
}

func TestRemoveConditionFromMiddle(t *testing.T) {
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady},
		{Type: LabeledConditionType},
		{Type: corev1.NodeMemoryPressure},
	}}}
	removeCondition(node, LabeledConditionType)

	types := make([]corev1.NodeConditionType, 0, len(node.Status.Conditions))
	for _, c := range node.Status.Conditions {
		types = append(types, c.Type)
	}
	if want := []corev1.NodeConditionType{corev1.NodeReady, corev1.NodeMemoryPressure}; !reflect.DeepEqual(types, want) {
		t.Errorf("conditions = %v, want %v", types, want)
	}
}

func TestEnsureCondition(t *testing.T) {
	applied := appliedCondition(LabeledConditionType, []string{"a"})

	t.Run("writes a missing condition", func(t *testing.T) {
		h := newHarness(t, node("n", nil))
		cached, _ := h.nodes.Lister().Get("n")
		if err := ensureCondition(context.Background(), h.client, cached, applied); err != nil {
			t.Fatal(err)
		}
		got := h.sync("n")
		if h.writes() != 1 || !conditionUpToDate(got, applied) {
			t.Errorf("writes = %d, conditions = %+v", h.writes(), got.Status.Conditions)
		}
		if len(cached.Status.Conditions) != 0 {
			t.Error("ensureCondition mutated the cached Node")
		}
	})

	t.Run("skips an up-to-date condition", func(t *testing.T) {
		n := node("n", nil)
		n.Status.Conditions = []corev1.NodeCondition{applied}
		h := newHarness(t, n)
		cached, _ := h.nodes.Lister().Get("n")
		if err := ensureCondition(context.Background(), h.client, cached, applied); err != nil {
			t.Fatal(err)
		}
		if h.writes() != 0 {
			t.Errorf("writes = %d, want 0", h.writes())
		}
	})

	t.Run("swallows conflicts", func(t *testing.T) {
		h := newHarness(t, node("n", nil))
		h.failUpdates("status", errConflict)
		cached, _ := h.nodes.Lister().Get("n")
		if err := ensureCondition(context.Background(), h.client, cached, applied); err != nil {
			t.Errorf("error = %v, want nil", err)
		}
	})

	t.Run("returns other errors", func(t *testing.T) {
		h := newHarness(t, node("n", nil))
		h.failUpdates("status", errInternal)
		cached, _ := h.nodes.Lister().Get("n")
		err := ensureCondition(context.Background(), h.client, cached, applied)
		if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), `updating node "n" condition kubeling.io/Labeled`) {
			t.Errorf("error = %v", err)
		}
	})
}

func TestEnsureConditionAbsent(t *testing.T) {
	withCondition := func() *corev1.Node {
		n := node("n", nil)
		n.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			appliedCondition(LabeledConditionType, []string{"a"}),
		}
		return n
	}

	t.Run("removes a present condition", func(t *testing.T) {
		h := newHarness(t, withCondition())
		cached, _ := h.nodes.Lister().Get("n")
		if err := ensureConditionAbsent(context.Background(), h.client, cached, LabeledConditionType); err != nil {
			t.Fatal(err)
		}
		got := h.sync("n")
		if h.writes() != 1 || hasCondition(got, LabeledConditionType) || !hasCondition(got, corev1.NodeReady) {
			t.Errorf("writes = %d, conditions = %+v", h.writes(), got.Status.Conditions)
		}
		if !hasCondition(cached, LabeledConditionType) {
			t.Error("ensureConditionAbsent mutated the cached Node")
		}
	})

	t.Run("skips an absent condition", func(t *testing.T) {
		h := newHarness(t, node("n", nil))
		cached, _ := h.nodes.Lister().Get("n")
		if err := ensureConditionAbsent(context.Background(), h.client, cached, LabeledConditionType); err != nil {
			t.Fatal(err)
		}
		if h.writes() != 0 {
			t.Errorf("writes = %d, want 0", h.writes())
		}
	})

	t.Run("swallows conflicts", func(t *testing.T) {
		h := newHarness(t, withCondition())
		h.failUpdates("status", errConflict)
		cached, _ := h.nodes.Lister().Get("n")
		if err := ensureConditionAbsent(context.Background(), h.client, cached, LabeledConditionType); err != nil {
			t.Errorf("error = %v, want nil", err)
		}
	})

	t.Run("returns other errors", func(t *testing.T) {
		h := newHarness(t, withCondition())
		h.failUpdates("status", errInternal)
		cached, _ := h.nodes.Lister().Get("n")
		err := ensureConditionAbsent(context.Background(), h.client, cached, LabeledConditionType)
		if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), `clearing node "n" condition kubeling.io/Labeled`) {
			t.Errorf("error = %v", err)
		}
	})
}
