package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// fieldManager is the managedFields manager name the API server records for
// this binary's own writes: the part of its default user agent before the
// first "/".
var fieldManager, _, _ = strings.Cut(rest.DefaultKubernetesUserAgent(), "/")

// drift describes how a Node's values differ from what its matching rules
// want. It names keys (or IPs) only, never label or annotation values, so
// it is safe to log and to put into a condition message.
type drift struct {
	// missing are keys or addresses the rules set that the Node lacks.
	missing []string
	// changed are keys the Node carries with a different value.
	changed []string
}

func (d drift) String() string {
	var parts []string
	if len(d.missing) > 0 {
		parts = append(parts, "missing: "+strings.Join(d.missing, ", "))
	}
	if len(d.changed) > 0 {
		parts = append(parts, "changed: "+strings.Join(d.changed, ", "))
	}
	if len(parts) == 0 {
		return "out of order"
	}
	return strings.Join(parts, "; ")
}

// fingerprint identifies a set of desired values, independent of order.
func fingerprint[K comparable, V any](desired map[K]V) string {
	parts := make([]string, 0, len(desired))
	for k, v := range desired {
		parts = append(parts, fmt.Sprintf("%v=%v", k, v))
	}
	slices.Sort(parts)
	return strings.Join(parts, "\x00")
}

// metadataDrift compares existing labels or annotations with the desired
// ones.
func metadataDrift(existing, desired map[string]string) drift {
	var d drift
	for k, v := range desired {
		cur, ok := existing[k]
		switch {
		case !ok:
			d.missing = append(d.missing, k)
		case cur != v:
			d.changed = append(d.changed, k)
		}
	}
	slices.Sort(d.missing)
	slices.Sort(d.changed)
	return d
}

// externalIPDrift compares a Node's ExternalIP addresses with the ones its
// rules want. No missing address means only their order is off.
func externalIPDrift(addresses []corev1.NodeAddress, externalIPs []string) drift {
	var d drift
	for _, ip := range externalIPs {
		if !slices.Contains(d.missing, ip) && !slices.Contains(addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: ip}) {
			d.missing = append(d.missing, ip)
		}
	}
	return d
}

// recentWriters returns the field managers, most recent first, other than
// this binary that wrote the given subresource of node ("" for the object
// itself) at or after since, and own a field under path (e.g. "f:metadata",
// "f:labels"). Managed field timestamps only have second precision, so
// since is rounded down to the second. Managers that removed every field
// they owned no longer have an entry, so this is a best-effort hint.
func recentWriters(node *corev1.Node, subresource string, since time.Time, path ...string) []string {
	since = since.Truncate(time.Second)
	entries := slices.Clone(node.ManagedFields)
	slices.SortStableFunc(entries, func(a, b metav1.ManagedFieldsEntry) int {
		return entryTime(b).Compare(entryTime(a))
	})

	var writers []string
	for _, e := range entries {
		if e.Subresource != subresource || e.Manager == fieldManager || e.FieldsV1 == nil ||
			entryTime(e).Before(since) || slices.Contains(writers, e.Manager) {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(e.FieldsV1.GetRawBytes(), &fields); err != nil || !hasFieldPath(fields, path) {
			continue
		}
		writers = append(writers, e.Manager)
	}
	return writers
}

func entryTime(e metav1.ManagedFieldsEntry) time.Time {
	if e.Time == nil {
		return time.Time{}
	}
	return e.Time.Time
}

func hasFieldPath(fields map[string]any, path []string) bool {
	for _, p := range path {
		next, ok := fields[p].(map[string]any)
		if !ok {
			return false
		}
		fields = next
	}
	return true
}

// beforeApply decides what a rule controller does with a Node its matching
// rules' values aren't (fully) applied to. It returns true when the caller
// should write the values right away; otherwise it has already done this
// pass's single write (or nothing) and requeued the Node as needed.
//
//   - A Node whose rules want different values than the ones last applied to
//     it goes back to False (Pending), like a Node seen for the first time.
//   - A Node whose condition is True had its values applied and then changed
//     by someone else. If that happened long enough after they were applied,
//     they are restored right away and the condition stays True. Otherwise
//     it is a flap: the condition goes False (Drifted) and the Node is
//     requeued after an exponential cooldown.
//   - A Node still cooling down is only requeued.
//   - A Node without the condition gets it as False (Pending) first.
//   - A Node whose condition is already False gets its values written.
//
// desired is the fingerprint of the values to apply; writers lists who
// changed the values since the given time, for logging.
func (q *nodeQueue) beforeApply(ctx context.Context, client kubernetes.Interface, node *corev1.Node, t corev1.NodeConditionType, matched []string, desired string, d drift, writers func(since time.Time) []string) (bool, error) {
	current := findCondition(node, t)
	if current != nil && current.Status == corev1.ConditionTrue && q.flaps.rulesChanged(node.Name, desired) {
		return false, ensureCondition(ctx, client, node, pendingCondition(t, matched),
			"missing", d.missing, "changed", d.changed,
			"verdict", "matching rules changed since their values were applied, applying the new values next")
	}
	if current != nil && current.Status == corev1.ConditionTrue {
		cooldown, flaps, appliedAt := q.flaps.drifted(node.Name, current.LastHeartbeatTime.Time)
		changedBy := writers(appliedAt)
		if cooldown == 0 {
			klog.InfoS("restoring node values changed by another writer",
				"controller", q.name, "node", node.Name, "rules", matched,
				"missing", d.missing, "changed", d.changed, "changedBy", changedBy,
				"verdict", "values held for longer than the flap window, restoring now and keeping the condition True")
			return true, nil
		}
		q.queue.AddAfter(node.Name, cooldown)
		return false, ensureCondition(ctx, client, node, driftedCondition(t, matched, d, changedBy, cooldown),
			"missing", d.missing, "changed", d.changed, "changedBy", changedBy, "flaps", flaps, "cooldown", cooldown,
			"verdict", fmt.Sprintf("values were changed by another writer %s after being applied, restoring them after the cooldown",
				q.flaps.now().Sub(appliedAt).Round(time.Millisecond)))
	}

	if wait := q.flaps.remaining(node.Name); wait > 0 {
		q.queue.AddAfter(node.Name, wait)
		return false, nil
	}

	if current == nil {
		return false, ensureCondition(ctx, client, node, pendingCondition(t, matched),
			"missing", d.missing, "changed", d.changed,
			"verdict", "values of matching rules are not applied yet, applying them next")
	}
	return true, nil
}
