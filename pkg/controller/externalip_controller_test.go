package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
