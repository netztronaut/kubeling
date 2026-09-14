package config

import "testing"

func TestParseRef(t *testing.T) {
	tests := []struct {
		ref, wantNamespace, wantName string
	}{
		{ref: "kubeling-config", wantNamespace: "", wantName: "kubeling-config"},
		{ref: "kube-system/kubeling-config", wantNamespace: "kube-system", wantName: "kubeling-config"},
		{ref: "", wantNamespace: "", wantName: ""},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			namespace, name := ParseRef(tt.ref)
			if namespace != tt.wantNamespace || name != tt.wantName {
				t.Errorf("ParseRef(%q) = (%q, %q), want (%q, %q)", tt.ref, namespace, name, tt.wantNamespace, tt.wantName)
			}
		})
	}
}
