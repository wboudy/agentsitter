// Package guard bounds how often agentsitter is willing to press keys.
//
// The failure mode this exists to prevent is a rule that matches a prompt it
// cannot actually dismiss. Without limits that becomes an endless keypress
// loop against a live agent pane. With them it degrades into a logged
// complaint and a quarantined pane, which is recoverable.
package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// window is the span over which hourly rate limits are counted.
const window = time.Hour

// retention is how long an untouched pane record is kept before being dropped,
// which keeps the state file and the resident set bounded on a long-lived
// daemon.
const retention = 24 * time.Hour

// Limits mirrors the configured bounds.
type Limits struct {
	PaneCooldown      time.Duration
	PerRulePerHour    int
	GlobalPerHour     int
	MaxVerifyFailures int
	Quarantine        time.Duration
}

// PaneState is the per-pane bookkeeping agentsitter persists across restarts.
type PaneState struct {
	LastAction      time.Time              `json:"last_action,omitzero"`
	LastSeen        time.Time              `json:"last_seen,omitzero"`
	RuleHits        map[string][]time.Time `json:"rule_hits,omitempty"`
	Failures        int                    `json:"failures,omitempty"`
	QuarantineUntil time.Time              `json:"quarantine_until,omitzero"`
}

// State is the whole persisted picture.
type State struct {
	Panes  map[string]*PaneState `json:"panes,omitempty"`
	Global []time.Time           `json:"global,omitempty"`
}

// Guard enforces cooldowns, hourly caps, and quarantine.
type Guard struct {
	mu     sync.Mutex
	limits Limits
	path   string
	state  State
	dirty  bool
}

// Decision explains why an action was allowed or refused.
type Decision struct {
	Allowed bool
	Reason  string
}

// New creates a guard, loading persisted state from path when it exists. A
// missing or corrupt state file is not fatal: agentsitter starts with empty
// counters rather than refusing to run.
func New(limits Limits, path string) (*Guard, error) {
	g := &Guard{
		limits: limits,
		path:   path,
		state:  State{Panes: map[string]*PaneState{}},
	}
	if path == "" {
		return g, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return g, nil
	case err != nil:
		return g, fmt.Errorf("read state %s: %w", path, err)
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		return g, fmt.Errorf("parse state %s (starting fresh): %w", path, err)
	}
	if loaded.Panes == nil {
		loaded.Panes = map[string]*PaneState{}
	}
	g.state = loaded
	return g, nil
}

// pane returns the record for key, creating it if needed.
func (g *Guard) pane(key string) *PaneState {
	p, ok := g.state.Panes[key]
	if !ok {
		p = &PaneState{RuleHits: map[string][]time.Time{}}
		g.state.Panes[key] = p
	}
	if p.RuleHits == nil {
		p.RuleHits = map[string][]time.Time{}
	}
	return p
}

// Allow reports whether agentsitter may answer a prompt in this pane using this
// rule right now.
func (g *Guard) Allow(key, rule string, perRuleOverride int, cooldownOverride time.Duration, now time.Time) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()

	p := g.pane(key)
	p.LastSeen = now
	g.dirty = true

	if !p.QuarantineUntil.IsZero() && now.Before(p.QuarantineUntil) {
		return Decision{false, fmt.Sprintf("pane quarantined for another %s after %d failed attempts",
			now.Sub(p.QuarantineUntil).Abs().Truncate(time.Second), p.Failures)}
	}

	// A rule may shorten or lengthen the cooldown; an unset TOML duration
	// arrives as zero and means "use the global limit".
	cooldown := g.limits.PaneCooldown
	if cooldownOverride > 0 {
		cooldown = cooldownOverride
	}
	if !p.LastAction.IsZero() && now.Sub(p.LastAction) < cooldown {
		return Decision{false, fmt.Sprintf("pane cooldown, %s remaining",
			(cooldown - now.Sub(p.LastAction)).Truncate(time.Millisecond))}
	}

	perRule := g.limits.PerRulePerHour
	if perRuleOverride > 0 {
		perRule = perRuleOverride
	}
	if n := countSince(p.RuleHits[rule], now.Add(-window)); n >= perRule {
		return Decision{false, fmt.Sprintf("rule %q hit its cap of %d/hour on this pane", rule, perRule)}
	}
	if n := countSince(g.state.Global, now.Add(-window)); n >= g.limits.GlobalPerHour {
		return Decision{false, fmt.Sprintf("global cap of %d actions/hour reached", g.limits.GlobalPerHour)}
	}
	return Decision{Allowed: true}
}

