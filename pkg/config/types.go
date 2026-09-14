// Package config loads the cloud-controller-manager's rule configuration
// from a ConfigMap, read via the Kubernetes API rather than a mounted
// volume.
package config

import (
	"fmt"
	"regexp"
)

// Match describes which Nodes a rule applies to. Both constraints are
// optional; an unset one imposes no restriction. When both are set, a Node
// must satisfy both (logical AND).
type Match struct {
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	ProviderIDPattern string            `json:"providerIDPattern,omitempty"`
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

// Config is the schema of the "config.yaml" key inside the ConfigMap. Each
// of the three maps is independently keyed by an arbitrary rule ID and
// reconciled by its own controller.
type Config struct {
	ExternalIPs map[string]ExternalIPRule `json:"externalIPs,omitempty"`
	Labels      map[string]LabelRule      `json:"labels,omitempty"`
	Annotations map[string]AnnotationRule `json:"annotations,omitempty"`
}

// Validate reports an error if any rule's ProviderIDPattern doesn't
// compile as a regular expression.
func (c Config) Validate() error {
	for id, r := range c.ExternalIPs {
		if err := validatePattern(r.ProviderIDPattern); err != nil {
			return fmt.Errorf("externalIPs rule %q: %w", id, err)
		}
	}
	for id, r := range c.Labels {
		if err := validatePattern(r.ProviderIDPattern); err != nil {
			return fmt.Errorf("labels rule %q: %w", id, err)
		}
	}
	for id, r := range c.Annotations {
		if err := validatePattern(r.ProviderIDPattern); err != nil {
			return fmt.Errorf("annotations rule %q: %w", id, err)
		}
	}
	return nil
}

func validatePattern(pattern string) error {
	if pattern == "" {
		return nil
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("invalid providerIDPattern %q: %w", pattern, err)
	}
	return nil
}
