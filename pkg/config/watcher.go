package config

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// Key is the ConfigMap data key holding the YAML configuration document.
const Key = "config.yaml"

// Watcher keeps an in-memory, always-current snapshot of the Config parsed
// from a single ConfigMap's "config.yaml" key, obtained via the Kubernetes
// API (list+watch) rather than a mounted volume.
type Watcher struct {
	namespace string
	name      string
	informer  cache.SharedIndexInformer
	current   atomic.Pointer[Config]

	// OnChange, if set before Run is called, is invoked whenever the parsed
	// configuration changes.
	OnChange func()
}

// NewWatcher builds a Watcher for the ConfigMap "namespace/name". Call Run
// to start watching; Current returns a zero-value Config until the first
// successful load.
func NewWatcher(client kubernetes.Interface, namespace, name string, resync time.Duration) (*Watcher, error) {
	w := &Watcher{namespace: namespace, name: name}
	w.current.Store(&Config{})

	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	w.informer = cache.NewSharedIndexInformer(
		// Wrapped like client-go's generated informers, so clients that
		// can't stream initial events (such as the fake clientset) opt out
		// of WatchList instead of stalling the initial sync.
		cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = selector
				return client.CoreV1().ConfigMaps(namespace).List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = selector
				return client.CoreV1().ConfigMaps(namespace).Watch(ctx, options)
			},
		}, client),
		&corev1.ConfigMap{},
		resync,
		cache.Indexers{},
	)

	_, err := w.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.load(obj) },
		UpdateFunc: func(_, obj any) { w.load(obj) },
		DeleteFunc: func(any) { w.clear() },
	})
	if err != nil {
		return nil, fmt.Errorf("registering configmap event handler: %w", err)
	}

	return w, nil
}

// Current returns the most recently loaded configuration. Safe to call from
// multiple goroutines.
func (w *Watcher) Current() Config {
	return *w.current.Load()
}

// Run starts watching the ConfigMap, blocking until ctx is canceled.
func (w *Watcher) Run(ctx context.Context) {
	w.informer.Run(ctx.Done())
}

func (w *Watcher) ref() string {
	return fmt.Sprintf("%s/%s", w.namespace, w.name)
}

func (w *Watcher) load(obj any) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}

	raw, ok := cm.Data[Key]
	if !ok {
		klog.InfoS("configmap has no config.yaml key, treating as empty", "configmap", w.ref())
		w.clear()
		return
	}

	cfg, err := Parse([]byte(raw))
	if err != nil {
		klog.ErrorS(err, "invalid configmap config.yaml, keeping previous configuration", "configmap", w.ref())
		return
	}

	w.current.Store(&cfg)
	klog.InfoS("loaded rule configuration", "configmap", w.ref(),
		"externalIPs", len(cfg.ExternalIPs), "labels", len(cfg.Labels), "annotations", len(cfg.Annotations))
	if w.OnChange != nil {
		w.OnChange()
	}
}

// Parse decodes and validates a "config.yaml" document. Unknown fields are
// rejected rather than ignored, so a typo or an outdated schema fails
// loudly instead of silently applying nothing.
func Parse(raw []byte) (Config, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", Key, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (w *Watcher) clear() {
	w.current.Store(&Config{})
	if w.OnChange != nil {
		w.OnChange()
	}
}
