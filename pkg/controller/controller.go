// Package controller implements Kubeling's Node controllers, each applying
// one map of ConfigMap-defined rules onto matching Nodes:
// ExternalIPController and MetadataController (labels, annotations) manage
// Node addresses and metadata, while InitializationController removes the
// taint the kubelet sets when started with --cloud-provider=external (and
// optionally stamps a providerID) on Nodes no cloud-controller-manager
// takes care of.
package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/netztronaut/kubeling/pkg/config"
)

// ConfigSource provides the current rule configuration. *config.Watcher
// implements it.
type ConfigSource interface {
	Current() config.Config
}

// nodeQueue is the workqueue plumbing shared by every controller in this
// package: Node add/update events enqueue the Node's name, and workers call
// reconcile for it, retrying with rate limiting on error. Controllers embed
// it, which also exports Run and EnqueueAll on them.
type nodeQueue struct {
	name      string
	lister    corev1listers.NodeLister
	synced    cache.InformerSynced
	queue     workqueue.TypedRateLimitingInterface[string]
	reconcile func(ctx context.Context, nodeName string) error
}

func newNodeQueue(name string, nodes corev1informers.NodeInformer, reconcile func(context.Context, string) error) (*nodeQueue, error) {
	q := &nodeQueue{
		name:      name,
		lister:    nodes.Lister(),
		synced:    nodes.Informer().HasSynced,
		reconcile: reconcile,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: name},
		),
	}

	_, err := nodes.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    q.enqueue,
		UpdateFunc: func(_, obj any) { q.enqueue(obj) },
	})
	if err != nil {
		return nil, fmt.Errorf("registering %s controller event handler: %w", name, err)
	}
	return q, nil
}

func (q *nodeQueue) enqueue(obj any) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	q.queue.Add(key)
}

// EnqueueAll re-evaluates every known Node, e.g. after the configuration
// changes.
func (q *nodeQueue) EnqueueAll() {
	nodes, err := q.lister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("listing nodes to re-enqueue for %s controller: %w", q.name, err))
		return
	}
	for _, node := range nodes {
		q.queue.Add(node.Name)
	}
}

// Run starts the reconcile workers, blocking until ctx is canceled.
func (q *nodeQueue) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer q.queue.ShutDown()

	klog.InfoS("starting controller", "controller", q.name)

	if !cache.WaitForCacheSync(ctx.Done(), q.synced) {
		return fmt.Errorf("%s controller: failed to wait for node cache to sync", q.name)
	}

	for range workers {
		go wait.UntilWithContext(ctx, q.runWorker, time.Second)
	}

	<-ctx.Done()
	klog.InfoS("stopping controller", "controller", q.name)
	return nil
}

func (q *nodeQueue) runWorker(ctx context.Context) {
	for q.processNextItem(ctx) {
	}
}

func (q *nodeQueue) processNextItem(ctx context.Context) bool {
	key, shutdown := q.queue.Get()
	if shutdown {
		return false
	}
	defer q.queue.Done(key)

	if err := q.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("%s controller: reconciling node %q: %w", q.name, key, err))
		q.queue.AddRateLimited(key)
		return true
	}

	q.queue.Forget(key)
	return true
}
