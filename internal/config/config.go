// Package config loads agentsitter's TOML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration so it can be written as "3s" in TOML.
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Config is the whole of agentsitter's runtime configuration.
type Config struct {
	// PollInterval is the delay between sweeps of every target.
	PollInterval Duration `toml:"poll_interval"`
	// CaptureLines is how many rows are kept from the bottom of each pane's
	// visible screen. Scrollback is never read: an answered prompt lingers
	// there and would make verification fail forever.
	CaptureLines int `toml:"capture_lines"`
	// MatchLines is how many of those rows rules are matched against. Keeping
	// this smaller than CaptureLines anchors matching near the prompt and stops
	// stale scrollback from triggering a rule.
	MatchLines int `toml:"match_lines"`
	// StablePolls is how many consecutive identical captures a pane must
	// produce before it is considered settled enough to act on.
	StablePolls int `toml:"stable_polls"`
	// Settle is how long to wait after a keypress before re-reading the pane.
	Settle Duration `toml:"settle"`
	// DryRun decides everything but sends no keys.
	DryRun bool `toml:"dry_run"`
	// Learn records unrecognised prompts for later rule authoring.
	Learn bool `toml:"learn"`
	// SkipActivePane leaves the pane you are looking at alone.
	SkipActivePane bool `toml:"skip_active_pane"`

	// PauseFile is a kill switch. While it exists agentsitter keeps observing and
	// logging but sends no keys. `agentsitter pause` and `agentsitter resume` manage
	// it, and so can anything else that can create a file.
	PauseFile string `toml:"pause_file"`

	StateFile string   `toml:"state_file"`
	AuditFile string   `toml:"audit_file"`
	LearnDir  string   `toml:"learn_dir"`
	RuleFiles []string `toml:"rule_files"`

	// NotifyCommand runs on notable events with a JSON event on stdin.
	NotifyCommand string `toml:"notify_command"`

	Limits  Limits   `toml:"limits"`
	Safety  Safety   `toml:"safety"`
	Targets []Target `toml:"targets"`
}

// Limits bound how often agentsitter is willing to press keys. They exist so that
// a rule which matches but never resolves degrades into a logged complaint
// rather than an infinite keypress loop.
type Limits struct {
	PaneCooldown      Duration `toml:"pane_cooldown"`
	PerRulePerHour    int      `toml:"per_rule_per_hour"`
	GlobalPerHour     int      `toml:"global_per_hour"`
	MaxVerifyFailures int      `toml:"max_verify_failures"`
	Quarantine        Duration `toml:"quarantine"`
}

// Safety is a veto that runs after a rule matches and before any key is sent.
// Any pane whose visible text matches one of these patterns is left alone no
// matter what the ruleset says.
type Safety struct {
	Enabled    bool     `toml:"enabled"`
	NeverMatch []string `toml:"never_match"`

	compiled []*regexp.Regexp
}

// Compiled returns the parsed never_match patterns.
func (s *Safety) Compiled() []*regexp.Regexp { return s.compiled }

// Veto reports the first never_match pattern that the text trips, or nil.
func (s *Safety) Veto(text string) *regexp.Regexp {
	if !s.Enabled {
		return nil
	}
	for _, re := range s.compiled {
		if re.MatchString(text) {
			return re
		}
	}
	return nil
}

// Target is one tmux server to watch, either on this machine or over SSH.
type Target struct {
	// Name labels the target in logs. Defaults to the SSH host, or "local".
	Name string `toml:"name"`
	// SSH is an ssh destination such as "user@host" or a host alias from
	// ssh_config. Empty means this machine.
	SSH string `toml:"ssh"`
	// SSHArgs are extra arguments passed to ssh before the destination.
	SSHArgs []string `toml:"ssh_args"`
	// Socket is a tmux socket name (tmux -L). The special value "*" means
	// every socket with a live server, which is the default: agents do not
	// always live on tmux's "default" socket, and a watcher that assumes they
	// do silently watches nothing.
	Socket string `toml:"socket"`
	// SocketPath is a tmux socket path (tmux -S), taking priority over Socket.
	SocketPath string `toml:"socket_path"`
	// Sessions filters panes by session name. Patterns are regexes; empty
	// means every session.
	Sessions []string `toml:"sessions"`
	// Commands filters panes by their foreground process name. Patterns are
	// regexes; empty means every pane.
	Commands []string `toml:"commands"`
	// ExcludePanes skips panes whose "session:window.pane" address matches.
	ExcludePanes []string `toml:"exclude_panes"`

	sessionRes []*regexp.Regexp
	commandRes []*regexp.Regexp
	excludeRes []*regexp.Regexp
}

