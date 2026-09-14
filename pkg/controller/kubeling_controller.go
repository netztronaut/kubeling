package controller

import (
	"context"
	"fmt"
	"regexp"
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

// OnboardedConditionType is the NodeCondition type KubelingController
// maintains to reflect onboarding state. Namespaced to avoid colliding
// with condition types other tooling might add to the Node.
const OnboardedConditionType corev1.NodeConditionType = "kubeling.io/Onboarded"

// KubelingController watches Nodes and, for each one whose
// spec.providerID matches an onboarding rule's providerIDPattern, applies
// that rule's label once and reflects the outcome in the
// OnboardedConditionType NodeCondition: Pending until a rule matches,
// Onboarded once the label has been applied. Onboarding is a one-time
// stamp, not a continuously-enforced policy: once a Node is Onboarded, its
// label and condition are never re-evaluated or removed, even if the
// matching rule is later changed or removed (mirrors the "nothing is ever
// removed automatically" convention PolicyController uses for
// externalIPs).
type KubelingController struct {
	client  kubernetes.Interface
	watcher *config.Watcher
	lister  corev1listers.NodeLister
	synced  cache.InformerSynced
	queue   workqueue.TypedRateLimitingInterface[string]
}

// NewKubelingController builds a KubelingController. The Node informer is
// owned by the caller (typically a shared informer factory) and must be
// started separately.
func NewKubelingController(client kubernetes.Interface, nodes corev1informers.NodeInformer, watcher *config.Watcher) *KubelingController {
	c := &KubelingController{
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

func (c *KubelingController) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.queue.Add(key)
}

// EnqueueAll re-evaluates every known Node against the current onboarding
// configuration. Exported so a config.Watcher's OnChange hook can call it
// directly (alongside PolicyController.Enqueue) when the ConfigMap changes,
// since an edited rule should re-evaluate Nodes that didn't previously
// match.
func (c *KubelingController) EnqueueAll() {
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
func (c *KubelingController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("starting kubeling controller")

	if !cache.WaitForCacheSync(ctx.Done(), c.synced) {
		return fmt.Errorf("failed to wait for node cache to sync")
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	klog.InfoS("stopping kubeling controller")
	return nil
}

func (c *KubelingController) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *KubelingController) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("reconciling onboarding for node %q: %w", key, err))
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

// reconcile applies at most one change to the Node per call: the
// onboarding label, or the onboarding condition, never both. An Update
// (label write) triggers the informer's UpdateFunc, which re-enqueues the
// Node so the condition write follows on the next pass against a
// known-fresh object.
func (c *KubelingController) reconcile(ctx context.Context, key string) error {
	cfg := c.watcher.Current()
	if len(cfg.Onboarding) == 0 {
		return nil
	}

	node, err := c.lister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	rule, ruleID, matched := matchOnboardingRule(cfg.Onboarding, node.Spec.ProviderID)

	if matched && node.Labels[rule.Label] != rule.LabelValue {
		updated := node.DeepCopy()
		if updated.Labels == nil {
			updated.Labels = map[string]string{}
		}
		updated.Labels[rule.Label] = rule.LabelValue

		_, err := c.client.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			return nil
		}
		if err != nil {
			return err
		}
		klog.InfoS("onboarded node", "node", node.Name, "rule", ruleID, "label", rule.Label, "value", rule.LabelValue)
		return nil
	}

	desired := pendingCondition()
	if matched {
		desired = onboardedCondition(ruleID)
	}
	if conditionUpToDate(node, desired) {
		return nil
	}

	updated := node.DeepCopy()
	setCondition(updated, desired)

	_, err = c.client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return nil
	}
	if err != nil {
		return err
	}
	klog.InfoS("updated node onboarding condition", "node", node.Name, "status", desired.Status, "reason", desired.Reason)
	return nil
}

// matchOnboardingRule returns the first rule (in sorted-ID order, for
// deterministic behavior when multiple rules could match) whose
// ProviderIDPattern matches providerID. A Node with no providerID yet
// never matches.
func matchOnboardingRule(rules map[string]config.OnboardingRule, providerID string) (rule config.OnboardingRule, ruleID string, matched bool) {
	if providerID == "" {
		return config.OnboardingRule{}, "", false
	}

	ids := make([]string, 0, len(rules))
	for id := range rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		r := rules[id]
		// Already validated by config.Config.Validate when the watcher
		// loaded this configuration, so compilation is not expected to
		// fail here.
		re, err := regexp.Compile(r.ProviderIDPattern)
		if err != nil {
			klog.ErrorS(err, "onboarding rule has invalid providerIDPattern, skipping", "rule", id)
			continue
		}
		if re.MatchString(providerID) {
			return r, id, true
		}
	}
	return config.OnboardingRule{}, "", false
}

func pendingCondition() corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:    OnboardedConditionType,
		Status:  corev1.ConditionFalse,
		Reason:  "Pending",
		Message: "No onboarding rule matches this node's providerID yet.",
	}
}

func onboardedCondition(ruleID string) corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:    OnboardedConditionType,
		Status:  corev1.ConditionTrue,
		Reason:  "Onboarded",
		Message: fmt.Sprintf("Onboarded via rule %q.", ruleID),
	}
}

// conditionUpToDate reports whether node already carries desired's Type,
// Status, Reason and Message (LastTransitionTime/LastHeartbeatTime are
// ignored for this comparison since setCondition manages those itself).
func conditionUpToDate(node *corev1.Node, desired corev1.NodeCondition) bool {
	for _, c := range node.Status.Conditions {
		if c.Type != desired.Type {
			continue
		}
		return c.Status == desired.Status && c.Reason == desired.Reason && c.Message == desired.Message
	}
	return false
}

// setCondition replaces node's condition of desired.Type with desired (or
// appends it if absent), preserving LastTransitionTime when the status
// hasn't changed and stamping both timestamps to now otherwise.
func setCondition(node *corev1.Node, desired corev1.NodeCondition) {
	now := metav1.Now()
	desired.LastHeartbeatTime = now
	desired.LastTransitionTime = now

	for i, c := range node.Status.Conditions {
		if c.Type != desired.Type {
			continue
		}
		if c.Status == desired.Status {
			desired.LastTransitionTime = c.LastTransitionTime
		}
		node.Status.Conditions[i] = desired
		return
	}
	node.Status.Conditions = append(node.Status.Conditions, desired)
}
