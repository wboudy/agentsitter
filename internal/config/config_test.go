package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("a missing config file should not be an error: %v", err)
	}
	if cfg.PollInterval.Duration != 3*time.Second {
		t.Fatalf("PollInterval = %v, want 3s", cfg.PollInterval.Duration)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Label() != "local" {
		t.Fatalf("expected a single local target, got %+v", cfg.Targets)
	}
	if !cfg.Safety.Enabled {
		t.Fatal("safety veto should be on by default")
	}
}

func TestLoadParsesDurationsAndTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentsitter.toml")
	body := `
poll_interval = "1500ms"
capture_lines = 40
match_lines = 10
settle = "250ms"

[limits]
pane_cooldown = "30s"

[[targets]]
name = "workstation"
socket = "agents"

[[targets]]
ssh = "user@example.invalid"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval.Duration != 1500*time.Millisecond {
		t.Fatalf("PollInterval = %v", cfg.PollInterval.Duration)
	}
	if cfg.Limits.PaneCooldown.Duration != 30*time.Second {
		t.Fatalf("PaneCooldown = %v", cfg.Limits.PaneCooldown.Duration)
	}
	// Declaring targets replaces the default rather than appending to it.
	if len(cfg.Targets) != 2 {
		t.Fatalf("Targets = %d, want 2 (declared targets replace the default)", len(cfg.Targets))
	}
	if cfg.Targets[0].Label() != "workstation" || cfg.Targets[0].Socket != "agents" {
		t.Fatalf("unexpected first target: %+v", cfg.Targets[0])
	}
	if !cfg.Targets[1].Remote() || cfg.Targets[1].Label() != "user@example.invalid" {
		t.Fatalf("unexpected second target: %+v", cfg.Targets[1])
	}
	// An omitted socket means "discover them all". Assuming tmux's "default"
	// socket would silently watch nothing whenever agents run on a named one.
	if cfg.Targets[1].Socket != DiscoverSockets {
		t.Fatalf("Socket = %q, want %q", cfg.Targets[1].Socket, DiscoverSockets)
	}
	// An explicitly named socket is left alone.
	if cfg.Targets[0].Socket != "agents" {
		t.Fatalf("Socket = %q, want the declared name", cfg.Targets[0].Socket)
	}
	// Omitted command filters fall back to the agent-process defaults.
	if len(cfg.Targets[1].Commands) == 0 {
		t.Fatal("expected default command filters")
	}
}

func TestMatchLinesClampedToCaptureLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentsitter.toml")
	if err := os.WriteFile(path, []byte("capture_lines = 20\nmatch_lines = 999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MatchLines != 20 {
		t.Fatalf("MatchLines = %d, want it clamped to capture_lines (20)", cfg.MatchLines)
	}
}

func TestBadPatternIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentsitter.toml")
	if err := os.WriteFile(path, []byte("[safety]\nnever_match = [\"(unclosed\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "bad pattern") {
		t.Fatalf("expected a bad pattern error, got %v", err)
	}
}

func TestSafetyVeto(t *testing.T) {
	cfg := Default()
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}
	if re := cfg.Safety.Veto("Allow the agent to run `rm -rf /srv/data`?"); re == nil {
		t.Fatal("a recursive delete should be vetoed")
	}
	if re := cfg.Safety.Veto("Allow the agent to run `go test ./...`?"); re != nil {
		t.Fatalf("an ordinary command should not be vetoed, tripped %v", re)
	}

	cfg.Safety.Enabled = false
	if re := cfg.Safety.Veto("rm -rf /"); re != nil {
		t.Fatal("a disabled safety section should veto nothing")
	}
}

func TestTargetFilters(t *testing.T) {
	cfg := Config{Targets: []Target{{
		Sessions:     []string{`^agents$`},
		Commands:     []string{`^codex$`},
		ExcludePanes: []string{`^agents:9\.`},
	}}}
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}
	tg := cfg.Targets[0]
	if !tg.MatchesSession("agents") || tg.MatchesSession("other") {
		t.Fatal("session filter did not behave as expected")
	}
	if !tg.MatchesCommand("codex") || tg.MatchesCommand("bash") {
		t.Fatal("command filter did not behave as expected")
	}
	if !tg.Excluded("agents:9.1") || tg.Excluded("agents:1.1") {
		t.Fatal("pane exclusion did not behave as expected")
	}
}

func TestDefaultCommandsCoverVersionNamedBinaries(t *testing.T) {
	cfg := Config{Targets: []Target{{}}}
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}
	// Some agent CLIs run from a version-named path, so tmux reports the
	// process as a bare version number rather than a program name.
	if !cfg.Targets[0].MatchesCommand("2.1.243") {
		t.Fatal("default command filters should match a version-named agent binary")
	}
	// Orchestrators launch agents through wrappers, so the process name is
	// often the agent name with a suffix. An anchored match would watch
	// nothing here while still looking healthy.
	for _, wrapper := range []string{"codex-dispatch", "codex", "claude-wrapper", "claude"} {
		if !cfg.Targets[0].MatchesCommand(wrapper) {
			t.Errorf("default command filters should match %q", wrapper)
		}
	}
	for _, unrelated := range []string{"btop", "sshd:", "tailscaled", "bash", "zsh"} {
		if cfg.Targets[0].MatchesCommand(unrelated) {
			t.Errorf("default command filters should not match %q", unrelated)
		}
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}
	got, err := ExpandPath("~/x/y")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, "x", "y") {
		t.Fatalf("ExpandPath = %q", got)
	}
	for _, in := range []string{"", "/abs/path", "relative/path", "~someone/else"} {
		got, err := ExpandPath(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != in {
			t.Fatalf("ExpandPath(%q) = %q, want it unchanged", in, got)
		}
	}
}
