package controller

import (
	"sync"
	"time"
)

const (
	// minCooldown is how long a rule controller waits before restoring a
	// Node's values the first time they flap; every further flap doubles it,
	// up to maxCooldown.
	minCooldown = 500 * time.Millisecond
	maxCooldown = 64 * time.Second
	// flapWindow is how long applied values have to hold for their next
	// drift not to count as a flap. Values that held longer reset the
	// cooldown back to none.
	flapWindow = 2 * maxCooldown
)

// flapDetector tracks, per Node, how often a rule controller's applied
// values were changed back by someone else shortly after being applied,
// and the resulting exponential cooldown before restoring them again. It
// keeps two controllers that disagree about a Node (e.g. a policy engine
// rewriting the labels Kubeling sets) from updating it in a tight loop.
type flapDetector struct {
	mu    sync.Mutex
	now   func() time.Time
	nodes map[string]*flapState
}

type flapState struct {
	flaps     int
	appliedAt time.Time
	until     time.Time
	// desired identifies the values last applied, to tell a rule change
	// apart from someone else changing the Node.
	desired string
}

func newFlapDetector() *flapDetector {
	return &flapDetector{now: time.Now, nodes: map[string]*flapState{}}
}

// applied records that node's values, identified by desired, were just
// written.
func (f *flapDetector) applied(node, desired string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.state(node)
	s.appliedAt = f.now()
	s.desired = desired
}

// rulesChanged reports whether node's values were last applied for
// different desired values, i.e. its rules changed since. It resets any
// flap streak and cooldown when they did, since restoring values nobody
// wants anymore is no flap. Nodes nothing was applied to since this process
// started report false.
func (f *flapDetector) rulesChanged(node, desired string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.nodes[node]
	if !ok || s.desired == "" || s.desired == desired {
		return false
	}
	*s = flapState{}
	return true
}

// drifted records that node's values no longer match its rules, after they
// were last confirmed applied at confirmedAt (e.g. an Applied condition's
// heartbeat; the later of it and the last recorded write counts). It
// returns how long to wait before restoring them (zero to restore right
// away), the number of consecutive flaps, and when the values were last
// applied. A drift during an ongoing cooldown just reports what is left of
// it.
func (f *flapDetector) drifted(node string, confirmedAt time.Time) (time.Duration, int, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	s := f.state(node)

	last := s.appliedAt
	if confirmedAt.After(last) {
		last = confirmedAt
	}
	if now.Before(s.until) {
		return s.until.Sub(now), s.flaps, last
	}
	if now.Sub(last) > flapWindow {
		s.flaps = 0
		return 0, 0, last
	}

	s.flaps++
	cooldown := maxCooldown
	// Bounding the shift keeps it from overflowing on long flap streaks.
	if shift := s.flaps - 1; shift < 16 && minCooldown<<shift < maxCooldown {
		cooldown = minCooldown << shift
	}
	s.until = now.Add(cooldown)
	return cooldown, s.flaps, last
}

// remaining returns how much of node's cooldown is left, zero if none.
func (f *flapDetector) remaining(node string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.nodes[node]; ok && f.now().Before(s.until) {
		return s.until.Sub(f.now())
	}
	return 0
}

// forget drops everything known about node, e.g. once no rule matches it
// anymore or it was deleted.
func (f *flapDetector) forget(node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes, node)
}

func (f *flapDetector) state(node string) *flapState {
	s, ok := f.nodes[node]
	if !ok {
		s = &flapState{}
		f.nodes[node] = s
	}
	return s
}