// RecordAction notes that keys were sent.
func (g *Guard) RecordAction(key, rule string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	p := g.pane(key)
	p.LastAction = now
	p.LastSeen = now
	p.RuleHits[rule] = append(trimBefore(p.RuleHits[rule], now.Add(-window)), now)
	g.state.Global = append(trimBefore(g.state.Global, now.Add(-window)), now)
	g.dirty = true
}

// RecordSuccess clears the consecutive-failure counter for a pane.
func (g *Guard) RecordSuccess(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	p := g.pane(key)
	p.Failures = 0
	p.QuarantineUntil = time.Time{}
	g.dirty = true
}

// RecordFailure counts a verification failure and quarantines the pane once
// they accumulate. It returns true when this failure triggered a quarantine.
func (g *Guard) RecordFailure(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	p := g.pane(key)
	p.Failures++
	g.dirty = true
	if p.Failures >= g.limits.MaxVerifyFailures {
		p.QuarantineUntil = now.Add(g.limits.Quarantine)
		return true
	}
	return false
}

// Quarantined reports whether a pane is currently held back.
func (g *Guard) Quarantined(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.state.Panes[key]
	return ok && !p.QuarantineUntil.IsZero() && now.Before(p.QuarantineUntil)
}

// Release clears a quarantine and its failure count.
func (g *Guard) Release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.state.Panes[key]; ok {
		p.Failures = 0
		p.QuarantineUntil = time.Time{}
		g.dirty = true
	}
}

// Prune drops stale timestamps and pane records.
func (g *Guard) Prune(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	cutoff := now.Add(-window)
	g.state.Global = trimBefore(g.state.Global, cutoff)
	for key, p := range g.state.Panes {
		for rule, hits := range p.RuleHits {
			if kept := trimBefore(hits, cutoff); len(kept) == 0 {
				delete(p.RuleHits, rule)
			} else {
				p.RuleHits[rule] = kept
			}
		}
		idle := p.LastSeen.IsZero() || now.Sub(p.LastSeen) > retention
		quiet := len(p.RuleHits) == 0 && p.Failures == 0 &&
			(p.QuarantineUntil.IsZero() || now.After(p.QuarantineUntil))
		if idle && quiet {
			delete(g.state.Panes, key)
		}
	}
	g.dirty = true
}

// Snapshot returns a copy of the current state for reporting.
func (g *Guard) Snapshot() State {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := State{Panes: make(map[string]*PaneState, len(g.state.Panes))}
	out.Global = append(out.Global, g.state.Global...)
	for k, p := range g.state.Panes {
		cp := *p
		cp.RuleHits = make(map[string][]time.Time, len(p.RuleHits))
		for rule, hits := range p.RuleHits {
			cp.RuleHits[rule] = append([]time.Time(nil), hits...)
		}
		out.Panes[k] = &cp
	}
	return out
}

// ActionsInLastHour reports the global action count.
func (g *Guard) ActionsInLastHour(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return countSince(g.state.Global, now.Add(-window))
}

// Save writes state to disk atomically. It is a no-op when nothing changed.
func (g *Guard) Save() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.path == "" || !g.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(g.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, g.path); err != nil {
		return err
	}
	g.dirty = false
	return nil
}

// countSince counts timestamps at or after cutoff.
func countSince(ts []time.Time, cutoff time.Time) int {
	n := 0
	for _, t := range ts {
		if !t.Before(cutoff) {
			n++
		}
	}
	return n
}

// trimBefore drops timestamps older than cutoff, keeping the slice sorted.
func trimBefore(ts []time.Time, cutoff time.Time) []time.Time {
	if len(ts) == 0 {
		return ts
	}
	out := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
