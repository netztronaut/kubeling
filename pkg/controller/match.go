package controller

import (
	"regexp"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"

	"github.com/steigr/kubeling/pkg/config"
)

// matches reports whether node satisfies m. An unset NodeSelector,
// SelectorTerms or ProviderIDPattern imposes no constraint; node must
// satisfy every one that is set.
func matches(node *corev1.Node, m config.Match) bool {
	if len(m.NodeSelector) > 0 {
		if !labels.SelectorFromSet(labels.Set(m.NodeSelector)).Matches(labels.Set(node.Labels)) {
			return false
		}
	}
	if len(m.SelectorTerms) > 0 {
		ns, err := nodeaffinity.NewNodeSelector(&corev1.NodeSelector{NodeSelectorTerms: m.SelectorTerms})
		if err != nil {
			// Already validated by config.Config.Validate, like
			// providerIDPattern below.
			klog.ErrorS(err, "rule has invalid selectorTerms, treating as non-matching")
			return false
		}
		if !ns.Match(node) {
			return false
		}
	}
	if m.ProviderIDPattern != "" {
		re, err := compilePattern(m.ProviderIDPattern)
		if err != nil {
			// Already validated by config.Config.Validate when the watcher
			// loaded this configuration; treat as non-matching if it
			// somehow still fails to compile here.
			klog.ErrorS(err, "rule has invalid providerIDPattern, treating as non-matching", "pattern", m.ProviderIDPattern)
			return false
		}
		if !re.MatchString(node.Spec.ProviderID) {
			return false
		}
	}
	return true
}

// patterns caches compiled providerIDPatterns, since every rule is matched
// against every Node on each reconcile. Only a handful of distinct patterns
// exist at any time, so the cache is never pruned.
var patterns sync.Map // string -> *regexp.Regexp

func compilePattern(pattern string) (*regexp.Regexp, error) {
	if re, ok := patterns.Load(pattern); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	patterns.Store(pattern, re)
	return re, nil
}

// matchingIDs returns the IDs (sorted, for deterministic ordering) of
// every rule in rules that matches node.
func matchingIDs[T any](node *corev1.Node, rules map[string]T, match func(T) config.Match) []string {
	var ids []string
	for id, r := range rules {
		if matches(node, match(r)) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
