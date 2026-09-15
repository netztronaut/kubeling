package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"

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
	c, err := NewNodeController(h.client, h.nodes, "custom")
	if err != nil {
		t.Fatal(err)
	}

	c.EnqueueAll()

	if got := c.queue.Len(); got != 2 {
		t.Errorf("queue length = %d, want 2", got)
	}
}
