package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types this project maintains on Node status, one per
// reconciled domain. Each is present on a Node only while at least one
// rule for that domain matches it; a Node that no rule ever matches for a
// given domain carries no condition of that type at all.
const (
	LabeledConditionType            corev1.NodeConditionType = "kubeling.io/Labeled"
	AnnotatedConditionType          corev1.NodeConditionType = "kubeling.io/Annotated"
	ExternalIPsAppliedConditionType corev1.NodeConditionType = "kubeling.io/ExternalIPsApplied"
)

func pendingCondition(t corev1.NodeConditionType, matched []string) corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:    t,
		Status:  corev1.ConditionFalse,
		Reason:  "Pending",
		Message: fmt.Sprintf("Waiting to apply matching rule(s): %s.", strings.Join(matched, ", ")),
	}
}

func appliedCondition(t corev1.NodeConditionType, matched []string) corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:    t,
		Status:  corev1.ConditionTrue,
		Reason:  "Applied",
		Message: fmt.Sprintf("Applied from matching rule(s): %s.", strings.Join(matched, ", ")),
	}
}

// conditionUpToDate reports whether node already carries desired's Type,
// Status, Reason and Message (timestamps are ignored for this comparison;
// setCondition manages those itself).
func conditionUpToDate(node *corev1.Node, desired corev1.NodeCondition) bool {
	for _, c := range node.Status.Conditions {
		if c.Type != desired.Type {
			continue
		}
		return c.Status == desired.Status && c.Reason == desired.Reason && c.Message == desired.Message
	}
	return false
}

// setCondition replaces node's condition of desired.Type with desired (or
// appends it if absent), preserving LastTransitionTime when the status
// hasn't changed and stamping both timestamps to now otherwise.
func setCondition(node *corev1.Node, desired corev1.NodeCondition) {
	now := metav1.Now()
	desired.LastHeartbeatTime = now
	desired.LastTransitionTime = now

	for i, c := range node.Status.Conditions {
		if c.Type != desired.Type {
			continue
		}
		if c.Status == desired.Status {
			desired.LastTransitionTime = c.LastTransitionTime
		}
		node.Status.Conditions[i] = desired
		return
	}
	node.Status.Conditions = append(node.Status.Conditions, desired)
}

// removeCondition deletes node's condition of type t, if present.
func removeCondition(node *corev1.Node, t corev1.NodeConditionType) {
	for i, c := range node.Status.Conditions {
		if c.Type == t {
			node.Status.Conditions = append(node.Status.Conditions[:i], node.Status.Conditions[i+1:]...)
			return
		}
	}
}

func hasCondition(node *corev1.Node, t corev1.NodeConditionType) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == t {
			return true
		}
	}
	return false
}
