package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		ref, wantNamespace, wantName string
	}{
		{ref: "kubeling-config", wantNamespace: "", wantName: "kubeling-config"},
		{ref: "kube-system/kubeling-config", wantNamespace: "kube-system", wantName: "kubeling-config"},
		{ref: "", wantNamespace: "", wantName: ""},
		{ref: "/kubeling-config", wantNamespace: "", wantName: "kubeling-config"},
		{ref: "kube-system/", wantNamespace: "kube-system", wantName: ""},
		{ref: "a/b/c", wantNamespace: "a", wantName: "b/c"},
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

func TestOwnNamespace(t *testing.T) {
	tests := []struct {
		name    string
		content string
		missing bool
		want    string
	}{
		{name: "missing file", missing: true, want: "default"},
		{name: "namespace", content: "kubeling", want: "kubeling"},
		{name: "surrounding whitespace", content: "  kube-system\n", want: "kube-system"},
		{name: "blank file", content: " \n", want: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "namespace")
			if !tt.missing {
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			original := serviceAccountNamespaceFile
			serviceAccountNamespaceFile = path
			t.Cleanup(func() { serviceAccountNamespaceFile = original })

			if got := OwnNamespace(); got != tt.want {
				t.Errorf("OwnNamespace() = %q, want %q", got, tt.want)
			}
		})
	}
}
