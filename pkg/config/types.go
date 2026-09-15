// Package config loads Kubeling's Node rule configuration
// from a ConfigMap, read via the Kubernetes API rather than a mounted
// volume.
package config

import (
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
)

// Match describes which Nodes a rule applies to. Every constraint is
// optional; an unset one imposes no restriction. When several are set, a
// Node must satisfy all of them (logical AND).
//
// SelectorTerms has the shape and semantics of a Pod's required node
// affinity: the terms are ORed, and the requirements within one term are
// ANDed.
type Match struct {
	NodeSelector      map[string]string         `json:"nodeSelector,omitempty"`
	SelectorTerms     []corev1.NodeSelectorTerm `json:"selectorTerms,omitempty"`
	ProviderIDPattern string                    `json:"providerIDPattern,omitempty"`
}

// ExternalIPRule adds ExternalIPs to matching Nodes' status.addresses. The
// result is a union across every matching rule: existing addresses (from
// any source) are preserved, and nothing is ever removed automatically.
type ExternalIPRule struct {
	Match
	ExternalIPs []string `json:"externalIPs,omitempty"`
}

// LabelRule authoritatively sets Labels on matching Nodes' metadata: a
// rule's value for a key overwrites whatever is already there. If two
// matching rules disagree on the value for the same key, that key is left
// untouched on both sides rather than fought over.
type LabelRule struct {
	Match
	Labels map[string]string `json:"labels,omitempty"`
}

// AnnotationRule is the annotations equivalent of LabelRule.
type AnnotationRule struct {
	Match
	Annotations map[string]string `json:"annotations,omitempty"`
}

// InitializationRule opts matching Nodes into initialization: Kubeling
// removes the node.cloudprovider.kubernetes.io/uninitialized taint the
// kubelet sets when started with --cloud-provider=external, doing what a
// cloud-controller-manager would do for Nodes no cloud-controller-manager
// takes care of. Nodes matching no rule are left alone.
//
// When ProviderIDScheme is set and a matching Node has no providerID yet,
// Kubeling also stamps "<ProviderIDScheme>://<node-name>" onto it.
type InitializationRule struct {
	Match
	ProviderIDScheme string `json:"providerIDScheme,omitempty"`
}

// Config is the schema of the "config.yaml" key inside the ConfigMap. Each
// of the four maps is independently keyed by an arbitrary rule ID and
// reconciled by its own controller.
type Config struct {
	Initialization map[string]InitializationRule `json:"initialization,omitempty"`
	ExternalIPs    map[string]ExternalIPRule     `json:"externalIPs,omitempty"`
	Labels         map[string]LabelRule          `json:"labels,omitempty"`
	Annotations    map[string]AnnotationRule     `json:"annotations,omitempty"`
}

// providerIDScheme is a URI scheme as defined by RFC 3986.
var providerIDScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// Validate reports an error if any rule's ProviderIDPattern doesn't
// compile as a regular expression, its SelectorTerms are malformed, or an
// initialization rule's ProviderIDScheme isn't a valid URI scheme.
func (c Config) Validate() error {
	for id, r := range c.Initialization {
		if err := validateMatch(r.Match); err != nil {
			return fmt.Errorf("initialization rule %q: %w", id, err)
		}
		if r.ProviderIDScheme != "" && !providerIDScheme.MatchString(r.ProviderIDScheme) {
			return fmt.Errorf("initialization rule %q: invalid providerIDScheme %q: must be a URI scheme like \"custom\"", id, r.ProviderIDScheme)
		}
	}
	for id, r := range c.ExternalIPs {
		if err := validateMatch(r.Match); err != nil {
			return fmt.Errorf("externalIPs rule %q: %w", id, err)
		}
	}
	for id, r := range c.Labels {
		if err := validateMatch(r.Match); err != nil {
			return fmt.Errorf("labels rule %q: %w", id, err)
		}
	}
	for id, r := range c.Annotations {
		if err := validateMatch(r.Match); err != nil {
			return fmt.Errorf("annotations rule %q: %w", id, err)
		}
	}
	return nil
}

func validateMatch(m Match) error {
	if m.ProviderIDPattern != "" {
		if _, err := regexp.Compile(m.ProviderIDPattern); err != nil {
			return fmt.Errorf("invalid providerIDPattern %q: %w", m.ProviderIDPattern, err)
		}
	}
	return validateSelectorTerms(m.SelectorTerms)
}

// validateSelectorTerms rejects terms the scheduler would accept but that
// can never do what was meant: an empty term (which silently matches
// nothing) and a matchFields key other than metadata.name (the only Node
// field that can be selected on, so any other key silently never matches).
// Everything else — operators, values, label keys — is checked by parsing
// the terms exactly as the scheduler does.
func validateSelectorTerms(terms []corev1.NodeSelectorTerm) error {
	if len(terms) == 0 {
		return nil
	}
	for i, term := range terms {
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			return fmt.Errorf("selectorTerms[%d]: term must set matchExpressions or matchFields", i)
		}
		for j, req := range term.MatchFields {
			if req.Key != "metadata.name" {
				return fmt.Errorf("selectorTerms[%d].matchFields[%d]: unsupported key %q, only metadata.name is supported", i, j, req.Key)
			}
		}
	}
	if _, err := nodeaffinity.NewNodeSelector(&corev1.NodeSelector{NodeSelectorTerms: terms}); err != nil {
		return fmt.Errorf("invalid selectorTerms: %w", err)
	}
	return nil
}
