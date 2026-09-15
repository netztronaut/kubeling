// Command kubeling is a minimal out-of-tree cloud
// controller manager for clusters that have no real cloud backing them. It
// lets kubelets run with --cloud-provider=external by stamping a
// "custom://<node-name>" ProviderID onto every Node and removing the
// node.cloudprovider.kubernetes.io/uninitialized taint. It can optionally
// apply externalIPs, labels and annotations to Nodes based on rules read
// from a ConfigMap, each matched by nodeSelector and/or providerIDPattern.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/netztronaut/kubeling/pkg/config"
	"github.com/netztronaut/kubeling/pkg/controller"
)

func main() {
	klog.InitFlags(nil)

	var (
		kubeconfig              string
		providerName            string
		leaderElect             bool
		leaderElectionNamespace string
		leaseLockName           string
		resyncPeriod            time.Duration
		workers                 int
		healthAddr              string
		configMapRef            string
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig file. If unset, in-cluster config is used.")
	flag.StringVar(&providerName, "provider-id", "custom", "Scheme used for the ProviderID stamped onto Nodes (providerID = <provider-id>://<node-name>).")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election so only one replica reconciles Nodes at a time.")
	flag.StringVar(&leaderElectionNamespace, "leader-elect-namespace", "kube-system", "Namespace holding the leader election Lease.")
	flag.StringVar(&leaseLockName, "leader-elect-lease-name", "kubeling", "Name of the leader election Lease.")
	flag.DurationVar(&resyncPeriod, "resync-period", 10*time.Minute, "Node and ConfigMap informer resync period.")
	flag.IntVar(&workers, "workers", 2, "Number of node reconcile workers.")
	flag.StringVar(&healthAddr, "health-addr", ":10258", "Address to serve /healthz on.")
	flag.StringVar(&configMapRef, "configmap", os.Getenv("CONFIGMAP"), "ConfigMap holding the rule configuration, as \"(namespace/)name\". The namespace defaults to this controller's own namespace when omitted. Read via the Kubernetes API, never mounted as a volume. Leave empty to disable rule processing. Defaults to the CONFIGMAP environment variable.")
	flag.Parse()

	restConfig, err := loadConfig(kubeconfig)
	if err != nil {
		klog.ErrorS(err, "failed to load kubernetes client config")
		os.Exit(1)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.ErrorS(err, "failed to build kubernetes client")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go serveHealth(healthAddr)

	factory := informers.NewSharedInformerFactory(client, resyncPeriod)
	nodeInformer := factory.Core().V1().Nodes()

	nc, err := controller.NewNodeController(client, nodeInformer, providerName)
	if err != nil {
		klog.ErrorS(err, "failed to build node controller")
		os.Exit(1)
	}

	var (
		watcher *config.Watcher
		lc      *controller.MetadataController[config.LabelRule]
		ac      *controller.MetadataController[config.AnnotationRule]
		eic     *controller.ExternalIPController
	)
	if configMapRef != "" {
		cmNamespace, cmName := config.ParseRef(configMapRef)
		if cmNamespace == "" {
			cmNamespace = config.OwnNamespace()
		}
		if watcher, err = config.NewWatcher(client, cmNamespace, cmName, resyncPeriod); err != nil {
			klog.ErrorS(err, "failed to build configmap watcher")
			os.Exit(1)
		}
		if lc, err = controller.NewLabelController(client, nodeInformer, watcher); err != nil {
			klog.ErrorS(err, "failed to build label controller")
			os.Exit(1)
		}
		if ac, err = controller.NewAnnotationController(client, nodeInformer, watcher); err != nil {
			klog.ErrorS(err, "failed to build annotation controller")
			os.Exit(1)
		}
		if eic, err = controller.NewExternalIPController(client, nodeInformer, watcher); err != nil {
			klog.ErrorS(err, "failed to build externalIP controller")
			os.Exit(1)
		}
		watcher.OnChange = func() {
			lc.EnqueueAll()
			ac.EnqueueAll()
			eic.EnqueueAll()
		}
	}

	run := func(ctx context.Context) {
		factory.Start(ctx.Done())

		var wg sync.WaitGroup

		wg.Go(func() {
			if err := nc.Run(ctx, workers); err != nil {
				klog.ErrorS(err, "node controller exited with error")
			}
		})

		if lc != nil {
			go watcher.Run(ctx)

			wg.Go(func() {
				if err := lc.Run(ctx, workers); err != nil {
					klog.ErrorS(err, "label controller exited with error")
				}
			})

			wg.Go(func() {
				if err := ac.Run(ctx, workers); err != nil {
					klog.ErrorS(err, "annotation controller exited with error")
				}
			})

			wg.Go(func() {
				if err := eic.Run(ctx, workers); err != nil {
					klog.ErrorS(err, "externalIP controller exited with error")
				}
			})
		}

		wg.Wait()
	}

	if !leaderElect {
		run(ctx)
		return
	}

	id, err := os.Hostname()
	if err != nil {
		klog.ErrorS(err, "failed to determine hostname for leader election identity")
		os.Exit(1)
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: client.CoreV1().Events(leaderElectionNamespace)})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kubeling"})

	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		leaderElectionNamespace,
		leaseLockName,
		client.CoreV1(),
		client.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: recorder,
		},
	)
	if err != nil {
		klog.ErrorS(err, "failed to create leader election lock")
		os.Exit(1)
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				klog.InfoS("leadership lost, shutting down")
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				if identity == id {
					return
				}
				klog.InfoS("observed new leader", "leader", identity)
			},
		},
	})
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

func serveHealth(addr string) {
	klog.InfoS("serving health checks", "address", addr)
	if err := http.ListenAndServe(addr, healthHandler()); err != nil {
		klog.ErrorS(err, "health server failed")
	}
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}
