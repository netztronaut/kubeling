package controller

import (
	"reflect"
	"testing"

	"github.com/steigr/kubeling/pkg/config"
)

func TestApplyOverwrite(t *testing.T) {
	tests := []struct {
		name        string
		existing    map[string]string
		desired     map[string]string
		want        map[string]string
		wantChanged bool
	}{
		{
			name:        "no desired keys is a no-op",
			existing:    map[string]string{"a": "1"},
			desired:     nil,
			want:        map[string]string{"a": "1"},
			wantChanged: false,
		},
		{
			name:        "adds a missing key",
			existing:    map[string]string{"a": "1"},
			desired:     map[string]string{"b": "2"},
			want:        map[string]string{"a": "1", "b": "2"},
			wantChanged: true,
		},
		{
			name:        "overwrites a differing value",
			existing:    map[string]string{"a": "1"},
			desired:     map[string]string{"a": "2"},
			want:        map[string]string{"a": "2"},
			wantChanged: true,
		},
		{
			name:        "already matching is a no-op",
			existing:    map[string]string{"a": "1"},
			desired:     map[string]string{"a": "1"},
			want:        map[string]string{"a": "1"},
			wantChanged: false,
		},
		{
			name:        "nil existing map",
			existing:    nil,
			desired:     map[string]string{"a": "1"},
			want:        map[string]string{"a": "1"},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := applyOverwrite(tt.existing, tt.desired)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveValues(t *testing.T) {
	values := func(r config.LabelRule) map[string]string { return r.Labels }

	t.Run("agreeing rules both contribute", func(t *testing.T) {
		rules := map[string]config.LabelRule{
			"a": {Labels: map[string]string{"env": "prod"}},
			"b": {Labels: map[string]string{"rack": "r1"}},
		}
		got := resolveValues("labels", "node-1", []string{"a", "b"}, rules, values)
		want := map[string]string{"env": "prod", "rack": "r1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("agreeing on the same key is fine", func(t *testing.T) {
		rules := map[string]config.LabelRule{
			"a": {Labels: map[string]string{"env": "prod"}},
			"b": {Labels: map[string]string{"env": "prod"}},
		}
		got := resolveValues("labels", "node-1", []string{"a", "b"}, rules, values)
		want := map[string]string{"env": "prod"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("conflicting values drop the key entirely", func(t *testing.T) {
		rules := map[string]config.LabelRule{
			"a": {Labels: map[string]string{"env": "prod", "rack": "r1"}},
			"b": {Labels: map[string]string{"env": "staging"}},
		}
		got := resolveValues("labels", "node-1", []string{"a", "b"}, rules, values)
		want := map[string]string{"rack": "r1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
}
