package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func TestMetadataDrift(t *testing.T) {
	d := metadataDrift(
		map[string]string{"same": "1", "changed-b": "old", "changed-a": "old", "extra": "x"},
		map[string]string{"same": "1", "changed-b": "new", "changed-a": "new", "missing-b": "", "missing-a": "v"},
	)
	want := drift{missing: []string{"missing-a", "missing-b"}, changed: []string{"changed-a", "changed-b"}}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("drift = %+v, want %+v", d, want)
	}
	if got := d.String(); got != "missing: missing-a, missing-b; changed: changed-a, changed-b" {
		t.Errorf("String() = %q", got)
	}
}

func TestFingerprint(t *testing.T) {
	a := fingerprint(map[string]string{"a": "1", "b": ""})
	if a != fingerprint(map[string]string{"b": "", "a": "1"}) {
		t.Error("fingerprint depends on order")
	}
	for _, other := range []map[string]string{{"a": "1"}, {"a": "1", "b": "2"}, {"a": "1", "c": ""}} {
		if fingerprint(other) == a {
			t.Errorf("fingerprint(%v) collides", other)
		}
	}
	if externalIPFingerprint([]string{"192.0.2.1", "2001:db8::1", "192.0.2.1"}) != externalIPFingerprint([]string{"2001:db8::1", "192.0.2.1"}) {
		t.Error("externalIPFingerprint depends on order or duplicates")
	}
}

func TestExternalIPDrift(t *testing.T) {
	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "192.0.2.1"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
	}

	d := externalIPDrift(addresses, []string{"203.0.113.1", "192.0.2.1", "2001:db8::1", "2001:db8::1"})
	if want := []string{"192.0.2.1", "2001:db8::1"}; !reflect.DeepEqual(d.missing, want) || d.changed != nil {
		t.Errorf("drift = %+v, want missing %v", d, want)
	}
	if got := externalIPDrift(addresses, []string{"203.0.113.1"}).String(); got != "out of order" {
		t.Errorf("String() = %q, want out of order", got)
	}
}

func TestRecentWriters(t *testing.T) {
	since := time.Date(2026, 1, 1, 12, 0, 0, 500_000_000, time.UTC)
	at := func(d time.Duration) *metav1.Time {
		t := metav1.NewTime(since.Truncate(time.Second).Add(d))
		return &t
	}
	fields := metav1.NewFieldsV1
	labels := fields(`{"f:metadata":{"f:labels":{"f:example.com/role":{}}}}`)

	n := node("node-1", nil)
	n.ManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "before", Time: at(-time.Second), FieldsV1: labels},
		{Manager: "same-second", Time: at(0), FieldsV1: labels},
		{Manager: "latest", Time: at(3 * time.Second), FieldsV1: labels},
		{Manager: "latest", Time: at(2 * time.Second), FieldsV1: labels},
		{Manager: "annotations-only", Time: at(time.Second), FieldsV1: fields(`{"f:metadata":{"f:annotations":{}}}`)},
		{Manager: "status", Subresource: "status", Time: at(time.Second), FieldsV1: labels},
		{Manager: fieldManager, Time: at(4 * time.Second), FieldsV1: labels},
		{Manager: "no-time", FieldsV1: labels},
		{Manager: "no-fields", Time: at(time.Second)},
		{Manager: "broken", Time: at(time.Second), FieldsV1: fields(`{`)},
	}

	got := recentWriters(n, "", since, "f:metadata", "f:labels")
	if want := []string{"latest", "same-second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("recentWriters = %v, want %v", got, want)
	}
	if got := recentWriters(n, "status", since, "f:metadata"); !reflect.DeepEqual(got, []string{"status"}) {
		t.Errorf("recentWriters(status) = %v", got)
	}
}

func TestDriftedCondition(t *testing.T) {
	d := drift{missing: []string{"a"}, changed: []string{"b"}}

	c := driftedCondition(LabeledConditionType, []string{"x", "y"}, d, []string{"policy", "other"}, 4*time.Second)
	if c.Status != corev1.ConditionFalse || c.Reason != "Drifted" ||
		c.Message != "Values of matching rule(s) x, y were changed by policy, other shortly after being applied (missing: a; changed: b); restoring them after a 4s cooldown." {
		t.Errorf("condition = %+v", c)
	}
	if c := driftedCondition(LabeledConditionType, []string{"x"}, d, nil, time.Second); c.Message !=
		"Values of matching rule(s) x were changed shortly after being applied (missing: a; changed: b); restoring them after a 1s cooldown." {
		t.Errorf("message without writers = %q", c.Message)
	}
}

func TestDeletedNodesAreForgotten(t *testing.T) {
	h := newHarness(t)
	q, err := newNodeQueue("test", h.nodes, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		q.flaps.applied(name, "v1")
		if cooldown, _, _ := q.flaps.drifted(name, time.Time{}); cooldown == 0 {
			t.Fatalf("no cooldown for %q", name)
		}
	}

	q.forget(node("a", nil))
	q.forget(cache.DeletedFinalStateUnknown{Key: "b", Obj: node("b", nil)})
	q.forget(struct{}{})

	for _, name := range []string{"a", "b"} {
		if got := q.flaps.remaining(name); got != 0 {
			t.Errorf("remaining for deleted %q = %s", name, got)
		}
	}
}
