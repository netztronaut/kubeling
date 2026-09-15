package controller

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/netztronaut/kubeling/pkg/config"
)

// staticConfig is a ConfigSource whose configuration tests set directly.
type staticConfig struct {
	cfg config.Config
}

func (s *staticConfig) Current() config.Config { return s.cfg }

// harness wires a controller to a fake API server and a Node informer whose
// cache is filled by hand instead of by a running informer, so reconcile
// can be driven step by step.
type harness struct {
	t      *testing.T
	client *fake.Clientset
	nodes  corev1informers.NodeInformer
}

func newHarness(t *testing.T, nodes ...*corev1.Node) *harness {
	t.Helper()
	client := fake.NewClientset()
	h := &harness{
		t:      t,
		client: client,
		nodes:  informers.NewSharedInformerFactory(client, 0).Core().V1().Nodes(),
	}
	for _, n := range nodes {
		if _, err := client.CoreV1().Nodes().Create(context.Background(), n, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating node %q: %v", n.Name, err)
		}
		h.sync(n.Name)
	}
	return h
}

// sync copies a Node from the fake API server into the informer cache, as
// a running informer would after every write.
func (h *harness) sync(name string) *corev1.Node {
	h.t.Helper()
	node, err := h.client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("getting node %q: %v", name, err)
	}
	if err := h.nodes.Informer().GetIndexer().Update(node); err != nil {
		h.t.Fatalf("caching node %q: %v", name, err)
	}
	return node
}

// writes counts the update requests sent to the fake API server so far.
func (h *harness) writes() int {
	n := 0
	for _, a := range h.client.Actions() {
		if a.GetVerb() == "update" {
			n++
		}
	}
	return n
}

// converge calls reconcile for a Node until it stops writing, syncing the
// cache after each pass. It returns the number of writes it took and the
// resulting Node.
func (h *harness) converge(reconcile func(context.Context, string) error, name string) (int, *corev1.Node) {
	h.t.Helper()
	start := h.writes()
	for range 10 {
		before := h.writes()
		if err := reconcile(context.Background(), name); err != nil {
			h.t.Fatalf("reconcile: %v", err)
		}
		node := h.sync(name)
		if h.writes() == before {
			return before - start, node
		}
	}
	h.t.Fatalf("node %q did not converge within 10 passes", name)
	return 0, nil
}

// failUpdates makes every update of a Node's subresource ("" for the Node
// object itself, "status" for its status) fail with err. The request is
// still recorded, so it counts towards writes.
func (h *harness) failUpdates(subresource string, err error) {
	h.client.PrependReactor("update", "nodes", func(a clienttesting.Action) (bool, runtime.Object, error) {
		return a.GetSubresource() == subresource, nil, err
	})
}

