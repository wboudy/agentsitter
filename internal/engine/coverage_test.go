package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/wboudy/agentsitter/internal/audit"
)

// This file is the coverage record for the built-in ruleset: one fixture per
// real dialog, each asserting not just that the prompt is recognized but that
// the intended option is the one chosen.
//
// The Codex fixtures are copied verbatim from the rendered-terminal snapshot
// tests in the openai/codex repository, so they are what the TUI actually
// draws. The capacity fixture was captured from a live agent. The Claude Code
// fixture is assembled from strings found in the shipped binary.
//
// Adding a dialog here is the way to extend coverage: paste the screen, state
// the answer you want, and the test will tell you whether the ruleset agrees.

type dialogCase struct {
	name string
	// screen is what the pane shows.
	screen string
	// after is what the pane shows once the prompt has been answered. Codex
	// menus carry no highlight, so they are answered by pressing the option
	// number and need no confirmation read; menus that do carry a highlight
	// need one, and say so with needsConfirmRead.
	after            string
	needsConfirmRead bool

	wantRule   string
	wantOption string
	wantKeys   string
}

func codexDialogs() []dialogCase {
	return []dialogCase{
		{
			name: "exec approval",
			screen: `  Would you like to run the following command?

  Reason: this is a test reason such as one that would be produced by the model

  $ echo hello world

› 1. Yes, proceed (y)
  2. Yes, and don't ask again for commands that start with ` + "`echo hello world`" + ` (p)
  3. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel`,
			after:      "  the command ran",
			wantRule:   "codex-approve-command",
			wantOption: "Yes, proceed",
			wantKeys:   "1",
		},
		{
			name: "patch approval",
			screen: `  Would you like to make the following edits?

  Description: The model wants to apply changes
  Destination: /tmp/project/README.md

› 1. Yes, proceed (y)
  2. Yes, and don't ask again for these files (a)
  3. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel`,
			after:      "  the edits were applied",
			wantRule:   "codex-approve-edits",
			wantOption: "Yes, proceed",
			wantKeys:   "1",
		},
		{
			name: "permission grant",
			screen: `  Would you like to grant these permissions?

  Reason: need workspace access

  Permission rule: network; read ` + "`/tmp/readme.txt`" + `; write ` + "`/tmp/out.txt`" + `

› 1. Yes, grant these permissions for this turn (y)
  2. Yes, grant for this turn with strict auto review (r)
  3. Yes, grant these permissions for this session (a)
  4. No, continue without permissions (d)

  Press enter to confirm or esc to cancel`,
			after:      "  permissions granted",
			wantRule:   "codex-grant-permissions",
			wantOption: "for this turn",
			wantKeys:   "1",
		},
		{
			name: "network access",
			screen: `  Do you want to approve network access to "example.com"?

  Reason: network request blocked

› 1. Yes, just this once (y)
  2. Yes, and allow this host for this conversation (a)
  3. Yes, and allow this host in the future (p)
  4. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel`,
			after:      "  request allowed",
			wantRule:   "approve-network-access",
			wantOption: "just this once",
			wantKeys:   "1",
		},
		{
			name: "update offer is declined",
			screen: `  ✨ New version available! Would you like to update?
  Full release notes: https://example.invalid/releases/latest
› 1. Yes, update now
  2. No, not now
  3. Don't remind me

  Press enter to confirm or esc to go back`,
			after:      "  carrying on",
			wantRule:   "codex-decline-update",
			wantOption: "No, not now",
			wantKeys:   "2",
		},
		{
			name: "capacity pressure keeps the good model",
			screen: `  Our systems are thinking a bit more about this request before
  responding.
  Hang tight or retry with a faster model for a quicker response, though
  it may be less capable of handling complex requests.

› 1. Retry with a faster model
  2. Dismiss and keep waiting
  3. Learn more

  No action is required. Codex will keep waiting, and this menu will close
  when the response is ready.`,
			after:      "  still working",
			wantRule:   "prefer-keep-waiting",
			wantOption: "Dismiss and keep waiting",
			wantKeys:   "2",
		},
		{
			name: "claude code permission prompt",
			screen: `Do you want to proceed?

❯ 1. Yes
  2. Yes, and don't ask again for Bash commands
  3. No, and tell Claude what to do differently (esc)`,
			after:            "  proceeding",
			needsConfirmRead: true,
			wantRule:         "confirm-proceed",
			wantOption:       "Yes",
			wantKeys:         "Enter",
		},
	}
}

