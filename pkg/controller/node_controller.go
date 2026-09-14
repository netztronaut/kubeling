package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// UninitializedTaintKey is the taint the kubelet applies to a Node when it
// is started with --cloud-provider=external. A cloud-controller-manager is
// expected to remove it once the Node has been initialized.
const UninitializedTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"

// NodeController watches Nodes and initializes the ones that were
// registered by a kubelet running with --cloud-provider=external.
type NodeController struct {
	*nodeQueue
	client       kubernetes.Interface
	providerName string
}

// NewNodeController builds a NodeController that stamps ProviderIDs
// formatted as "<providerName>://<node-name>". The Node informer is owned
// by the caller (typically a shared informer factory) and must be started
// separately.
func NewNodeController(client kubernetes.Interface, nodes corev1informers.NodeInformer, providerName string) (*NodeController, error) {
	c := &NodeController{client: client, providerName: providerName}
	q, err := newNodeQueue("node", nodes, c.reconcile)
	if err != nil {
		return nil, err
	}
	c.nodeQueue = q
	return c, nil
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
