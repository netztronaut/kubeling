package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

func TestMergeExternalIPsMoreCases(t *testing.T) {
	ext := func(ip string) corev1.NodeAddress {
		return corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: ip}
	}
	tests := []struct {
		name        string
		addresses   []corev1.NodeAddress
		externalIPs []string
		want        []corev1.NodeAddress
		wantChanged bool
	}{
		{
			name:        "empty rule IP list is a no-op even when ordering is off",
			addresses:   []corev1.NodeAddress{ext("203.0.113.1"), ext("2001:db8::1")},
			externalIPs: []string{},
			want:        []corev1.NodeAddress{ext("203.0.113.1"), ext("2001:db8::1")},
			wantChanged: false,
		},
		{
			name:        "duplicate existing ExternalIPs are collapsed",
			addresses:   []corev1.NodeAddress{ext("203.0.113.1"), ext("203.0.113.1")},
			externalIPs: []string{"203.0.113.1"},
			want:        []corev1.NodeAddress{ext("203.0.113.1")},
			wantChanged: true,
		},
		{
			name: "other address types keep their relative order",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node-1"},
				ext("198.51.100.1"),
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeInternalDNS, Address: "node-1.cluster.local"},
			},
			externalIPs: []string{"198.51.100.1"},
			want: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node-1"},
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeInternalDNS, Address: "node-1.cluster.local"},
				ext("198.51.100.1"),
			},
			wantChanged: true,
		},
		{
			name:        "an InternalIP with the same address does not satisfy an ExternalIP",
			addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "203.0.113.1"}},
			externalIPs: []string{"203.0.113.1"},
			want:        []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "203.0.113.1"}, ext("203.0.113.1")},
			wantChanged: true,
		},
		{
			name:        "IPv4-mapped IPv6 addresses sort as IPv4",
			addresses:   nil,
			externalIPs: []string{"::ffff:203.0.113.1", "2001:db8::1"},
			want:        []corev1.NodeAddress{ext("2001:db8::1"), ext("::ffff:203.0.113.1")},
			wantChanged: true,
		},
		{
			name:        "unparseable addresses sort after IPv6 and stay stable among IPv4",
			addresses:   nil,
			externalIPs: []string{"not-an-ip", "203.0.113.1", "2001:db8::1"},
			want:        []corev1.NodeAddress{ext("2001:db8::1"), ext("not-an-ip"), ext("203.0.113.1")},
			wantChanged: true,
		},
		{
			name:        "existing IPs not in any rule are kept",
			addresses:   []corev1.NodeAddress{ext("198.51.100.7")},
			externalIPs: []string{"203.0.113.1"},
			want:        []corev1.NodeAddress{ext("198.51.100.7"), ext("203.0.113.1")},
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

func TestMergeExternalIPsIsIdempotent(t *testing.T) {
	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
		{Type: corev1.NodeHostName, Address: "node-1"},
	}
	externalIPs := []string{"2001:db8::1", "203.0.113.2"}

	once, changed := mergeExternalIPs(addresses, externalIPs)
	if !changed {
		t.Fatal("expected the first merge to change something")
	}
	twice, changed := mergeExternalIPs(once, externalIPs)
	if changed || !reflect.DeepEqual(once, twice) {
		t.Errorf("second merge changed = %v: %+v -> %+v", changed, once, twice)
	}
}