func TestCoverageOfKnownDialogs(t *testing.T) {
	for _, c := range codexDialogs() {
		t.Run(c.name, func(t *testing.T) {
			screens := []string{c.screen, c.after}
			if c.needsConfirmRead {
				// One extra read: the menu is still up while the highlight is
				// verified, and only then is the submit key sent.
				screens = []string{c.screen, c.screen, c.after}
			}
			h := newHarness(t, screens, nil)

			res := h.engine.Sweep(context.Background())
			ev := firstEvent(t, res)

			if ev.Outcome != audit.OutcomeAnswered {
				t.Fatalf("outcome = %s (rule %s, reason %q)", ev.Outcome, ev.Rule, ev.Reason)
			}
			if ev.Rule != c.wantRule {
				t.Errorf("rule = %q, want %q", ev.Rule, c.wantRule)
			}
			if !strings.Contains(ev.Option, c.wantOption) {
				t.Errorf("chose %q, want it to contain %q", ev.Option, c.wantOption)
			}
			if got := strings.Join(h.client.allKeys(), ","); got != c.wantKeys {
				t.Errorf("keys = %q, want %q", got, c.wantKeys)
			}
		})
	}
}

// TestDestructiveApprovalsAreStillVetoed makes sure widening the ruleset did
// not open a path around the safety list. The dialog is recognized and would
// otherwise be approved; the veto has to win.
func TestDestructiveApprovalsAreStillVetoed(t *testing.T) {
	screen := `  Would you like to run the following command?

  $ rm -rf /srv/data

› 1. Yes, proceed (y)
  2. Yes, and don't ask again for commands that start with ` + "`rm`" + ` (p)
  3. No, and tell Codex what to do differently (esc)`

	h := newHarness(t, []string{screen, "unchanged"}, nil)
	ev := firstEvent(t, h.engine.Sweep(context.Background()))

	if ev.Outcome != audit.OutcomeVetoed {
		t.Fatalf("outcome = %s, want vetoed", ev.Outcome)
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("a vetoed approval must send nothing, sent %v", h.client.sent)
	}
}

// TestNarrowestOptionPreferred pins the policy that an unattended answer does
// not widen permissions further than the prompt requires.
func TestNarrowestOptionPreferred(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		reject string
	}{
		{
			name: "does not take a permanent command exemption",
			screen: `  Would you like to run the following command?

  $ go test ./...

› 1. Yes, proceed (y)
  2. Yes, and don't ask again for commands that start with ` + "`go`" + ` (p)`,
			reject: "don't ask again",
		},
		{
			name: "does not take a session-wide permission grant",
			screen: `  Would you like to grant these permissions?

› 1. Yes, grant these permissions for this turn (y)
  2. Yes, grant these permissions for this session (a)`,
			reject: "for this session",
		},
		{
			name: "does not allow a host indefinitely",
			screen: `  Do you want to approve network access to "example.com"?

› 1. Yes, just this once (y)
  2. Yes, and allow this host in the future (p)`,
			reject: "in the future",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, []string{c.screen, "done"}, nil)
			ev := firstEvent(t, h.engine.Sweep(context.Background()))
			if ev.Outcome != audit.OutcomeAnswered {
				t.Fatalf("outcome = %s, reason %q", ev.Outcome, ev.Reason)
			}
			if strings.Contains(strings.ToLower(ev.Option), c.reject) {
				t.Fatalf("chose the broader option %q", ev.Option)
			}
		})
	}
}
