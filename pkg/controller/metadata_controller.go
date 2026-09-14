package controller

import (
	"context"
	"fmt"
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

// metadataDomain bundles what differs between reconciling Node labels and
// Node annotations. Everything else (matching, conflict resolution,
// condition management, workqueue plumbing) is identical, so
// MetadataController implements it once for both, parameterized by T
// (config.LabelRule or config.AnnotationRule).
type metadataDomain[T any] struct {
	kind          string
	conditionType corev1.NodeConditionType
	rules         func(config.Config) map[string]T
	match         func(T) config.Match
	values        func(T) map[string]string
	get           func(*corev1.Node) map[string]string
	set           func(*corev1.Node, map[string]string)
}

// MetadataController reconciles either Node labels or Node annotations
// (whichever metadataDomain it was built with) from ConfigMap-defined
// rules, matched by nodeSelector and/or providerIDPattern. Matching rules'
// values are authoritatively merged and applied; same-key disagreements
// between matching rules are left untouched on both sides and logged.
// Applied values are never removed automatically, even if a rule is later
// changed or removed, or a Node stops matching.
//
// The domain's NodeCondition tracks live applicability: absent while no
// rule matches, Pending while a match exists but hasn't been fully applied
// yet, Applied once it has.
type MetadataController[T any] struct {
	domain  metadataDomain[T]
	client  kubernetes.Interface
	watcher *config.Watcher
	lister  corev1listers.NodeLister
	synced  cache.InformerSynced
	queue   workqueue.TypedRateLimitingInterface[string]
}

// NewLabelController builds a MetadataController that reconciles Node
// labels from Config.Labels.
func NewLabelController(client kubernetes.Interface, nodes corev1informers.NodeInformer, watcher *config.Watcher) *MetadataController[config.LabelRule] {
	return newMetadataController(client, nodes, watcher, metadataDomain[config.LabelRule]{
		kind:          "labels",
		conditionType: LabeledConditionType,
		rules:         func(cfg config.Config) map[string]config.LabelRule { return cfg.Labels },
		match:         func(r config.LabelRule) config.Match { return r.Match },
		values:        func(r config.LabelRule) map[string]string { return r.Labels },
		get:           func(n *corev1.Node) map[string]string { return n.Labels },
		set:           func(n *corev1.Node, v map[string]string) { n.Labels = v },
	})
}

// NewAnnotationController builds a MetadataController that reconciles Node
// annotations from Config.Annotations.
func NewAnnotationController(client kubernetes.Interface, nodes corev1informers.NodeInformer, watcher *config.Watcher) *MetadataController[config.AnnotationRule] {
	return newMetadataController(client, nodes, watcher, metadataDomain[config.AnnotationRule]{
		kind:          "annotations",
		conditionType: AnnotatedConditionType,
		rules:         func(cfg config.Config) map[string]config.AnnotationRule { return cfg.Annotations },
		match:         func(r config.AnnotationRule) config.Match { return r.Match },
		values:        func(r config.AnnotationRule) map[string]string { return r.Annotations },
		get:           func(n *corev1.Node) map[string]string { return n.Annotations },
		set:           func(n *corev1.Node, v map[string]string) { n.Annotations = v },
	})
}

func newMetadataController[T any](client kubernetes.Interface, nodes corev1informers.NodeInformer, watcher *config.Watcher, domain metadataDomain[T]) *MetadataController[T] {
	c := &MetadataController[T]{
		domain:  domain,
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

func (c *MetadataController[T]) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.queue.Add(key)
}

// EnqueueAll re-evaluates every known Node, e.g. after the configuration
// changes. Exported so a config.Watcher's OnChange hook can call it.
func (c *MetadataController[T]) EnqueueAll() {
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
func (c *MetadataController[T]) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("starting metadata controller", "domain", c.domain.kind)

	if !cache.WaitForCacheSync(ctx.Done(), c.synced) {
		return fmt.Errorf("failed to wait for node cache to sync")
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	klog.InfoS("stopping metadata controller", "domain", c.domain.kind)
	return nil
}

func (c *MetadataController[T]) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *MetadataController[T]) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("reconciling %s for node %q: %w", c.domain.kind, key, err))
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

// reconcile applies at most one change per call: clearing a stale
// condition, setting the Pending condition, writing the metadata itself,
// or setting the Applied condition. Each write bumps the Node's
// resourceVersion, which the informer observes and re-enqueues, so the
// next step follows on a fresh object.
func (c *MetadataController[T]) reconcile(ctx context.Context, key string) error {
	cfg := c.watcher.Current()
	rules := c.domain.rules(cfg)
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

	matched := matchingIDs(node, rules, c.domain.match)

	if len(matched) == 0 {
		return c.ensureConditionAbsent(ctx, node)
	}

	desired := resolveValues(c.domain.kind, node.Name, matched, rules, c.domain.values)
	newValues, changed := applyOverwrite(c.domain.get(node), desired)

	if !changed {
		return c.ensureCondition(ctx, node, appliedCondition(c.domain.conditionType, matched))
	}

	if !conditionUpToDate(node, pendingCondition(c.domain.conditionType, matched)) {
		return c.ensureCondition(ctx, node, pendingCondition(c.domain.conditionType, matched))
	}

	updated := node.DeepCopy()
	c.domain.set(updated, newValues)
	if _, err := c.client.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating node %q %s: %w", node.Name, c.domain.kind, err)
	}
	klog.InfoS("applied node metadata", "domain", c.domain.kind, "node", node.Name, "rules", matched)
	return nil
}

func (c *MetadataController[T]) ensureCondition(ctx context.Context, node *corev1.Node, desired corev1.NodeCondition) error {
	if conditionUpToDate(node, desired) {
		return nil
	}
	updated := node.DeepCopy()
	setCondition(updated, desired)
	if _, err := c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating node %q %s condition: %w", node.Name, c.domain.kind, err)
	}
	klog.InfoS("updated node condition", "domain", c.domain.kind, "node", node.Name, "status", desired.Status, "reason", desired.Reason)
	return nil
}

func (c *MetadataController[T]) ensureConditionAbsent(ctx context.Context, node *corev1.Node) error {
	if !hasCondition(node, c.domain.conditionType) {
		return nil
	}
	updated := node.DeepCopy()
	removeCondition(updated, c.domain.conditionType)
	if _, err := c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("clearing node %q %s condition: %w", node.Name, c.domain.kind, err)
	}
	klog.InfoS("cleared node condition, no rule matches", "domain", c.domain.kind, "node", node.Name)
	return nil
}

// resolveValues merges the Values maps (selected by values) of every
// matched rule for a single Node. A key is included only if every matched
// rule that sets it agrees on the value; otherwise it's a conflict between
// authoritative rules, which is logged and the key is left out entirely so
// the rules never fight over it.
func resolveValues[T any](kind, node string, matched []string, rules map[string]T, values func(T) map[string]string) map[string]string {
	type entry struct {
		ruleID string
		value  string
	}
	byKey := map[string][]entry{}
	for _, id := range matched {
		for k, v := range values(rules[id]) {
			byKey[k] = append(byKey[k], entry{ruleID: id, value: v})
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
			ruleIDs := make([]string, len(entries))
			for i, e := range entries {
				ruleIDs[i] = e.ruleID
			}
			klog.ErrorS(nil, "conflicting rule values for node, leaving key untouched", "kind", kind, "node", node, "key", k, "rules", ruleIDs)
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
