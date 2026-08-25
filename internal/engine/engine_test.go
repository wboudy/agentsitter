package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wboudy/agentsitter/internal/audit"
	"github.com/wboudy/agentsitter/internal/config"
	"github.com/wboudy/agentsitter/internal/guard"
	"github.com/wboudy/agentsitter/internal/rules"
	"github.com/wboudy/agentsitter/internal/tmuxio"
)

const esc = "\x1b"

// hl wraps a line in reverse video, which is how a TUI marks the selection.
func hl(s string) string { return esc + "[7m" + s + esc + "[0m" }

// fakeClient serves scripted screens and records every key sent.
type fakeClient struct {
	panes    []tmuxio.Pane
	screens  []string // consumed in order; the last one repeats
	captures int
	sent     [][]string
	sendErr  error
}

func (f *fakeClient) ListPanes(context.Context) ([]tmuxio.Pane, error) { return f.panes, nil }

func (f *fakeClient) Capture(_ context.Context, _ string, _ int) (string, error) {
	i := f.captures
	f.captures++
	if i >= len(f.screens) {
		i = len(f.screens) - 1
	}
	return f.screens[i], nil
}

func (f *fakeClient) SendKeys(_ context.Context, _ string, keys ...string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, append([]string(nil), keys...))
	return nil
}

// allKeys flattens every keypress into one slice, in order.
func (f *fakeClient) allKeys() []string {
	var out []string
	for _, batch := range f.sent {
		out = append(out, batch...)
	}
	return out
}

type harness struct {
	engine *Engine
	client *fakeClient
	dir    string
}

