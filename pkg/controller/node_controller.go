// Package controller implements the controllers a minimal out-of-tree
// cloud provider needs: one that stamps a ProviderID onto every Node and
// removes the taint the kubelet sets when started with
// --cloud-provider=external, and one that applies externalIPs from
// ConfigMap-defined policies onto matching Nodes.
package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
)

// UninitializedTaintKey is the taint the kubelet applies to a Node when it
// is started with --cloud-provider=external. A cloud-controller-manager is
// expected to remove it once the Node has been initialized.
const UninitializedTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"

// NodeController watches Nodes and initializes the ones that were
// registered by a kubelet running with --cloud-provider=external.
type NodeController struct {
	client       kubernetes.Interface
	providerName string
	lister       corev1listers.NodeLister
	synced       cache.InformerSynced
	queue        workqueue.TypedRateLimitingInterface[string]
}

// NewNodeController builds a NodeController that stamps ProviderIDs
// formatted as "<providerName>://<node-name>". The Node informer is owned
// by the caller (typically a shared informer factory) and must be started
// separately.
func NewNodeController(client kubernetes.Interface, nodes corev1informers.NodeInformer, providerName string) *NodeController {
	c := &NodeController{
		client:       client,
		providerName: providerName,
		lister:       nodes.Lister(),
		synced:       nodes.Informer().HasSynced,
		queue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	nodes.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, new interface{}) { c.enqueue(new) },
	})

	return c
}

func (c *NodeController) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.queue.Add(key)
}

// Run starts the reconcile workers, blocking until ctx is canceled.
func (c *NodeController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("starting node controller", "providerID", c.providerName)

	if !cache.WaitForCacheSync(ctx.Done(), c.synced) {
		return fmt.Errorf("failed to wait for node cache to sync")
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	klog.InfoS("stopping node controller")
	return nil
}

func (c *NodeController) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *NodeController) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("reconciling node %q: %w", key, err))
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

func (c *NodeController) reconcile(ctx context.Context, key string) error {
	node, err := c.lister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	needsProviderID := node.Spec.ProviderID == ""
	needsTaintRemoval := hasUninitializedTaint(node)
	if !needsProviderID && !needsTaintRemoval {
		return nil
	}

	updated := node.DeepCopy()
	if needsProviderID {
		updated.Spec.ProviderID = fmt.Sprintf("%s://%s", c.providerName, node.Name)
	}
	if needsTaintRemoval {
		updated.Spec.Taints = removeTaint(updated.Spec.Taints, UninitializedTaintKey)
	}

	_, err = c.client.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		// The informer will observe the newer version and requeue.
		return nil
	}
	if err != nil {
		return err
	}

	klog.InfoS("initialized node", "node", node.Name, "providerID", updated.Spec.ProviderID)
	return nil
}

func hasUninitializedTaint(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == UninitializedTaintKey {
			return true
		}
	}
	return false
}

func removeTaint(taints []corev1.Taint, key string) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(taints))
	for _, t := range taints {
		if t.Key == key {
			continue
		}
		out = append(out, t)
	}
	return out
}
