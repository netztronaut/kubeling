// Package config loads the cloud-controller-manager's policy configuration
// from a ConfigMap, read via the Kubernetes API rather than a mounted
// volume.
package config

import (
	"fmt"
	"regexp"
)

// Policy describes a set of Nodes, matched via NodeSelector, and the
// externalIPs, labels and annotations that should be present on those
// Nodes. Labels and annotations are authoritative: the policy's value wins
// over whatever is already on the Node. If two policies disagree on the
// value for the same key on the same Node, that key is left untouched on
// both sides rather than fought over.
type Policy struct {
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	ExternalIPs  []string          `json:"externalIPs,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// OnboardingRule matches Nodes by regular expression against
// spec.providerID. A Node whose providerID matches ProviderIDPattern gets
// Label=LabelValue applied once, and its onboarding NodeCondition set to
// Onboarded. Unlike Policy labels, this is a one-time stamp: once applied,
// it is never removed or re-applied, even if the rule is later changed or
// removed.
type OnboardingRule struct {
	ProviderIDPattern string `json:"providerIDPattern"`
	Label             string `json:"label"`
	LabelValue        string `json:"labelValue"`
}

// Config is the schema of the "config.yaml" key inside the policy
// ConfigMap.
type Config struct {
	Policies   map[string]Policy         `json:"policies,omitempty"`
	Onboarding map[string]OnboardingRule `json:"onboarding,omitempty"`
}

// Validate reports an error if any OnboardingRule's ProviderIDPattern
// doesn't compile as a regular expression.
func (c Config) Validate() error {
	for id, rule := range c.Onboarding {
		if _, err := regexp.Compile(rule.ProviderIDPattern); err != nil {
			return fmt.Errorf("onboarding rule %q: invalid providerIDPattern %q: %w", id, rule.ProviderIDPattern, err)
		}
	}
	return nil
}
