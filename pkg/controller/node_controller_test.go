package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNodeControllerReconcile(t *testing.T) {
	tests := []struct {
		name           string
		node           *corev1.Node
		wantWrites     int
		wantProviderID string
	}{
		{
			name: "initializes a new node",
			node: func() *corev1.Node {
				n := node("worker-1", nil)
				n.Spec.Taints = []corev1.Taint{
					{Key: UninitializedTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule},
					{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule},
				}
				return n
			}(),
			wantWrites:     1,
			wantProviderID: "custom://worker-1",
		},
		{
			name: "keeps an existing providerID",
			node: func() *corev1.Node {
				n := node("worker-1", nil)
				n.Spec.ProviderID = "metal://rack-1/worker-1"
				n.Spec.Taints = []corev1.Taint{{Key: UninitializedTaintKey, Effect: corev1.TaintEffectNoSchedule}}
				return n
			}(),
			wantWrites:     1,
			wantProviderID: "metal://rack-1/worker-1",
		},
		{
			name: "leaves an initialized node alone",
			node: func() *corev1.Node {
				n := node("worker-1", nil)
				n.Spec.ProviderID = "custom://worker-1"
				return n
			}(),
			wantWrites:     0,
			wantProviderID: "custom://worker-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.node)
			c, err := NewNodeController(h.client, h.nodes, "custom")
			if err != nil {
				t.Fatal(err)
			}

			writes, got := h.converge(c.reconcile, tt.node.Name)

			if writes != tt.wantWrites {
				t.Errorf("writes = %d, want %d", writes, tt.wantWrites)
			}
			if got.Spec.ProviderID != tt.wantProviderID {
				t.Errorf("providerID = %q, want %q", got.Spec.ProviderID, tt.wantProviderID)
			}
			if hasUninitializedTaint(got) {
				t.Errorf("uninitialized taint still present")
			}
			for _, taint := range tt.node.Spec.Taints {
				if taint.Key == UninitializedTaintKey {
					continue
				}
				found := false
				for _, g := range got.Spec.Taints {
					found = found || g.Key == taint.Key
				}
				if !found {
					t.Errorf("unrelated taint %q was removed", taint.Key)
				}
			}
		})
	}
}

func TestNodeControllerIgnoresDeletedNode(t *testing.T) {
	h := newHarness(t)
	c, err := NewNodeController(h.client, h.nodes, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background(), "gone"); err != nil {
		t.Errorf("reconcile of a missing node: %v", err)
	}
}
