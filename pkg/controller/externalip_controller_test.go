package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/netztronaut/kubeling/pkg/config"
)

func TestMergeExternalIPs(t *testing.T) {
	tests := []struct {
		name        string
		addresses   []corev1.NodeAddress
		externalIPs []string
		want        []corev1.NodeAddress
		wantChanged bool
	}{
		{
			name:        "no rule IPs is a no-op",
			addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}},
			externalIPs: nil,
			want:        []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}},
			wantChanged: false,
		},
		{
			name:        "adds missing external IPs without touching others",
			addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}},
			externalIPs: []string{"203.0.113.1", "203.0.113.2"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.2"},
			},
			wantChanged: true,
		},
		{
			name: "already present is a no-op",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			externalIPs: []string{"203.0.113.1"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			wantChanged: false,
		},
		{
			name: "only adds the missing subset",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			externalIPs: []string{"203.0.113.1", "203.0.113.2"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.2"},
			},
			wantChanged: true,
		},
		{
			name:        "duplicate IPs across rules are only added once",
			addresses:   nil,
			externalIPs: []string{"203.0.113.1", "203.0.113.2", "203.0.113.1"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.2"},
			},
			wantChanged: true,
		},
		{
			name:        "v6 addresses sort before v4 regardless of input order",
			addresses:   nil,
			externalIPs: []string{"203.0.113.1", "2001:db8::1", "203.0.113.2", "2001:db8::2"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "2001:db8::1"},
				{Type: corev1.NodeExternalIP, Address: "2001:db8::2"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.2"},
			},
			wantChanged: true,
		},
		{
			name: "existing v4-before-v6 ordering is normalized",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				{Type: corev1.NodeExternalIP, Address: "2001:db8::1"},
			},
			externalIPs: []string{"203.0.113.1", "2001:db8::1"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "2001:db8::1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			wantChanged: true,
		},
		{
			name: "ExternalIP block moves after other address types",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "2001:db8::1"},
				{Type: corev1.NodeHostName, Address: "node-1"},
			},
			externalIPs: []string{"2001:db8::1", "203.0.113.1"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node-1"},
				{Type: corev1.NodeExternalIP, Address: "2001:db8::1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := mergeExternalIPs(tt.addresses, tt.externalIPs)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMergeExternalIPsDoesNotMutateSharedBackingArray(t *testing.T) {
	// addresses is deliberately given spare capacity to simulate a slice
	// backed by an array that a shared informer cache object also points
	// to; mergeExternalIPs must never write into that spare capacity.
	backing := make([]corev1.NodeAddress, 1, 4)
	backing[0] = corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}

	out, changed := mergeExternalIPs(backing, []string{"203.0.113.1"})
	if !changed {
		t.Fatalf("expected a change")
	}
	if len(backing) != 1 || cap(backing) < 2 {
		t.Fatalf("backing array capacity assumption violated by test setup")
	}

	// Reslice into the original backing array's spare capacity: if
	// mergeExternalIPs wrote through it, this would observe the new IP.
	probe := backing[:2]
	if probe[1].Address == "203.0.113.1" {
		t.Fatalf("mergeExternalIPs mutated the caller's backing array")
	}
	if len(out) != 2 {
		t.Fatalf("got %d addresses, want 2", len(out))
	}
}

func TestExternalIPControllerReconcile(t *testing.T) {
	edge := func() *corev1.Node {
		n := node("edge-1", map[string]string{"zone": "edge"})
		n.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
			{Type: corev1.NodeExternalIP, Address: "198.51.100.7"},
		}
		return n
	}
	rules := map[string]config.ExternalIPRule{
		"edge-v4": {
			Match:       config.Match{NodeSelector: map[string]string{"zone": "edge"}},
			ExternalIPs: []string{"203.0.113.10"},
		},
		"edge-v6": {
			Match:       config.Match{NodeSelector: map[string]string{"zone": "edge"}},
			ExternalIPs: []string{"2001:db8::10"},
		},
	}

	h := newHarness(t, edge())
	source := &staticConfig{config.Config{ExternalIPs: rules}}
	c, err := NewExternalIPController(h.client, h.nodes, source)
	if err != nil {
		t.Fatal(err)
	}

	writes, got := h.converge(c.reconcile, "edge-1")

	if writes != 3 {
		t.Errorf("writes = %d, want 3", writes)
	}
	want := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
		{Type: corev1.NodeExternalIP, Address: "2001:db8::10"},
		{Type: corev1.NodeExternalIP, Address: "198.51.100.7"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
	}
	if !reflect.DeepEqual(got.Status.Addresses, want) {
		t.Errorf("addresses = %+v, want %+v", got.Status.Addresses, want)
	}
	cond := condition(got, ExternalIPsAppliedConditionType)
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatalf("condition = %+v, want Applied", cond)
	}
	if cond.Message != "Applied from matching rule(s): edge-v4, edge-v6." {
		t.Errorf("condition message = %q", cond.Message)
	}

	source.cfg = config.Config{}
	writes, got = h.converge(c.reconcile, "edge-1")

	if writes != 1 {
		t.Errorf("writes after removing rules = %d, want 1", writes)
	}
	if condition(got, ExternalIPsAppliedConditionType) != nil {
		t.Errorf("condition still present after all rules were removed")
	}
	if !reflect.DeepEqual(got.Status.Addresses, want) {
		t.Errorf("applied addresses were changed: %+v", got.Status.Addresses)
	}
}