func TestIsIPv6(t *testing.T) {
	tests := map[string]bool{
		"2001:db8::1":        true,
		"::1":                true,
		"::":                 true,
		"fe80::1%eth0":       false, // zones don't parse
		"203.0.113.1":        false,
		"::ffff:203.0.113.1": false,
		"":                   false,
		"not-an-ip":          false,
		"2001:db8::1/64":     false,
	}
	for addr, want := range tests {
		if got := isIPv6(addr); got != want {
			t.Errorf("isIPv6(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestCompareBool(t *testing.T) {
	tests := []struct {
		a, b bool
		want int
	}{
		{false, false, 0},
		{true, true, 0},
		{true, false, 1},
		{false, true, -1},
	}
	for _, tt := range tests {
		if got := compareBool(tt.a, tt.b); got != tt.want {
			t.Errorf("compareBool(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAddressesEqual(t *testing.T) {
	a := corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.1"}
	b := corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.2"}
	internal := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "203.0.113.1"}
	tests := []struct {
		name string
		x, y []corev1.NodeAddress
		want bool
	}{
		{name: "both nil", want: true},
		{name: "nil and empty", x: nil, y: []corev1.NodeAddress{}, want: true},
		{name: "equal", x: []corev1.NodeAddress{a, b}, y: []corev1.NodeAddress{a, b}, want: true},
		{name: "different order", x: []corev1.NodeAddress{a, b}, y: []corev1.NodeAddress{b, a}},
		{name: "different length", x: []corev1.NodeAddress{a}, y: []corev1.NodeAddress{a, b}},
		{name: "same address, different type", x: []corev1.NodeAddress{a}, y: []corev1.NodeAddress{internal}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := addressesEqual(tt.x, tt.y); got != tt.want {
				t.Errorf("addressesEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExternalIPControllerScenarios(t *testing.T) {
	edge := func(addresses ...corev1.NodeAddress) *corev1.Node {
		n := node("edge-1", map[string]string{"zone": "edge"})
		n.Status.Addresses = addresses
		return n
	}
	ext := func(ip string) corev1.NodeAddress {
		return corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: ip}
	}
	edgeRules := func(ips ...string) *staticConfig {
		return &staticConfig{config.Config{ExternalIPs: map[string]config.ExternalIPRule{"edge": {
			Match:       config.Match{NodeSelector: map[string]string{"zone": "edge"}},
			ExternalIPs: ips,
		}}}}
	}
	newController := func(t *testing.T, h *harness, source ConfigSource) *ExternalIPController {
		t.Helper()
		c, err := NewExternalIPController(h.client, h.nodes, source)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	t.Run("missing node is ignored", func(t *testing.T) {
		h := newHarness(t)
		c := newController(t, h, edgeRules("203.0.113.1"))
		if err := c.reconcile(context.Background(), "gone"); err != nil {
			t.Errorf("reconcile: %v", err)
		}
	})

	t.Run("non-matching node is untouched", func(t *testing.T) {
		h := newHarness(t, node("core-1", map[string]string{"zone": "core"}))
		c := newController(t, h, edgeRules("203.0.113.1"))

		writes, got := h.converge(c.reconcile, "core-1")

		if writes != 0 || len(got.Status.Addresses) != 0 || len(got.Status.Conditions) != 0 {
			t.Errorf("writes = %d, status = %+v", writes, got.Status)
		}
	})

	t.Run("addresses already present only need the Applied condition", func(t *testing.T) {
		h := newHarness(t, edge(ext("203.0.113.1")))
		c := newController(t, h, edgeRules("203.0.113.1"))

		writes, got := h.converge(c.reconcile, "edge-1")

		if writes != 1 {
			t.Errorf("writes = %d, want 1", writes)
		}
		if cond := condition(got, ExternalIPsAppliedConditionType); cond == nil || cond.Reason != "Applied" {
			t.Errorf("condition = %+v, want Applied", cond)
		}
	})

	t.Run("matching rule without IPs is Applied", func(t *testing.T) {
		h := newHarness(t, edge(ext("203.0.113.1"), ext("2001:db8::1")))
		c := newController(t, h, edgeRules())

		writes, got := h.converge(c.reconcile, "edge-1")

		if writes != 1 {
			t.Errorf("writes = %d, want 1", writes)
		}
		// Without rule IPs nothing is renormalized.
		if want := []corev1.NodeAddress{ext("203.0.113.1"), ext("2001:db8::1")}; !reflect.DeepEqual(got.Status.Addresses, want) {
			t.Errorf("addresses = %+v, want %+v", got.Status.Addresses, want)
		}
	})

	t.Run("reordering alone goes through Pending", func(t *testing.T) {
		h := newHarness(t, edge(ext("203.0.113.1"), ext("2001:db8::1")))
		c := newController(t, h, edgeRules("2001:db8::1"))

		writes, got := h.converge(c.reconcile, "edge-1")

		if writes != 3 {
			t.Errorf("writes = %d, want 3", writes)
		}
		if want := []corev1.NodeAddress{ext("2001:db8::1"), ext("203.0.113.1")}; !reflect.DeepEqual(got.Status.Addresses, want) {
			t.Errorf("addresses = %+v, want %+v", got.Status.Addresses, want)
		}
	})

	t.Run("removed addresses are restored", func(t *testing.T) {
		h := newHarness(t, edge())
		c := newController(t, h, edgeRules("203.0.113.1"))
		_, got := h.converge(c.reconcile, "edge-1")

		got.Status.Addresses = nil
		if _, err := h.client.CoreV1().Nodes().UpdateStatus(context.Background(), got, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		h.sync("edge-1")
		writes, got := h.converge(c.reconcile, "edge-1")

		if writes != 3 {
			t.Errorf("writes = %d, want 3", writes)
		}
		if want := []corev1.NodeAddress{ext("203.0.113.1")}; !reflect.DeepEqual(got.Status.Addresses, want) {
			t.Errorf("addresses = %+v, want %+v", got.Status.Addresses, want)
		}
	})

	t.Run("an already Pending node goes straight to writing addresses", func(t *testing.T) {
		n := edge()
		n.Status.Conditions = []corev1.NodeCondition{pendingCondition(ExternalIPsAppliedConditionType, []string{"edge"})}
		h := newHarness(t, n)
		c := newController(t, h, edgeRules("203.0.113.1"))

		writes, _ := h.converge(c.reconcile, "edge-1")

		if writes != 2 {
			t.Errorf("writes = %d, want 2", writes)
		}
	})

	t.Run("other conditions are preserved", func(t *testing.T) {
		n := edge()
		n.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			appliedCondition(LabeledConditionType, []string{"x"}),
		}
		h := newHarness(t, n)
		c := newController(t, h, edgeRules("203.0.113.1"))

		_, got := h.converge(c.reconcile, "edge-1")

		if !hasCondition(got, corev1.NodeReady) || !hasCondition(got, LabeledConditionType) || !hasCondition(got, ExternalIPsAppliedConditionType) {
			t.Errorf("conditions = %+v", got.Status.Conditions)
		}
	})

	for name, tc := range map[string]struct {
		err     error
		wantErr bool
	}{
		"conflict writing addresses is swallowed": {err: errConflict},
		"error writing addresses is returned":     {err: errInternal, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, edge())
			c := newController(t, h, edgeRules("203.0.113.1"))
			if err := c.reconcile(context.Background(), "edge-1"); err != nil {
				t.Fatalf("setting Pending: %v", err)
			}
			h.sync("edge-1")
			h.failUpdates("status", tc.err)

			err := c.reconcile(context.Background(), "edge-1")

			if !tc.wantErr {
				if err != nil {
					t.Errorf("error = %v, want nil", err)
				}
				return
			}
			if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), `updating node "edge-1" externalIPs`) {
				t.Errorf("error = %v", err)
			}
		})
	}
}