var (
	errConflict = apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "node", errors.New("object has been modified"))
	errInternal = apierrors.NewInternalError(errors.New("etcd is on fire"))
)

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func condition(node *corev1.Node, t corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == t {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

func TestEnqueueAll(t *testing.T) {
	h := newHarness(t, node("a", nil), node("b", nil))
	c, err := NewInitializationController(h.client, h.nodes, &staticConfig{})
	if err != nil {
		t.Fatal(err)
	}

	c.EnqueueAll()

	if got := c.queue.Len(); got != 2 {
		t.Errorf("queue length = %d, want 2", got)
	}
}

func TestEnqueueAllWithoutNodes(t *testing.T) {
	h := newHarness(t)
	c, err := NewInitializationController(h.client, h.nodes, &staticConfig{})
	if err != nil {
		t.Fatal(err)
	}

	c.EnqueueAll()

	if got := c.queue.Len(); got != 0 {
		t.Errorf("queue length = %d, want 0", got)
	}
}

func TestEnqueue(t *testing.T) {
	h := newHarness(t)
	q, err := newNodeQueue("test", h.nodes, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	q.enqueue(node("a", nil))
	q.enqueue(node("a", map[string]string{"changed": "true"}))
	q.enqueue(node("b", nil))
	// Not an object with metadata: reported via HandleError and skipped.
	q.enqueue(struct{}{})

	if got := q.queue.Len(); got != 2 {
		t.Fatalf("queue length = %d, want 2 (deduplicated a, b)", got)
	}
	for _, want := range []string{"a", "b"} {
		key, _ := q.queue.Get()
		if key != want {
			t.Errorf("dequeued %q, want %q", key, want)
		}
		q.queue.Done(key)
	}
}

func TestInformerEventsEnqueue(t *testing.T) {
	h := newHarness(t)
	c, err := NewInitializationController(h.client, h.nodes, &staticConfig{})
	if err != nil {
		t.Fatal(err)
	}

	informer := h.nodes.Informer()
	ctx := t.Context()
	go informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		t.Fatal("informer did not sync")
	}

	if _, err := h.client.CoreV1().Nodes().Create(ctx, node("added", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForKey(t, c.nodeQueue, "added")

	updated := node("added", map[string]string{"k": "v"})
	if _, err := h.client.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForKey(t, c.nodeQueue, "added")

	if err := h.client.CoreV1().Nodes().Delete(ctx, "added", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Deletes are deliberately not enqueued: there is nothing to clean up.
	time.Sleep(100 * time.Millisecond)
	if got := c.queue.Len(); got != 0 {
		t.Errorf("queue length after delete = %d, want 0", got)
	}
}

func waitForKey(t *testing.T, q *nodeQueue, want string) {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		key, _ := q.queue.Get()
		q.queue.Done(key)
		got <- key
	}()
	select {
	case key := <-got:
		if key != want {
			t.Errorf("dequeued %q, want %q", key, want)
		}
	case <-time.After(10 * time.Second):
		q.queue.ShutDown()
		t.Fatalf("timed out waiting for %q to be enqueued", want)
	}
}

func TestProcessNextItem(t *testing.T) {
	h := newHarness(t)
	failures := 2
	var calls []string
	q, err := newNodeQueue("test", h.nodes, func(_ context.Context, key string) error {
		calls = append(calls, key)
		if failures > 0 {
			failures--
			return errInternal
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	q.queue.Add("a")

	// Two failures are retried with rate limiting ...
	for i := 1; i <= 2; i++ {
		if !q.processNextItem(ctx) {
			t.Fatalf("processNextItem returned false on attempt %d", i)
		}
		if got := q.queue.NumRequeues("a"); got != i {
			t.Errorf("requeues after failure %d = %d, want %d", i, got, i)
		}
	}
	// ... and success forgets the key's failure history.
	if !q.processNextItem(ctx) {
		t.Fatal("processNextItem returned false on success")
	}
	if got := q.queue.NumRequeues("a"); got != 0 {
		t.Errorf("requeues after success = %d, want 0", got)
	}
	if want := []string{"a", "a", "a"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("reconcile calls = %v, want %v", calls, want)
	}

	q.queue.ShutDown()
	if q.processNextItem(ctx) {
		t.Error("processNextItem returned true after shutdown")
	}
}

func TestRunFailsWithoutCacheSync(t *testing.T) {
	h := newHarness(t)
	c, err := NewInitializationController(h.client, h.nodes, &staticConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The informer is never started, so the cache can't sync before ctx
	// is done.
	if err := c.Run(ctx, 1); err == nil {
		t.Fatal("expected an error")
	}
	if !c.queue.ShuttingDown() {
		t.Error("queue was not shut down")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	var reconciled atomic.Int32
	q, err := newNodeQueue("test", h.nodes, func(context.Context, string) error {
		reconciled.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The harness fills the cache by hand, so report it as synced.
	q.synced = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx, 3) }()

	for _, key := range []string{"a", "b", "c"} {
		q.queue.Add(key)
	}
	if err := wait.PollUntilContextTimeout(context.Background(), 10*time.Millisecond, 10*time.Second, true,
		func(context.Context) (bool, error) { return reconciled.Load() == 3, nil }); err != nil {
		t.Fatalf("workers reconciled %d keys, want 3", reconciled.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if !q.queue.ShuttingDown() {
		t.Error("queue was not shut down")
	}
}

// TestControllersConverge runs every controller against a started informer,
// the way main wires them up, and checks that a freshly registered Node ends
// up fully initialized with every rule applied.
func TestControllersConverge(t *testing.T) {
	client := fake.NewClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	nodes := factory.Core().V1().Nodes()
	source := &staticConfig{config.Config{
		Initialization: map[string]config.InitializationRule{"edge": {
			Match:            config.Match{NodeSelector: map[string]string{"zone": "edge"}},
			ProviderIDScheme: "custom",
		}},
		ExternalIPs: map[string]config.ExternalIPRule{"edge": {
			Match:       config.Match{NodeSelector: map[string]string{"zone": "edge"}},
			ExternalIPs: []string{"203.0.113.10", "2001:db8::10"},
		}},
		Labels: map[string]config.LabelRule{"edge": {
			Match:  config.Match{ProviderIDPattern: `^custom://edge-`},
			Labels: map[string]string{"environment": "production"},
		}},
		Annotations: map[string]config.AnnotationRule{"edge": {
			Match: config.Match{SelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
				{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"edge-1"}},
			}}}},
			Annotations: map[string]string{"example.com/rack": "r42"},
		}},
	}}

	ic, err := NewInitializationController(client, nodes, source)
	if err != nil {
		t.Fatal(err)
	}
	lc, err := NewLabelController(client, nodes, source)
	if err != nil {
		t.Fatal(err)
	}
	ac, err := NewAnnotationController(client, nodes, source)
	if err != nil {
		t.Fatal(err)
	}
	eic, err := NewExternalIPController(client, nodes, source)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	runners := []interface {
		Run(context.Context, int) error
	}{ic, lc, ac, eic}
	done := make(chan error, len(runners))
	for _, r := range runners {
		go func() { done <- r.Run(ctx, 2) }()
	}
	defer func() {
		cancel()
		for range runners {
			if err := <-done; err != nil {
				t.Errorf("controller Run: %v", err)
			}
		}
		factory.Shutdown()
	}()

	n := node("edge-1", map[string]string{"zone": "edge"})
	n.Spec.Taints = []corev1.Taint{{Key: UninitializedTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule}}
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}
	if _, err := client.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	wantAddresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
		{Type: corev1.NodeExternalIP, Address: "2001:db8::10"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
	}
	var got *corev1.Node
	converged := func(ctx context.Context) (bool, error) {
		got, err = client.CoreV1().Nodes().Get(ctx, "edge-1", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, ct := range []corev1.NodeConditionType{LabeledConditionType, AnnotatedConditionType, ExternalIPsAppliedConditionType} {
			if c := condition(got, ct); c == nil || c.Status != corev1.ConditionTrue {
				return false, nil
			}
		}
		return got.Spec.ProviderID == "custom://edge-1" &&
			!hasUninitializedTaint(got) &&
			got.Labels["environment"] == "production" &&
			got.Annotations["example.com/rack"] == "r42" &&
			reflect.DeepEqual(got.Status.Addresses, wantAddresses), nil
	}
	if err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 20*time.Second, true, converged); err != nil {
		t.Fatalf("node did not converge: %v\nspec: %+v\nlabels: %v\nannotations: %v\nstatus: %+v",
			err, got.Spec, got.Labels, got.Annotations, got.Status)
	}
}
