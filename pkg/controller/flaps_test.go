package controller

import (
	"testing"
	"time"
)

func TestFlapDetector(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	f := newFlapDetector()
	f.now = func() time.Time { return now }

	t.Run("values that held longer than the flap window are restored right away", func(t *testing.T) {
		cooldown, flaps, appliedAt := f.drifted("node-1", start.Add(-flapWindow-time.Second))
		if cooldown != 0 || flaps != 0 || !appliedAt.Equal(start.Add(-flapWindow-time.Second)) {
			t.Errorf("drifted = %s, %d, %s; want no cooldown", cooldown, flaps, appliedAt)
		}
	})

	t.Run("flapping doubles the cooldown up to the maximum", func(t *testing.T) {
		want := []time.Duration{
			500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
			16 * time.Second, 32 * time.Second, 64 * time.Second, 64 * time.Second, 64 * time.Second,
		}
		for i, w := range want {
			f.applied("node-1", "v1")
			now = now.Add(time.Second)

			cooldown, flaps, appliedAt := f.drifted("node-1", time.Time{})
			if cooldown != w || flaps != i+1 || !appliedAt.Equal(now.Add(-time.Second)) {
				t.Fatalf("flap %d: drifted = %s, %d, %s; want %s", i+1, cooldown, flaps, appliedAt, w)
			}
			if got := f.remaining("node-1"); got != w {
				t.Errorf("flap %d: remaining = %s, want %s", i+1, got, w)
			}

			// Drifting again during the cooldown doesn't count as a flap.
			now = now.Add(w / 2)
			if cooldown, flaps, _ := f.drifted("node-1", time.Time{}); cooldown != w-w/2 || flaps != i+1 {
				t.Errorf("flap %d during cooldown: drifted = %s, %d", i+1, cooldown, flaps)
			}
			now = now.Add(w - w/2)
			if got := f.remaining("node-1"); got != 0 {
				t.Errorf("flap %d: remaining after cooldown = %s", i+1, got)
			}
		}
	})

	t.Run("a later confirmation counts as the last apply", func(t *testing.T) {
		f.applied("node-1", "v1")
		now = now.Add(flapWindow + time.Second)
		cooldown, _, _ := f.drifted("node-1", now.Add(-time.Second))
		if cooldown != 64*time.Second {
			t.Errorf("cooldown = %s, want a flap", cooldown)
		}
		now = now.Add(cooldown)
	})

	t.Run("holding values resets the streak", func(t *testing.T) {
		f.applied("node-1", "v1")
		now = now.Add(flapWindow + time.Second)
		if cooldown, flaps, _ := f.drifted("node-1", time.Time{}); cooldown != 0 || flaps != 0 {
			t.Fatalf("drifted = %s, %d; want reset", cooldown, flaps)
		}
		f.applied("node-1", "v1")
		now = now.Add(time.Second)
		if cooldown, flaps, _ := f.drifted("node-1", time.Time{}); cooldown != minCooldown || flaps != 1 {
			t.Errorf("drifted = %s, %d; want the minimum cooldown again", cooldown, flaps)
		}
	})

	t.Run("changed rules reset the streak", func(t *testing.T) {
		f.applied("node-1", "v1")
		now = now.Add(time.Second)
		if f.rulesChanged("node-1", "v1") {
			t.Fatal("same values reported as a rule change")
		}
		if cooldown, _, _ := f.drifted("node-1", time.Time{}); cooldown == 0 {
			t.Fatal("expected a cooldown")
		}
		if !f.rulesChanged("node-1", "v2") {
			t.Fatal("different values not reported as a rule change")
		}
		if got := f.remaining("node-1"); got != 0 {
			t.Errorf("remaining after rule change = %s, want 0", got)
		}
		if f.rulesChanged("node-1", "v3") || f.rulesChanged("unknown", "v1") {
			t.Error("nothing applied since, yet reported as a rule change")
		}
	})

	t.Run("forget drops the cooldown", func(t *testing.T) {
		f.forget("node-1")
		if got := f.remaining("node-1"); got != 0 {
			t.Errorf("remaining = %s, want 0", got)
		}
		if _, ok := f.nodes["node-1"]; ok {
			t.Error("state still tracked")
		}
	})

	t.Run("nodes are independent", func(t *testing.T) {
		f.applied("node-2", "v1")
		if cooldown, _, _ := f.drifted("node-2", time.Time{}); cooldown != minCooldown {
			t.Fatalf("cooldown = %s", cooldown)
		}
		if got := f.remaining("node-3"); got != 0 {
			t.Errorf("remaining for untouched node = %s", got)
		}
	})
}
