package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLimits() Limits {
	return Limits{
		PaneCooldown:      5 * time.Second,
		PerRulePerHour:    3,
		GlobalPerHour:     10,
		MaxVerifyFailures: 3,
		Quarantine:        10 * time.Minute,
	}
}

func newGuard(t *testing.T) *Guard {
	t.Helper()
	g, err := New(testLimits(), "")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestCooldownBlocksRepeatAction(t *testing.T) {
	g := newGuard(t)
	now := time.Unix(1_700_000_000, 0)

	if d := g.Allow("local/%1", "r", 0, 0, now); !d.Allowed {
		t.Fatalf("first action should be allowed: %s", d.Reason)
	}
	g.RecordAction("local/%1", "r", now)

	if d := g.Allow("local/%1", "r", 0, 0, now.Add(2*time.Second)); d.Allowed {
		t.Fatal("a second action inside the cooldown should be refused")
	} else if !strings.Contains(d.Reason, "cooldown") {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}

	if d := g.Allow("local/%1", "r", 0, 0, now.Add(6*time.Second)); !d.Allowed {
		t.Fatalf("action after the cooldown should be allowed: %s", d.Reason)
	}
}

func TestCooldownOverride(t *testing.T) {
	g := newGuard(t)
	now := time.Unix(1_700_000_000, 0)
	g.RecordAction("local/%1", "r", now)

	// A rule-level override replaces the global cooldown.
	if d := g.Allow("local/%1", "r", 0, time.Second, now.Add(2*time.Second)); !d.Allowed {
		t.Fatalf("shorter override should allow the action: %s", d.Reason)
	}
	// Zero means "unset", so the global cooldown still applies.
	if d := g.Allow("local/%1", "r", 0, 0, now.Add(2*time.Second)); d.Allowed {
		t.Fatal("a zero override must fall back to the global cooldown")
	}
}

func TestPerRuleHourlyCap(t *testing.T) {
	g := newGuard(t)
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if d := g.Allow("local/%1", "r", 0, 0, at); !d.Allowed {
			t.Fatalf("action %d should be allowed: %s", i, d.Reason)
		}
		g.RecordAction("local/%1", "r", at)
	}

	at := base.Add(4 * time.Minute)
	if d := g.Allow("local/%1", "r", 0, 0, at); d.Allowed {
		t.Fatal("fourth action within the hour should hit the per-rule cap")
	} else if !strings.Contains(d.Reason, "cap of 3/hour") {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}

	// A different rule on the same pane has its own budget.
	if d := g.Allow("local/%1", "other", 0, 0, at); !d.Allowed {
		t.Fatalf("a different rule should have its own cap: %s", d.Reason)
	}
	// And the cap lifts once the hour has rolled past.
	if d := g.Allow("local/%1", "r", 0, 0, base.Add(90*time.Minute)); !d.Allowed {
		t.Fatalf("cap should lift after the window: %s", d.Reason)
	}
}

func TestGlobalHourlyCap(t *testing.T) {
	g := newGuard(t)
	base := time.Unix(1_700_000_000, 0)

	// Spread across panes and rules so only the global cap can bite.
	for i := 0; i < 10; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		g.RecordAction("local/%"+string(rune('a'+i)), "rule", at)
	}
	at := base.Add(20 * time.Minute)
	d := g.Allow("local/%zz", "rule", 0, 0, at)
	if d.Allowed {
		t.Fatal("global cap should refuse the eleventh action")
	}
	if !strings.Contains(d.Reason, "global cap") {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}
	if got := g.ActionsInLastHour(at); got != 10 {
		t.Fatalf("ActionsInLastHour = %d, want 10", got)
	}
}

func TestQuarantineAfterRepeatedFailures(t *testing.T) {
	g := newGuard(t)
	now := time.Unix(1_700_000_000, 0)

	if g.RecordFailure("local/%1", now) || g.RecordFailure("local/%1", now) {
		t.Fatal("quarantine should not trigger before the failure threshold")
	}
	if !g.RecordFailure("local/%1", now) {
		t.Fatal("third failure should trigger quarantine")
	}
	if !g.Quarantined("local/%1", now) {
		t.Fatal("pane should be quarantined")
	}
	if d := g.Allow("local/%1", "r", 0, 0, now); d.Allowed {
		t.Fatal("a quarantined pane must not be acted on")
	} else if !strings.Contains(d.Reason, "quarantined") {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}

	// Quarantine expires on its own.
	if g.Quarantined("local/%1", now.Add(11*time.Minute)) {
		t.Fatal("quarantine should expire")
	}
	// A success clears the counter so failures do not accumulate forever.
	g.RecordSuccess("local/%1")
	if g.RecordFailure("local/%1", now) {
		t.Fatal("failure count should have been reset by the success")
	}
}

func TestReleaseClearsQuarantine(t *testing.T) {
	g := newGuard(t)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 3; i++ {
		g.RecordFailure("local/%1", now)
	}
	g.Release("local/%1")
	if g.Quarantined("local/%1", now) {
		t.Fatal("Release should clear the quarantine")
	}
}

func TestPruneDropsStaleRecords(t *testing.T) {
	g := newGuard(t)
	base := time.Unix(1_700_000_000, 0)

	g.RecordAction("local/%old", "r", base)
	g.RecordAction("local/%new", "r", base.Add(48*time.Hour))
	g.Prune(base.Add(49 * time.Hour))

	snap := g.Snapshot()
	if _, ok := snap.Panes["local/%old"]; ok {
		t.Fatal("a pane untouched for longer than retention should be dropped")
	}
	if _, ok := snap.Panes["local/%new"]; !ok {
		t.Fatal("a recently active pane should be kept")
	}
	if len(snap.Global) != 1 {
		t.Fatalf("global timestamps = %d, want the stale one pruned", len(snap.Global))
	}
}

func TestSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	g, err := New(testLimits(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	g.RecordAction("local/%1", "r", now)
	for i := 0; i < 3; i++ {
		g.RecordFailure("local/%1", now)
	}
	if err := g.Save(); err != nil {
		t.Fatal(err)
	}

	// Limits survive a restart, so a pane cannot escape quarantine by
	// bouncing the daemon.
	again, err := New(testLimits(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Quarantined("local/%1", now.Add(time.Minute)) {
		t.Fatal("quarantine should persist across a restart")
	}
	if d := again.Allow("local/%1", "r", 0, 0, now.Add(time.Second)); d.Allowed {
		t.Fatal("cooldown should persist across a restart")
	}
}

func TestCorruptStateFileStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := New(testLimits(), path)
	if err == nil {
		t.Fatal("a corrupt state file should be reported")
	}
	if g == nil {
		t.Fatal("a usable guard should still be returned")
	}
	if d := g.Allow("local/%1", "r", 0, 0, time.Unix(1_700_000_000, 0)); !d.Allowed {
		t.Fatalf("guard should work with empty counters: %s", d.Reason)
	}
}

func TestMissingStateFileIsNotAnError(t *testing.T) {
	if _, err := New(testLimits(), filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("a missing state file should not be an error: %v", err)
	}
}
