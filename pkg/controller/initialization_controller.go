package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/netztronaut/kubeling/pkg/config"
)

// UninitializedTaintKey is the taint the kubelet applies to a Node when it
// is started with --cloud-provider=external. A cloud-controller-manager is
// expected to remove it once the Node has been initialized.
const UninitializedTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"

// InitializationController initializes the Nodes matched by
// ConfigMap-defined initialization rules, for Nodes no
// cloud-controller-manager takes care of: it removes the uninitialized
// taint and, if a matching rule sets a providerIDScheme, stamps a
// providerID onto Nodes that have none. Nodes matching no rule are never
// touched, so it can run next to a real cloud-controller-manager.
type InitializationController struct {
	*nodeQueue
	client kubernetes.Interface
	config ConfigSource
}

// NewInitializationController builds an InitializationController reading
// rules from source. The Node informer is owned by the caller (typically a
// shared informer factory) and must be started separately.
func NewInitializationController(client kubernetes.Interface, nodes corev1informers.NodeInformer, source ConfigSource) (*InitializationController, error) {
	c := &InitializationController{client: client, config: source}
	q, err := newNodeQueue("initialization", nodes, c.reconcile)
	if err != nil {
		return nil, err
	}
	c.nodeQueue = q
	return c, nil
}

func (c *InitializationController) reconcile(ctx context.Context, key string) error {
	node, err := c.lister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	rules := c.config.Current().Initialization
	matched := matchingIDs(node, rules, func(r config.InitializationRule) config.Match { return r.Match })
	if len(matched) == 0 {
		return nil
	}

	providerID := node.Spec.ProviderID
	if providerID == "" {
		scheme, ok := resolveProviderIDScheme(node.Name, matched, rules)
		if !ok {
			// Initializing without the providerID the rules ask for would
			// be irreversible, so wait for the configuration to be fixed.
			return nil
		}
		if scheme != "" {
			providerID = fmt.Sprintf("%s://%s", scheme, node.Name)
		}
	}
	if providerID == node.Spec.ProviderID && !hasUninitializedTaint(node) {
		return nil
	}

	updated := node.DeepCopy()
	updated.Spec.ProviderID = providerID
	updated.Spec.Taints = removeTaint(updated.Spec.Taints, UninitializedTaintKey)

	_, err = c.client.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		// The informer will observe the newer version and requeue.
		return nil
	}
	if err != nil {
		return fmt.Errorf("initializing node %q: %w", node.Name, err)
	}

	klog.InfoS("initialized node", "node", node.Name, "providerID", updated.Spec.ProviderID, "rules", matched)
	return nil
}

// resolveProviderIDScheme returns the providerIDScheme the matched rules
// agree on, or "" if none sets one. If they disagree, it logs the conflict
// and reports false.
func resolveProviderIDScheme(nodeName string, matched []string, rules map[string]config.InitializationRule) (string, bool) {
	var schemes []string
	for _, id := range matched {
		if s := rules[id].ProviderIDScheme; s != "" && !slices.Contains(schemes, s) {
			schemes = append(schemes, s)
		}
	}
	switch len(schemes) {
	case 0:
		return "", true
	case 1:
		return schemes[0], true
	default:
		klog.ErrorS(nil, "conflicting providerIDSchemes, not initializing node", "node", nodeName, "rules", matched, "schemes", schemes)
		return "", false
	}
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
