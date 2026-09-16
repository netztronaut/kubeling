package controller

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/netztronaut/kubeling/pkg/config"
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
	// field is the managed fields path of the domain's values.
	field string
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
	*nodeQueue
	domain metadataDomain[T]
	client kubernetes.Interface
	config ConfigSource
}

// NewLabelController builds a MetadataController that reconciles Node
// labels from Config.Labels.
func NewLabelController(client kubernetes.Interface, nodes corev1informers.NodeInformer, source ConfigSource) (*MetadataController[config.LabelRule], error) {
	return newMetadataController(client, nodes, source, metadataDomain[config.LabelRule]{
		kind:          "labels",
		conditionType: LabeledConditionType,
		rules:         func(cfg config.Config) map[string]config.LabelRule { return cfg.Labels },
		match:         func(r config.LabelRule) config.Match { return r.Match },
		values:        func(r config.LabelRule) map[string]string { return r.Labels },
		get:           func(n *corev1.Node) map[string]string { return n.Labels },
		set:           func(n *corev1.Node, v map[string]string) { n.Labels = v },
		field:         "f:labels",
	})
}

// NewAnnotationController builds a MetadataController that reconciles Node
// annotations from Config.Annotations.
func NewAnnotationController(client kubernetes.Interface, nodes corev1informers.NodeInformer, source ConfigSource) (*MetadataController[config.AnnotationRule], error) {
	return newMetadataController(client, nodes, source, metadataDomain[config.AnnotationRule]{
		kind:          "annotations",
		conditionType: AnnotatedConditionType,
		rules:         func(cfg config.Config) map[string]config.AnnotationRule { return cfg.Annotations },
		match:         func(r config.AnnotationRule) config.Match { return r.Match },
		values:        func(r config.AnnotationRule) map[string]string { return r.Annotations },
		get:           func(n *corev1.Node) map[string]string { return n.Annotations },
		set:           func(n *corev1.Node, v map[string]string) { n.Annotations = v },
		field:         "f:annotations",
	})
}

func newMetadataController[T any](client kubernetes.Interface, nodes corev1informers.NodeInformer, source ConfigSource, domain metadataDomain[T]) (*MetadataController[T], error) {
	c := &MetadataController[T]{domain: domain, client: client, config: source}
	q, err := newNodeQueue(domain.kind, nodes, c.reconcile)
	if err != nil {
		return nil, err
	}
	c.nodeQueue = q
	return c, nil
}

// reconcile applies at most one change per call: clearing a stale
// condition, setting the Pending (or Drifted) condition, writing the
// metadata itself, or setting the Applied condition. Each write bumps the
// Node's resourceVersion, which the informer observes and re-enqueues, so
// the next step follows on a fresh object. See beforeApply for how values
// changed by someone else are restored.
func (c *MetadataController[T]) reconcile(ctx context.Context, key string) error {
	node, err := c.lister.Get(key)
	if apierrors.IsNotFound(err) {
		c.flaps.forget(key)
		return nil
	}
	if err != nil {
		return err
	}

	// An empty rule map matches nothing, which still has to clear a
	// condition left behind by rules that have since been removed.
	rules := c.domain.rules(c.config.Current())
	matched := matchingIDs(node, rules, c.domain.match)
	if len(matched) == 0 {
		c.flaps.forget(key)
		return ensureConditionAbsent(ctx, c.client, node, c.domain.conditionType)
	}

	desired := resolveValues(c.domain.kind, node.Name, matched, rules, c.domain.values)
	newValues, changed := applyOverwrite(c.domain.get(node), desired)

	if !changed {
		return ensureCondition(ctx, c.client, node, appliedCondition(c.domain.conditionType, matched),
			"verdict", "values of matching rules are applied")
	}

	writers := func(since time.Time) []string {
		return recentWriters(node, "", since, "f:metadata", c.domain.field)
	}
	apply, err := c.beforeApply(ctx, c.client, node, c.domain.conditionType, matched, fingerprint(desired), metadataDrift(c.domain.get(node), desired), writers)
	if !apply {
		return err
	}

	updated := node.DeepCopy()
	c.domain.set(updated, newValues)
	if _, err := c.client.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating node %q %s: %w", node.Name, c.domain.kind, err)
	}
	c.flaps.applied(node.Name, fingerprint(desired))
	klog.InfoS("applied node metadata", "domain", c.domain.kind, "node", node.Name, "rules", matched)
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
	maps.Copy(out, existing)
	maps.Copy(out, desired)
	return out, true
}
