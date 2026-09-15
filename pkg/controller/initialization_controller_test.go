package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/netztronaut/kubeling/pkg/config"
)

func TestInitializationControllerReconcile(t *testing.T) {
	uninitialized := func(name, providerID string, labels map[string]string) *corev1.Node {
		n := node(name, labels)
		n.Spec.ProviderID = providerID
		n.Spec.Taints = []corev1.Taint{
			{Key: UninitializedTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule},
			{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule},
		}
		return n
	}

	tests := []struct {
		name           string
		node           *corev1.Node
		rules          map[string]config.InitializationRule
		wantWrites     int
		wantProviderID string
		wantTainted    bool
	}{
		{
			name:        "no rules leave the node alone",
			node:        uninitialized("worker-1", "", nil),
			wantWrites:  0,
			wantTainted: true,
		},
		{
			name: "a non-matching rule leaves the node alone",
			node: uninitialized("worker-1", "hcloud://123", nil),
			rules: map[string]config.InitializationRule{
				"metal": {Match: config.Match{ProviderIDPattern: `^metal://`}},
			},
			wantWrites:     0,
			wantProviderID: "hcloud://123",
			wantTainted:    true,
		},
		{
			name: "a matching providerIDPattern removes the taint and keeps the providerID",
			node: uninitialized("worker-1", "metal://rack-1/worker-1", nil),
			rules: map[string]config.InitializationRule{
				"metal": {Match: config.Match{ProviderIDPattern: `^metal://`}, ProviderIDScheme: "custom"},
			},
			wantWrites:     1,
			wantProviderID: "metal://rack-1/worker-1",
		},
		{
			name: "a providerIDScheme stamps a missing providerID",
			node: uninitialized("worker-1", "", map[string]string{"example.com/bare-metal": "true"}),
			rules: map[string]config.InitializationRule{
				"bare-metal": {
					Match:            config.Match{NodeSelector: map[string]string{"example.com/bare-metal": "true"}},
					ProviderIDScheme: "custom",
				},
			},
			wantWrites:     1,
			wantProviderID: "custom://worker-1",
		},
		{
			name: "without a providerIDScheme only the taint is removed",
			node: uninitialized("worker-1", "", nil),
			rules: map[string]config.InitializationRule{
				"no-provider-id": {Match: config.Match{ProviderIDPattern: `^$`}},
			},
			wantWrites: 1,
		},
		{
			name: "matching rules agreeing on a providerIDScheme stamp it",
			node: uninitialized("worker-1", "", nil),
			rules: map[string]config.InitializationRule{
				"a": {ProviderIDScheme: "custom"},
				"b": {ProviderIDScheme: "custom"},
				"c": {},
			},
			wantWrites:     1,
			wantProviderID: "custom://worker-1",
		},
		{
			name: "conflicting providerIDSchemes leave the node alone",
			node: uninitialized("worker-1", "", nil),
			rules: map[string]config.InitializationRule{
				"a": {ProviderIDScheme: "custom"},
				"b": {ProviderIDScheme: "metal"},
			},
			wantWrites:  0,
			wantTainted: true,
		},
		{
			name: "stamps a providerID onto an already untainted node",
			node: node("worker-1", nil),
			rules: map[string]config.InitializationRule{
				"all": {ProviderIDScheme: "custom"},
			},
			wantWrites:     1,
			wantProviderID: "custom://worker-1",
		},
		{
			name: "leaves an initialized node alone",
			node: func() *corev1.Node {
				n := node("worker-1", nil)
				n.Spec.ProviderID = "custom://worker-1"
				return n
			}(),
			rules: map[string]config.InitializationRule{
				"all": {ProviderIDScheme: "custom"},
			},
			wantWrites:     0,
			wantProviderID: "custom://worker-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.node)
			c, err := NewInitializationController(h.client, h.nodes, &staticConfig{cfg: config.Config{Initialization: tt.rules}})
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
			if tainted := hasUninitializedTaint(got); tainted != tt.wantTainted {
				t.Errorf("uninitialized taint present = %v, want %v", tainted, tt.wantTainted)
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

func TestInitializationControllerIgnoresDeletedNode(t *testing.T) {
	h := newHarness(t)
	c, err := NewInitializationController(h.client, h.nodes, &staticConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background(), "gone"); err != nil {
		t.Errorf("reconcile of a missing node: %v", err)
	}
}

// initializeAll is a ConfigSource with one initialization rule matching
// every Node and stamping providerIDs with scheme.
func initializeAll(scheme string) *staticConfig {
	return &staticConfig{cfg: config.Config{Initialization: map[string]config.InitializationRule{
		"all": {ProviderIDScheme: scheme},
	}}}
}

func TestInitializationControllerProviderIDScheme(t *testing.T) {
	h := newHarness(t, node("worker-1", nil))
	c, err := NewInitializationController(h.client, h.nodes, initializeAll("baremetal"))
	if err != nil {
		t.Fatal(err)
	}

	_, got := h.converge(c.reconcile, "worker-1")

	if got.Spec.ProviderID != "baremetal://worker-1" {
		t.Errorf("providerID = %q, want %q", got.Spec.ProviderID, "baremetal://worker-1")
	}
}

func TestInitializationControllerPreservesOtherFields(t *testing.T) {
	n := node("worker-1", map[string]string{"zone": "edge"})
	n.Annotations = map[string]string{"example.com/rack": "r42"}
	n.Spec.PodCIDR = "10.244.1.0/24"
	n.Spec.Taints = []corev1.Taint{
		{Key: UninitializedTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		{Key: "example.com/dedicated", Value: "gpu", Effect: corev1.TaintEffectNoExecute},
	}
	h := newHarness(t, n)
	c, err := NewInitializationController(h.client, h.nodes, initializeAll("custom"))
	if err != nil {
		t.Fatal(err)
	}

	_, got := h.converge(c.reconcile, "worker-1")

	if got.Labels["zone"] != "edge" || got.Annotations["example.com/rack"] != "r42" || got.Spec.PodCIDR != "10.244.1.0/24" {
		t.Errorf("unrelated fields changed: %+v", got)
	}
	want := []corev1.Taint{{Key: "example.com/dedicated", Value: "gpu", Effect: corev1.TaintEffectNoExecute}}
	if !reflect.DeepEqual(got.Spec.Taints, want) {
		t.Errorf("taints = %+v, want %+v", got.Spec.Taints, want)
	}
}

func TestInitializationControllerDoesNotMutateCache(t *testing.T) {
	n := node("worker-1", nil)
	n.Spec.Taints = []corev1.Taint{{Key: UninitializedTaintKey, Effect: corev1.TaintEffectNoSchedule}}
	h := newHarness(t, n)
	c, err := NewInitializationController(h.client, h.nodes, initializeAll("custom"))
	if err != nil {
		t.Fatal(err)
	}

	if err := c.reconcile(context.Background(), "worker-1"); err != nil {
		t.Fatal(err)
	}

	cached, err := h.nodes.Lister().Get("worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if cached.Spec.ProviderID != "" || !hasUninitializedTaint(cached) {
		t.Errorf("reconcile mutated the informer cache: %+v", cached.Spec)
	}
}

func TestInitializationControllerUpdateErrors(t *testing.T) {
	uninitialized := func() *corev1.Node {
		n := node("worker-1", nil)
		n.Spec.Taints = []corev1.Taint{{Key: UninitializedTaintKey, Effect: corev1.TaintEffectNoSchedule}}
		return n
	}

	t.Run("conflict is swallowed", func(t *testing.T) {
		h := newHarness(t, uninitialized())
		h.failUpdates("", errConflict)
		c, err := NewInitializationController(h.client, h.nodes, initializeAll("custom"))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.reconcile(context.Background(), "worker-1"); err != nil {
			t.Errorf("error = %v, want nil", err)
		}
	})

	t.Run("other errors are returned for a retry", func(t *testing.T) {
		h := newHarness(t, uninitialized())
		h.failUpdates("", errInternal)
		c, err := NewInitializationController(h.client, h.nodes, initializeAll("custom"))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.reconcile(context.Background(), "worker-1"); !apierrors.IsInternalError(err) {
			t.Errorf("error = %v, want an internal error", err)
		}
	})
}

func TestHasUninitializedTaint(t *testing.T) {
	tests := []struct {
		name   string
		taints []corev1.Taint
		want   bool
	}{
		{name: "no taints"},
		{name: "other taints", taints: []corev1.Taint{{Key: "example.com/other"}}},
		{name: "uninitialized taint", taints: []corev1.Taint{{Key: "example.com/other"}, {Key: UninitializedTaintKey}}, want: true},
		{name: "key prefix is not enough", taints: []corev1.Taint{{Key: UninitializedTaintKey + "-not"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &corev1.Node{Spec: corev1.NodeSpec{Taints: tt.taints}}
			if got := hasUninitializedTaint(n); got != tt.want {
				t.Errorf("hasUninitializedTaint() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveTaint(t *testing.T) {
	tests := []struct {
		name   string
		taints []corev1.Taint
		want   []corev1.Taint
	}{
		{name: "nil", taints: nil, want: []corev1.Taint{}},
		{
			name:   "absent key keeps everything",
			taints: []corev1.Taint{{Key: "a"}, {Key: "b"}},
			want:   []corev1.Taint{{Key: "a"}, {Key: "b"}},
		},
		{
			name:   "removes every effect of the key and keeps order",
			taints: []corev1.Taint{{Key: "a"}, {Key: "k", Effect: corev1.TaintEffectNoSchedule}, {Key: "b"}, {Key: "k", Effect: corev1.TaintEffectNoExecute}},
			want:   []corev1.Taint{{Key: "a"}, {Key: "b"}},
		},
		{
			name:   "only the key",
			taints: []corev1.Taint{{Key: "k"}},
			want:   []corev1.Taint{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var original []corev1.Taint
			if tt.taints != nil {
				original = slices.Clone(tt.taints)
			}
			got := removeTaint(tt.taints, "k")
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("removeTaint() = %+v, want %+v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.taints, original) {
				t.Errorf("removeTaint mutated its input: %+v", tt.taints)
			}
		})
	}
}
