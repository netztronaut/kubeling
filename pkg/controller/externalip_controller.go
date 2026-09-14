package controller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	client  kubernetes.Interface
	watcher *config.Watcher
	lister  corev1listers.NodeLister
	synced  cache.InformerSynced
	queue   workqueue.TypedRateLimitingInterface[string]
}

// NewExternalIPController builds an ExternalIPController. The Node
// informer is owned by the caller (typically a shared informer factory)
// and must be started separately.
func NewExternalIPController(client kubernetes.Interface, nodes corev1informers.NodeInformer, watcher *config.Watcher) *ExternalIPController {
	c := &ExternalIPController{
		client:  client,
		watcher: watcher,
		lister:  nodes.Lister(),
		synced:  nodes.Informer().HasSynced,
		queue:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	nodes.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, new interface{}) { c.enqueue(new) },
	})

	return c
}

func (c *ExternalIPController) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.queue.Add(key)
}

// EnqueueAll re-evaluates every known Node, e.g. after the configuration
// changes. Exported so a config.Watcher's OnChange hook can call it.
func (c *ExternalIPController) EnqueueAll() {
	nodes, err := c.lister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("listing nodes to re-enqueue: %w", err))
		return
	}
	for _, node := range nodes {
		c.queue.Add(node.Name)
	}
}

// Run starts the reconcile workers, blocking until ctx is canceled.
func (c *ExternalIPController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("starting externalIP controller")

	if !cache.WaitForCacheSync(ctx.Done(), c.synced) {
		return fmt.Errorf("failed to wait for node cache to sync")
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	klog.InfoS("stopping externalIP controller")
	return nil
}

func (c *ExternalIPController) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *ExternalIPController) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("reconciling externalIPs for node %q: %w", key, err))
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

// reconcile applies at most one change per call: clearing a stale
// condition, setting the Pending condition, writing the addresses
// themselves, or setting the Applied condition. Each write bumps the
// Node's resourceVersion, which the informer observes and re-enqueues, so
// the next step follows on a fresh object.
func (c *ExternalIPController) reconcile(ctx context.Context, key string) error {
	cfg := c.watcher.Current()
	rules := cfg.ExternalIPs
	if len(rules) == 0 {
		return nil
	}

	node, err := c.lister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	matched := matchingIDs(node, rules, func(r config.ExternalIPRule) config.Match { return r.Match })

	if len(matched) == 0 {
		return c.ensureConditionAbsent(ctx, node)
	}

	var externalIPs []string
	for _, id := range matched {
		externalIPs = append(externalIPs, rules[id].ExternalIPs...)
	}
	newAddresses, changed := mergeExternalIPs(node.Status.Addresses, externalIPs)

	if !changed {
		return c.ensureCondition(ctx, node, appliedCondition(ExternalIPsAppliedConditionType, matched))
	}

	if !conditionUpToDate(node, pendingCondition(ExternalIPsAppliedConditionType, matched)) {
		return c.ensureCondition(ctx, node, pendingCondition(ExternalIPsAppliedConditionType, matched))
	}

	updated := node.DeepCopy()
	updated.Status.Addresses = newAddresses
	if _, err := c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating node %q externalIPs: %w", node.Name, err)
	}
	klog.InfoS("applied node externalIPs", "node", node.Name, "rules", matched, "externalIPs", externalIPs)
	return nil
}

func (c *ExternalIPController) ensureCondition(ctx context.Context, node *corev1.Node, desired corev1.NodeCondition) error {
	if conditionUpToDate(node, desired) {
		return nil
	}
	updated := node.DeepCopy()
	setCondition(updated, desired)
	if _, err := c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating node %q externalIPs condition: %w", node.Name, err)
	}
	klog.InfoS("updated node condition", "domain", "externalIPs", "node", node.Name, "status", desired.Status, "reason", desired.Reason)
	return nil
}

func (c *ExternalIPController) ensureConditionAbsent(ctx context.Context, node *corev1.Node) error {
	if !hasCondition(node, ExternalIPsAppliedConditionType) {
		return nil
	}
	updated := node.DeepCopy()
	removeCondition(updated, ExternalIPsAppliedConditionType)
	if _, err := c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("clearing node %q externalIPs condition: %w", node.Name, err)
	}
	klog.InfoS("cleared node condition, no rule matches", "domain", "externalIPs", "node", node.Name)
	return nil
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