// Label returns a human-readable identifier for the target.
func (t Target) Label() string {
	switch {
	case t.Name != "":
		return t.Name
	case t.SSH != "":
		return t.SSH
	default:
		return "local"
	}
}

// Remote reports whether the target is reached over SSH.
func (t Target) Remote() bool { return t.SSH != "" }

// MatchesSession reports whether a session name passes the session filter.
func (t Target) MatchesSession(name string) bool { return matchAny(t.sessionRes, name) }

// MatchesCommand reports whether a pane's process name passes the filter.
func (t Target) MatchesCommand(cmd string) bool { return matchAny(t.commandRes, cmd) }

// Excluded reports whether a pane address is explicitly skipped.
func (t Target) Excluded(addr string) bool {
	for _, re := range t.excludeRes {
		if re.MatchString(addr) {
			return true
		}
	}
	return false
}

// matchAny returns true when the list is empty (no filter) or one pattern hits.
func matchAny(res []*regexp.Regexp, s string) bool {
	if len(res) == 0 {
		return true
	}
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// Default returns the configuration used when no file is present.
func Default() Config {
	return Config{
		PollInterval: Duration{3 * time.Second},
		CaptureLines: 80,
		MatchLines:   32,
		StablePolls:  2,
		Settle:       Duration{800 * time.Millisecond},
		DryRun:       false,
		Learn:        true,
		PauseFile:    "~/.local/state/agentsitter/paused",
		StateFile:    "~/.local/state/agentsitter/state.json",
		AuditFile:    "~/.local/state/agentsitter/audit.jsonl",
		LearnDir:     "~/.local/state/agentsitter/learn",
		Limits: Limits{
			PaneCooldown:      Duration{6 * time.Second},
			PerRulePerHour:    6,
			GlobalPerHour:     60,
			MaxVerifyFailures: 3,
			Quarantine:        Duration{10 * time.Minute},
		},
		Safety: Safety{
			Enabled:    true,
			NeverMatch: DefaultNeverMatch(),
		},
		Targets: []Target{DefaultTarget()},
	}
}

// DefaultTarget watches the local tmux server's agent panes.
func DefaultTarget() Target {
	return Target{Name: "local", Socket: DiscoverSockets, Commands: DefaultCommands()}
}

// DiscoverSockets is the socket value meaning "find them all".
const DiscoverSockets = "*"

// DefaultCommands lists the foreground process names that identify an agent
// pane.
//
// The agent names are prefix matches on purpose. Orchestrators routinely launch
// agents through a wrapper, so the process in tmux is something like
// "codex-dispatch" rather than "codex"; an anchored match would watch nothing
// while appearing to work.
//
// The bare version-number pattern is not a typo either: some agent CLIs install
// their binary under a version-named path, so the process shows up in tmux as
// something like "2.1.243".
func DefaultCommands() []string {
	return []string{`^codex`, `^claude`, `^gemini`, `^node$`, `^\d+\.\d+\.\d+`}
}

// DefaultNeverMatch is a conservative veto list. agentsitter will not answer a
// prompt whose visible text mentions one of these, because a wrong keypress
// there is expensive and unrecoverable. Set safety.enabled = false to disable.
func DefaultNeverMatch() []string {
	return []string{
		`(?i)\brm\s+-[a-z]*[rf]`,
		`(?i)\bgit\s+push\b.*--force|\bforce[- ]push\b`,
		`(?i)\bgit\s+reset\s+--hard\b`,
		`(?i)\bDROP\s+(TABLE|DATABASE|SCHEMA)\b`,
		`(?i)\bTRUNCATE\s+TABLE\b`,
		`(?i)\bmkfs\b|\bdd\s+if=.*of=/dev/`,
		`(?i)\bkubectl\s+delete\b`,
		`(?i)\bterraform\s+(destroy|apply)\b`,
		`(?i)delete (all|every) `,
		`(?i)\b(permanently|irreversibl)`,
		`(?i)enter your (password|passphrase|api key|token)`,
		`(?i)\bcredit card\b|\bpayment method\b`,
	}
}

// DefaultPath returns the configuration file location, honouring
// AGENTSITTER_CONFIG and then XDG_CONFIG_HOME.
func DefaultPath() string {
	if p := os.Getenv("AGENTSITTER_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "agentsitter.toml"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "agentsitter", "agentsitter.toml")
}

// Load reads a configuration file, falling back to defaults when the path does
// not exist. A missing file is not an error: agentsitter is useful with defaults
// alone on the machine it runs on.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	expanded, err := ExpandPath(path)
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(expanded)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := cfg.Finalize(); err != nil {
			return cfg, err
		}
		return cfg, nil
	case err != nil:
		return cfg, fmt.Errorf("read config %s: %w", expanded, err)
	}

	// Targets are replaced wholesale rather than merged, so a config that
	// declares its own targets does not silently keep the default one.
	if strings.Contains(string(data), "[[targets]]") {
		cfg.Targets = nil
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", expanded, err)
	}
	if err := cfg.Finalize(); err != nil {
		return cfg, fmt.Errorf("config %s: %w", expanded, err)
	}
	return cfg, nil
}

