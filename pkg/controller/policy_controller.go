package controller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/steigr/kubeling/pkg/config"
)

// syncKey is the sole workqueue item used to trigger a full policy
// reconcile pass: policy application depends on the whole Node set and the
// whole policy set together, so there's nothing narrower to key work by.
const syncKey = "sync"

// PolicyController applies externalIPs, labels and annotations from
// matching policies onto Nodes. Policies come from a config.Watcher backed
// by a ConfigMap and are matched against Nodes by nodeSelector.
type PolicyController struct {
	client  kubernetes.Interface
	watcher *config.Watcher
	lister  corev1listers.NodeLister
	synced  cache.InformerSynced
	queue   workqueue.TypedRateLimitingInterface[string]
}

// NewPolicyController builds a PolicyController. The Node informer is
// owned by the caller (typically a shared informer factory) and must be
// started separately.
func NewPolicyController(client kubernetes.Interface, nodes corev1informers.NodeInformer, watcher *config.Watcher) *PolicyController {
	c := &PolicyController{
		client:  client,
		watcher: watcher,
		lister:  nodes.Lister(),
		synced:  nodes.Informer().HasSynced,
		queue:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	nodes.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { c.Enqueue() },
		UpdateFunc: func(_, _ interface{}) { c.Enqueue() },
		DeleteFunc: func(interface{}) { c.Enqueue() },
	})

	return c
}

// Enqueue schedules a policy reconcile pass. Exported so a config.Watcher
// can call it directly (as OnChange) when the backing ConfigMap changes.
func (c *PolicyController) Enqueue() {
	c.queue.Add(syncKey)
}

// Run starts the reconcile worker, blocking until ctx is canceled.
func (c *PolicyController) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("starting policy controller")

	if !cache.WaitForCacheSync(ctx.Done(), c.synced) {
		return fmt.Errorf("failed to wait for node cache to sync")
	}

	go wait.UntilWithContext(ctx, c.runWorker, time.Second)

	<-ctx.Done()
	klog.InfoS("stopping policy controller")
	return nil
}

func (c *PolicyController) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *PolicyController) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx); err != nil {
		utilruntime.HandleError(fmt.Errorf("reconciling policies: %w", err))
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

// reconcile applies policies to Nodes one change at a time: each pass walks
// Nodes in a stable (sorted-name) order, resolves the policies matching
// each one, applies the first actual change it finds, and restarts.
// Reconcile is done once a full pass makes no change to any Node.
func (c *PolicyController) reconcile(ctx context.Context) error {
	for {
		changed, err := c.applyOnePass(ctx)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
	}
}

func (c *PolicyController) applyOnePass(ctx context.Context) (bool, error) {
	cfg := c.watcher.Current()
	if len(cfg.Policies) == 0 {
		return false, nil
	}

	ids := make([]string, 0, len(cfg.Policies))
	selectors := make(map[string]labels.Selector, len(cfg.Policies))
	for id, policy := range cfg.Policies {
		ids = append(ids, id)
		selectors[id] = labels.SelectorFromSet(labels.Set(policy.NodeSelector))
	}
	sort.Strings(ids)

	nodes, err := c.lister.List(labels.Everything())
	if err != nil {
		return false, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	for _, node := range nodes {
		var matching []string
		for _, id := range ids {
			if selectors[id].Matches(labels.Set(node.Labels)) {
				matching = append(matching, id)
			}
		}
		if len(matching) == 0 {
			continue
		}

		changed, err := c.applyToNode(ctx, node, matching, cfg.Policies)
		if err != nil {
			return false, err
		}
		if changed {
			return true, nil
		}
	}

	return false, nil
}

// applyToNode resolves the policies matching a single Node and applies at
// most one field-group change (labels+annotations, or externalIPs) so the
// caller can restart the pass with a fresh Node listing.
func (c *PolicyController) applyToNode(ctx context.Context, node *corev1.Node, matching []string, policies map[string]config.Policy) (bool, error) {
	desiredLabels := resolveKeyValues("labels", node.Name, matching, policies, func(p config.Policy) map[string]string { return p.Labels })
	desiredAnnotations := resolveKeyValues("annotations", node.Name, matching, policies, func(p config.Policy) map[string]string { return p.Annotations })

	newLabels, labelsChanged := applyOverwrite(node.Labels, desiredLabels)
	newAnnotations, annotationsChanged := applyOverwrite(node.Annotations, desiredAnnotations)

	if labelsChanged || annotationsChanged {
		updated := node.DeepCopy()
		if labelsChanged {
			updated.Labels = newLabels
		}
		if annotationsChanged {
			updated.Annotations = newAnnotations
		}
		if _, err := c.client.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return false, fmt.Errorf("updating node %q metadata: %w", node.Name, err)
		}
		klog.InfoS("applied policy metadata to node", "node", node.Name, "policies", matching, "labelsChanged", labelsChanged, "annotationsChanged", annotationsChanged)
		return true, nil
	}

	var externalIPs []string
	for _, id := range matching {
		externalIPs = append(externalIPs, policies[id].ExternalIPs...)
	}
	newAddresses, addressesChanged := mergeExternalIPs(node.Status.Addresses, externalIPs)
	if addressesChanged {
		updated := node.DeepCopy()
		updated.Status.Addresses = newAddresses
		if _, err := c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return false, fmt.Errorf("updating node %q status: %w", node.Name, err)
		}
		klog.InfoS("applied policy externalIPs to node", "node", node.Name, "policies", matching, "externalIPs", externalIPs)
		return true, nil
	}

	return false, nil
}

// resolveKeyValues merges the label or annotation maps (selected by get)
// from every policy in matching for a single Node. A key is included only
// if every matching policy that sets it agrees on the value; otherwise
// it's a conflict between authoritative policies, which is logged and the
// key is left out entirely so the policies never fight over it.
func resolveKeyValues(kind, node string, matching []string, policies map[string]config.Policy, get func(config.Policy) map[string]string) map[string]string {
	type entry struct {
		policyID string
		value    string
	}
	byKey := map[string][]entry{}
	for _, id := range matching {
		for k, v := range get(policies[id]) {
			byKey[k] = append(byKey[k], entry{policyID: id, value: v})
		}
	}

	resolved := make(map[string]string, len(byKey))
	for k, entries := range byKey {
		value := entries[0].value
		conflicting := false
		for _, e := range entries[1:] {
			if e.value != value {
				conflicting = true
				break
			}
		}
		if conflicting {
			policyIDs := make([]string, len(entries))
			for i, e := range entries {
				policyIDs[i] = e.policyID
			}
			klog.ErrorS(nil, "conflicting policy values for node, leaving key untouched", "kind", kind, "node", node, "key", k, "policies", policyIDs)
			continue
		}
		resolved[k] = value
	}
	return resolved
}

// applyOverwrite computes the result of authoritatively setting every
// key/value in desired onto existing, overwriting whatever is already
// there. Keys not mentioned in desired are left untouched. It reports
// whether anything changed.
func applyOverwrite(existing map[string]string, desired map[string]string) (map[string]string, bool) {
	if len(desired) == 0 {
		return existing, false
	}

	changed := false
	for k, v := range desired {
		if cur, ok := existing[k]; !ok || cur != v {
			changed = true
			break
		}
	}
	if !changed {
		return existing, false
	}

	out := make(map[string]string, len(existing)+len(desired))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range desired {
		out[k] = v
	}
	return out, true
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

	sort.SliceStable(merged, func(i, j int) bool {
		return isIPv6(merged[i]) && !isIPv6(merged[j])
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
