//go:build integration

// Package engine's integration test drives a real tmux server.
//
// Run it with: go test -tags integration ./internal/engine/
//
// It is tagged because it needs tmux on PATH and starts a server. It uses a
// private socket named after the test process, so it cannot touch any tmux
// session you are actually using.
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wboudy/agentsitter/internal/audit"
	"github.com/wboudy/agentsitter/internal/config"
	"github.com/wboudy/agentsitter/internal/guard"
	"github.com/wboudy/agentsitter/internal/rules"
)

// tmuxSocket returns a socket name private to this test process.
func tmuxSocket() string { return fmt.Sprintf("agentsitter-itest-%d", os.Getpid()) }

// startFakeAgent brings up a tmux session running the fake agent and returns
// the socket name.
func startFakeAgent(t *testing.T, initialSelection string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	script, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-agent.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}

	socket := tmuxSocket()
	cmd := exec.Command("tmux", "-L", socket, "new-session", "-d",
		"-s", "itest", "-x", "80", "-y", "24",
		"bash", script, initialSelection)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting tmux: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	// Wait for the menu to be drawn before handing the pane to agentsitter.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(capturePane(t, socket), "keep current model") {
			return socket
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("fake agent never rendered its menu:\n%s", capturePane(t, socket))
	return socket
}

func capturePane(t *testing.T, socket string) string {
	t.Helper()
	out, err := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", "itest").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// liveEngine builds an engine wired to the real tmux transport.
func liveEngine(t *testing.T, socket string) *Engine {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.StablePolls = 1
	cfg.Settle = config.Duration{Duration: 400 * time.Millisecond}
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuditFile = filepath.Join(dir, "audit.jsonl")
	cfg.LearnDir = filepath.Join(dir, "learn")
	cfg.PauseFile = filepath.Join(dir, "paused")
	cfg.Targets = []config.Target{{
		Name:     "itest",
		Socket:   socket,
		Commands: []string{`.`}, // the fixture runs under bash
	}}
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}

	rs, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	g, err := guard.New(guard.Limits{
		PaneCooldown:      0,
		PerRulePerHour:    10,
		GlobalPerHour:     100,
		MaxVerifyFailures: 3,
		Quarantine:        time.Minute,
	}, cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, rs, g, audit.NewLogger(cfg.AuditFile, cfg.LearnDir, ""), io.Discard)
}

// sweepUntil polls until an event appears or the deadline passes.
func sweepUntil(t *testing.T, e *Engine, timeout time.Duration) []audit.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if res := e.Sweep(context.Background()); len(res.Events) > 0 {
			return res.Events
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func TestIntegrationAnswersRealMenuThroughTmux(t *testing.T) {
	// The cursor starts on "Yes, switch models"; agentsitter must move it down
	// one row and submit, leaving the decline option chosen.
	socket := startFakeAgent(t, "0")
	e := liveEngine(t, socket)

	events := sweepUntil(t, e, 20*time.Second)
	if len(events) == 0 {
		t.Fatalf("agentsitter made no decision. pane was:\n%s", capturePane(t, socket))
	}
	ev := events[0]
	if ev.Outcome != audit.OutcomeAnswered {
		t.Fatalf("outcome = %s, reason = %s\npane:\n%s", ev.Outcome, ev.Reason, capturePane(t, socket))
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(capturePane(t, socket), "ANSWERED:") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	got := capturePane(t, socket)
	if !strings.Contains(got, "ANSWERED: No, keep current model") {
		t.Fatalf("agentsitter chose the wrong option. pane:\n%s", got)
	}
}

func TestIntegrationAlreadyOnCorrectOptionJustSubmits(t *testing.T) {
	// The decline option is already selected, so no arrow key should be needed.
	socket := startFakeAgent(t, "1")
	e := liveEngine(t, socket)

	if events := sweepUntil(t, e, 20*time.Second); len(events) == 0 || events[0].Outcome != audit.OutcomeAnswered {
		t.Fatalf("expected an answer, got %+v\npane:\n%s", events, capturePane(t, socket))
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(capturePane(t, socket), "ANSWERED:") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := capturePane(t, socket); !strings.Contains(got, "ANSWERED: No, keep current model") {
		t.Fatalf("wrong option chosen. pane:\n%s", got)
	}
}

func TestIntegrationDryRunLeavesMenuUntouched(t *testing.T) {
	socket := startFakeAgent(t, "0")
	e := liveEngine(t, socket)
	e.cfg.DryRun = true

	events := sweepUntil(t, e, 15*time.Second)
	if len(events) == 0 || events[0].Outcome != audit.OutcomeDryRun {
		t.Fatalf("expected a dry run decision, got %+v", events)
	}
	// Give the pane a moment to react, if it were going to.
	time.Sleep(time.Second)
	if got := capturePane(t, socket); strings.Contains(got, "ANSWERED:") {
		t.Fatalf("a dry run must not answer the menu. pane:\n%s", got)
	}
}