// Finalize applies fallbacks, compiles patterns, and validates. Load calls it;
// callers assembling a Config by hand must call it before use, or the regex
// filters will all be empty.
func (c *Config) Finalize() error {
	if c.PollInterval.Duration <= 0 {
		c.PollInterval = Duration{3 * time.Second}
	}
	if c.CaptureLines <= 0 {
		c.CaptureLines = 80
	}
	if c.MatchLines <= 0 || c.MatchLines > c.CaptureLines {
		c.MatchLines = c.CaptureLines
	}
	if c.StablePolls < 1 {
		c.StablePolls = 1
	}
	if c.Limits.GlobalPerHour <= 0 {
		c.Limits.GlobalPerHour = 60
	}
	if c.Limits.PerRulePerHour <= 0 {
		c.Limits.PerRulePerHour = 6
	}
	if c.Limits.MaxVerifyFailures <= 0 {
		c.Limits.MaxVerifyFailures = 3
	}
	if len(c.Targets) == 0 {
		c.Targets = []Target{DefaultTarget()}
	}

	for _, p := range []*string{&c.StateFile, &c.AuditFile, &c.LearnDir, &c.PauseFile} {
		v, err := ExpandPath(*p)
		if err != nil {
			return err
		}
		*p = v
	}
	for i, p := range c.RuleFiles {
		v, err := ExpandPath(p)
		if err != nil {
			return err
		}
		c.RuleFiles[i] = v
	}

	compiled, err := compileAll(c.Safety.NeverMatch, "safety.never_match")
	if err != nil {
		return err
	}
	c.Safety.compiled = compiled

	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Socket == "" && t.SocketPath == "" {
			t.Socket = DiscoverSockets
		}
		if len(t.Commands) == 0 {
			t.Commands = DefaultCommands()
		}
		if t.sessionRes, err = compileAll(t.Sessions, "targets.sessions"); err != nil {
			return err
		}
		if t.commandRes, err = compileAll(t.Commands, "targets.commands"); err != nil {
			return err
		}
		if t.excludeRes, err = compileAll(t.ExcludePanes, "targets.exclude_panes"); err != nil {
			return err
		}
	}
	return nil
}

func compileAll(pats []string, field string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(pats))
	for _, p := range pats {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: bad pattern %q: %w", field, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// ExpandPath resolves a leading "~" against the current user's home directory.
func ExpandPath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p, nil
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil // "~user" forms are left alone
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p, fmt.Errorf("expand %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