// newHarness wires an engine to a fake tmux with a single codex pane.
func newHarness(t *testing.T, screens []string, mutate func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.StablePolls = 1 // one capture is enough in tests
	cfg.Settle = config.Duration{Duration: 0}
	cfg.PauseFile = filepath.Join(dir, "paused")
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuditFile = filepath.Join(dir, "audit.jsonl")
	cfg.LearnDir = filepath.Join(dir, "learn")
	if mutate != nil {
		mutate(&cfg)
	}
	// Re-run validation and pattern compilation after mutation.
	cfgPath := filepath.Join(dir, "agentsitter.toml")
	if err := os.WriteFile(cfgPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded.StablePolls = cfg.StablePolls
	loaded.Settle = cfg.Settle
	loaded.PauseFile = cfg.PauseFile
	loaded.StateFile = cfg.StateFile
	loaded.AuditFile = cfg.AuditFile
	loaded.LearnDir = cfg.LearnDir
	loaded.DryRun = cfg.DryRun
	loaded.Learn = cfg.Learn
	loaded.Safety.Enabled = cfg.Safety.Enabled
	cfg = loaded

	rs, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	g, err := guard.New(guard.Limits{
		PaneCooldown:      cfg.Limits.PaneCooldown.Duration,
		PerRulePerHour:    cfg.Limits.PerRulePerHour,
		GlobalPerHour:     cfg.Limits.GlobalPerHour,
		MaxVerifyFailures: cfg.Limits.MaxVerifyFailures,
		Quarantine:        cfg.Limits.Quarantine.Duration,
	}, cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{
		panes: []tmuxio.Pane{{
			ID: "%1", Session: "agents", Window: "1", Index: "2", Command: "codex",
		}},
		screens: screens,
	}

	lg := audit.NewLogger(cfg.AuditFile, cfg.LearnDir, "")
	e := New(cfg, rs, g, lg, io.Discard)
	e.NewClient = func(config.Target) PaneClient { return client }
	e.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	return &harness{engine: e, client: client, dir: dir}
}

// outcomes lists the outcomes of a sweep.
func outcomes(res Result) []audit.Outcome {
	var out []audit.Outcome
	for _, ev := range res.Events {
		out = append(out, ev.Outcome)
	}
	return out
}

func firstEvent(t *testing.T, res Result) audit.Event {
	t.Helper()
	if len(res.Events) == 0 {
		t.Fatal("expected at least one event")
	}
	return res.Events[0]
}

// downgradeMenu is a usage-limit prompt with the decline option one row below
// the highlighted entry.
func downgradeMenu(selected int) string {
	lines := []string{
		"You've hit your usage limit for the current model.",
		"",
		"  Yes, switch models",
		"  No, keep current model",
	}
	lines[2+selected] = hl(strings.TrimSpace(lines[2+selected]))
	return strings.Join(lines, "\n")
}

func TestAnswersDowngradePromptByDecliningIt(t *testing.T) {
	// Screen 0: initial read, highlight on "Yes, switch models".
	// Screen 1: confirmation read after moving, highlight on the decline line.
	// Screen 2: after submitting, the prompt is gone.
	h := newHarness(t, []string{
		downgradeMenu(0),
		downgradeMenu(1),
		"the agent carried on working",
	}, nil)

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeAnswered {
		t.Fatalf("outcome = %s, reason = %s", ev.Outcome, ev.Reason)
	}
	if ev.Rule != "decline-model-downgrade" {
		t.Fatalf("rule = %s", ev.Rule)
	}
	if !strings.Contains(ev.Option, "No, keep current model") {
		t.Fatalf("chose option %q, want the decline line", ev.Option)
	}
	if got := h.client.allKeys(); strings.Join(got, ",") != "Down,Enter" {
		t.Fatalf("keys sent = %v, want one Down then Enter", got)
	}
}

func TestNeverSubmitsWhenHighlightLandsElsewhere(t *testing.T) {
	// The confirmation read still shows the highlight on the wrong entry, so
	// the attempt must be abandoned without pressing Enter.
	h := newHarness(t, []string{
		downgradeMenu(0),
		downgradeMenu(0),
	}, nil)

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeAborted {
		t.Fatalf("outcome = %s, want aborted (reason: %s)", ev.Outcome, ev.Reason)
	}
	for _, k := range h.client.allKeys() {
		if k == "Enter" {
			t.Fatal("Enter must never be sent when the highlight did not land on the intended option")
		}
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	h := newHarness(t, []string{downgradeMenu(0)}, func(c *config.Config) {
		c.DryRun = true
	})

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeDryRun {
		t.Fatalf("outcome = %s", ev.Outcome)
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("dry run sent keys: %v", h.client.sent)
	}
	if ev.Option == "" {
		t.Fatal("a dry run should still report the option it would have chosen")
	}
}

func TestPauseFileSuppressesKeys(t *testing.T) {
	h := newHarness(t, []string{downgradeMenu(0)}, nil)
	if err := os.WriteFile(h.engine.cfg.PauseFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeDryRun || ev.Reason != "paused" {
		t.Fatalf("outcome = %s, reason = %q, want a paused dry run", ev.Outcome, ev.Reason)
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("paused engine sent keys: %v", h.client.sent)
	}
}

func TestSafetyVetoBlocksDestructivePrompt(t *testing.T) {
	menu := strings.Join([]string{
		"Allow the agent to run `rm -rf /srv/data`?",
		hl("Yes"),
		"  No",
	}, "\n")
	h := newHarness(t, []string{menu}, nil)

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeVetoed {
		t.Fatalf("outcome = %s, want vetoed", ev.Outcome)
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("a vetoed prompt must not be answered, sent: %v", h.client.sent)
	}
	if !ev.Notable() {
		t.Fatal("a veto should be notable enough to notify about")
	}
}

func TestSafetyVetoCanBeDisabled(t *testing.T) {
	menu := strings.Join([]string{
		"Allow the agent to run `rm -rf /srv/data`?",
		hl("Yes"),
		"  No",
	}, "\n")
	h := newHarness(t, []string{menu, menu, "done"}, func(c *config.Config) {
		c.Safety.Enabled = false
	})

	res := h.engine.Sweep(context.Background())
	if got := firstEvent(t, res).Outcome; got == audit.OutcomeVetoed {
		t.Fatal("veto should not apply when safety is disabled")
	}
}

func TestUnresolvedWhenNoPreferredOptionPresent(t *testing.T) {
	// The prompt is recognised but offers no decline option at all.
	menu := strings.Join([]string{
		"You've hit your usage limit for the current model.",
		hl("Switch models"),
		"  Buy more credits",
	}, "\n")
	h := newHarness(t, []string{menu}, nil)

	res := h.engine.Sweep(context.Background())

	var sawUnresolved bool
	for _, ev := range res.Events {
		if ev.Outcome == audit.OutcomeUnresolved {
			sawUnresolved = true
		}
		if ev.Outcome == audit.OutcomeAnswered {
			t.Fatalf("must not answer a prompt with no acceptable option (chose %q)", ev.Option)
		}
	}
	if !sawUnresolved {
		t.Fatalf("expected an unresolved outcome, got %v", outcomes(res))
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("nothing should be sent, got %v", h.client.sent)
	}
}

func TestVerifyFailureWhenPromptRemains(t *testing.T) {
	// The prompt is still on screen after answering.
	h := newHarness(t, []string{
		downgradeMenu(0),
		downgradeMenu(1),
		downgradeMenu(1),
	}, nil)

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeVerifyFailed {
		t.Fatalf("outcome = %s, want verify_failed", ev.Outcome)
	}
	if !ev.Notable() {
		t.Fatal("a verification failure should be notable")
	}
}

func TestNumberedMenuAnsweredByDigit(t *testing.T) {
	// No highlight anywhere, but the options are numbered.
	menu := strings.Join([]string{
		"You've hit your usage limit for the current model.",
		"1. Yes, switch models",
		"2. No, keep current model",
	}, "\n")
	// Only two reads here: the initial one and the after-check. A numbered
	// answer needs no confirmation read, because there is no cursor to verify.
	h := newHarness(t, []string{menu, "carried on"}, nil)

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeAnswered {
		t.Fatalf("outcome = %s, reason = %s", ev.Outcome, ev.Reason)
	}
	if got := h.client.allKeys(); strings.Join(got, ",") != "2" {
		t.Fatalf("keys = %v, want the option number alone", got)
	}
	if h.client.captures != 2 {
		t.Fatalf("captures = %d, want 2", h.client.captures)
	}
}

func TestNoHighlightAndNoNumbersIsUnresolved(t *testing.T) {
	menu := strings.Join([]string{
		"You've hit your usage limit for the current model.",
		"Yes, switch models",
		"No, keep current model",
	}, "\n")
	h := newHarness(t, []string{menu}, nil)

	res := h.engine.Sweep(context.Background())
	for _, ev := range res.Events {
		if ev.Outcome == audit.OutcomeAnswered {
			t.Fatal("must not guess when nothing is highlighted and nothing is numbered")
		}
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("nothing should be sent, got %v", h.client.sent)
	}
}

func TestUnsettledPaneIsNotActedOn(t *testing.T) {
	h := newHarness(t, []string{downgradeMenu(0), downgradeMenu(1), "x"}, func(c *config.Config) {
		c.StablePolls = 2
	})
	h.engine.cfg.StablePolls = 2

	res := h.engine.Sweep(context.Background())
	if res.Evaluated != 0 {
		t.Fatal("a pane seen only once must not be evaluated when stable_polls is 2")
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("nothing should be sent, got %v", h.client.sent)
	}
}

func TestCooldownThrottlesSecondAnswer(t *testing.T) {
	h := newHarness(t, []string{
		downgradeMenu(0), downgradeMenu(1), "gone",
	}, nil)

	if got := firstEvent(t, h.engine.Sweep(context.Background())).Outcome; got != audit.OutcomeAnswered {
		t.Fatalf("first sweep outcome = %s", got)
	}

	// The prompt comes back immediately, inside the pane cooldown.
	h.client.screens = []string{downgradeMenu(0)}
	h.client.captures = 0
	res := h.engine.Sweep(context.Background())
	if got := firstEvent(t, res).Outcome; got != audit.OutcomeThrottled {
		t.Fatalf("second sweep outcome = %s, want throttled", got)
	}
}

func TestUnknownPromptIsRecordedForLearning(t *testing.T) {
	menu := strings.Join([]string{
		"Some dialog agentsitter has never seen before",
		hl("❯ Option one"),
		"  Option two",
	}, "\n")
	h := newHarness(t, []string{menu}, nil)

	res := h.engine.Sweep(context.Background())
	ev := firstEvent(t, res)

	if ev.Outcome != audit.OutcomeUnknownPrompt {
		t.Fatalf("outcome = %s, want unknown_prompt", ev.Outcome)
	}
	if ev.Capture == "" {
		t.Fatal("expected a learn capture path")
	}
	body, err := os.ReadFile(ev.Capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Option one") {
		t.Fatal("learn capture should contain the screen text")
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("an unknown prompt must not be answered, sent: %v", h.client.sent)
	}
}

func TestOrdinaryOutputIsIgnored(t *testing.T) {
	h := newHarness(t, []string{
		"  - Packaged self-test: PASS\n\n› Ask the agent to do anything\n\n  Context 24% used",
	}, nil)

	res := h.engine.Sweep(context.Background())
	if len(res.Events) != 0 {
		t.Fatalf("idle pane produced events: %v", outcomes(res))
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("idle pane triggered keys: %v", h.client.sent)
	}
}

func TestPaneFiltersSkipNonAgentPanes(t *testing.T) {
	h := newHarness(t, []string{downgradeMenu(0), downgradeMenu(1), "x"}, nil)
	h.client.panes = []tmuxio.Pane{
		{ID: "%1", Session: "agents", Window: "1", Index: "1", Command: "zsh"},
		{ID: "%2", Session: "agents", Window: "1", Index: "2", Command: "btop"},
		{ID: "%3", Session: "agents", Window: "1", Index: "3", Command: "codex", Dead: true},
		{ID: "%4", Session: "agents", Window: "1", Index: "4", Command: "codex", InMode: true},
	}

	res := h.engine.Sweep(context.Background())
	if res.Watched != 0 {
		t.Fatalf("watched %d panes, want 0 (shell, monitor, dead, and copy-mode panes are all skipped)", res.Watched)
	}
}

func TestAuditLogRecordsEveryDecision(t *testing.T) {
	h := newHarness(t, []string{downgradeMenu(0), downgradeMenu(1), "gone"}, nil)
	h.engine.Sweep(context.Background())

	data, err := os.ReadFile(h.engine.cfg.AuditFile)
	if err != nil {
		t.Fatalf("audit log should exist: %v", err)
	}
	line := strings.TrimSpace(string(data))
	for _, want := range []string{`"outcome":"answered"`, `"rule":"decline-model-downgrade"`, `"pane_addr":"agents:1.2"`} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line missing %s:\n%s", want, line)
		}
	}
}
