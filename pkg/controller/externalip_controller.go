package controller

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/netztronaut/kubeling/pkg/config"
)

// ExternalIPController reconciles Node status.addresses ExternalIP entries
// from ConfigMap-defined rules (Config.ExternalIPs), matched by
// nodeSelector and/or providerIDPattern. Every matching rule's IPs are
// unioned onto the Node; nothing is ever removed automatically, even if a
// rule is later changed or removed, or a Node stops matching.
//
// The kubeling.io/ExternalIPsApplied condition tracks live applicability:
// absent while no rule matches, Pending while a match exists but hasn't
// been fully applied yet, Applied once it has.
type ExternalIPController struct {
	*nodeQueue
	client kubernetes.Interface
	config ConfigSource
}

// NewExternalIPController builds an ExternalIPController. The Node
// informer is owned by the caller (typically a shared informer factory)
// and must be started separately.
func NewExternalIPController(client kubernetes.Interface, nodes corev1informers.NodeInformer, source ConfigSource) (*ExternalIPController, error) {
	c := &ExternalIPController{client: client, config: source}
	q, err := newNodeQueue("externalIPs", nodes, c.reconcile)
	if err != nil {
		return nil, err
	}
	c.nodeQueue = q
	return c, nil
}

// reconcile applies at most one change per call: clearing a stale
// condition, setting the Pending (or Drifted) condition, writing the
// addresses themselves, or setting the Applied condition. Each write bumps
// the Node's resourceVersion, which the informer observes and re-enqueues,
// so the next step follows on a fresh object. See beforeApply for how
// addresses changed by someone else are restored.
func (c *ExternalIPController) reconcile(ctx context.Context, key string) error {
	node, err := c.lister.Get(key)
	if apierrors.IsNotFound(err) {
		c.flaps.forget(key)
		return nil
	}
	if err != nil {
		return err
	}

	// An empty rule map matches nothing, which still has to clear a
	// condition left behind by rules that have since been removed.
	rules := c.config.Current().ExternalIPs
	matched := matchingIDs(node, rules, func(r config.ExternalIPRule) config.Match { return r.Match })
	if len(matched) == 0 {
		c.flaps.forget(key)
		return ensureConditionAbsent(ctx, c.client, node, ExternalIPsAppliedConditionType)
	}

	var externalIPs []string
	for _, id := range matched {
		externalIPs = append(externalIPs, rules[id].ExternalIPs...)
	}
	newAddresses, changed := mergeExternalIPs(node.Status.Addresses, externalIPs)

	if !changed {
		return ensureCondition(ctx, c.client, node, appliedCondition(ExternalIPsAppliedConditionType, matched),
			"verdict", "addresses of matching rules are applied")
	}

	writers := func(since time.Time) []string {
		return recentWriters(node, "status", since, "f:status", "f:addresses")
	}
	desired := externalIPFingerprint(externalIPs)
	apply, err := c.beforeApply(ctx, c.client, node, ExternalIPsAppliedConditionType, matched, desired, externalIPDrift(node.Status.Addresses, externalIPs), writers)
	if !apply {
		return err
	}

	updated := node.DeepCopy()
	updated.Status.Addresses = newAddresses
	if _, err := c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating node %q externalIPs: %w", node.Name, err)
	}
	c.flaps.applied(node.Name, desired)
	klog.InfoS("applied node externalIPs", "node", node.Name, "rules", matched, "externalIPs", externalIPs)
	return nil
}

// externalIPFingerprint identifies a set of rule-supplied IPs, independent
// of order and duplicates.
func externalIPFingerprint(externalIPs []string) string {
	set := make(map[string]bool, len(externalIPs))
	for _, ip := range externalIPs {
		set[ip] = true
	}
	return fingerprint(set)
}

// mergeExternalIPs computes the ExternalIP addresses a Node should have:
// the union of its existing ExternalIP addresses and externalIPs, deduped
// and ordered with IPv6 addresses before IPv4 ones (stable within each
// family). The resulting ExternalIP entries are placed as a single block
// after every other address type, since that's where they end up anyway
// and it keeps the ordering well-defined regardless of how they were
// originally interleaved. Other address types are left untouched. It
// reports whether anything changed.
func mergeExternalIPs(addresses []corev1.NodeAddress, externalIPs []string) ([]corev1.NodeAddress, bool) {
	if len(externalIPs) == 0 {
		return addresses, false
	}

	var others []corev1.NodeAddress
	var merged []string
	seen := make(map[string]bool, len(addresses)+len(externalIPs))
	for _, addr := range addresses {
		if addr.Type != corev1.NodeExternalIP {
			others = append(others, addr)
			continue
		}
		if seen[addr.Address] {
			continue
		}
		seen[addr.Address] = true
		merged = append(merged, addr.Address)
	}
	for _, ip := range externalIPs {
		if seen[ip] {
			continue
		}
		seen[ip] = true
		merged = append(merged, ip)
	}

	slices.SortStableFunc(merged, func(a, b string) int {
		// IPv6 (true) sorts before IPv4 (false); equal families keep order.
		return compareBool(isIPv6(b), isIPv6(a))
	})

	out := make([]corev1.NodeAddress, 0, len(others)+len(merged))
	out = append(out, others...)
	for _, ip := range merged {
		out = append(out, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: ip})
	}

	if addressesEqual(out, addresses) {
		return addresses, false
	}
	return out, true
}

// isIPv6 reports whether addr parses as an IPv6 address. Unparseable
// strings are treated as not IPv6 so they sort after real IPv6 addresses
// rather than causing a panic.
func isIPv6(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.To4() == nil
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

func addressesEqual(a, b []corev1.NodeAddress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
