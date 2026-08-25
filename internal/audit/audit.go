// Package audit records what agentsitter saw and what it did about it.
//
// Every keypress agentsitter sends is written to an append-only JSONL log before
// the fact. Unattended software that types into live sessions has to be
// auditable after the fact, otherwise an unexpected agent state is impossible
// to attribute.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Outcome classifies what happened to a prompt agentsitter recognised.
type Outcome string

const (
	// OutcomeAnswered means keys were sent and the after-check passed.
	OutcomeAnswered Outcome = "answered"
	// OutcomeDryRun means the answer was resolved but no keys were sent.
	OutcomeDryRun Outcome = "dry_run"
	// OutcomeUnresolved means a rule recognised the prompt but none of its
	// preferred options were on screen, so nothing was pressed.
	OutcomeUnresolved Outcome = "unresolved"
	// OutcomeVetoed means the safety list blocked an otherwise valid answer.
	OutcomeVetoed Outcome = "vetoed"
	// OutcomeThrottled means a rate limit or quarantine blocked the answer.
	OutcomeThrottled Outcome = "throttled"
	// OutcomeVerifyFailed means keys were sent but the prompt did not clear.
	OutcomeVerifyFailed Outcome = "verify_failed"
	// OutcomeAborted means the screen changed between deciding and confirming,
	// so agentsitter backed out rather than pressing Enter on an unknown option.
	OutcomeAborted Outcome = "aborted"
	// OutcomeError means a tmux or transport call failed.
	OutcomeError Outcome = "error"
	// OutcomeUnknownPrompt means a prompt was detected that no rule claims.
	OutcomeUnknownPrompt Outcome = "unknown_prompt"
)

// Event is one line of the audit log.
type Event struct {
	Time     time.Time `json:"time"`
	Outcome  Outcome   `json:"outcome"`
	Target   string    `json:"target"`
	Pane     string    `json:"pane,omitempty"`
	PaneAddr string    `json:"pane_addr,omitempty"`
	Command  string    `json:"command,omitempty"`
	Rule     string    `json:"rule,omitempty"`
	Option   string    `json:"option,omitempty"`
	Keys     []string  `json:"keys,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Capture  string    `json:"capture,omitempty"`
	Excerpt  string    `json:"excerpt,omitempty"`
}

// Notable reports whether an event is worth waking a human for.
func (e Event) Notable() bool {
	switch e.Outcome {
	case OutcomeVetoed, OutcomeVerifyFailed, OutcomeUnknownPrompt, OutcomeError:
		return true
	}
	return false
}

// String renders a one-line human-readable summary.
func (e Event) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %s", e.Outcome, e.Target)
	if e.PaneAddr != "" {
		fmt.Fprintf(&b, " %s", e.PaneAddr)
	}
	if e.Pane != "" {
		fmt.Fprintf(&b, " (%s)", e.Pane)
	}
	if e.Rule != "" {
		fmt.Fprintf(&b, " rule=%s", e.Rule)
	}
	if e.Option != "" {
		fmt.Fprintf(&b, " option=%q", truncate(e.Option, 60))
	}
	if len(e.Keys) > 0 {
		fmt.Fprintf(&b, " keys=%s", strings.Join(e.Keys, ","))
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, " reason=%s", e.Reason)
	}
	return b.String()
}

// Logger appends events to a JSONL file, stores unrecognised screens, and
// shells out to a notification command.
type Logger struct {
	mu       sync.Mutex
	path     string
	learnDir string
	notify   string

	// seen deduplicates learn captures so a prompt that sits on screen for an
	// hour produces one file rather than one per poll.
	seen map[string]time.Time
}

// NewLogger creates a logger. An empty path disables file logging.
func NewLogger(path, learnDir, notifyCommand string) *Logger {
	return &Logger{
		path:     path,
		learnDir: learnDir,
		notify:   notifyCommand,
		seen:     map[string]time.Time{},
	}
}

// Log appends an event.
func (l *Logger) Log(ev Event) error {
	if l.path == "" {
		return nil
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// learnRetention is how long a learn fingerprint is remembered.
const learnRetention = 6 * time.Hour

// maxSeen caps the dedupe map so a long-running daemon cannot grow unbounded.
const maxSeen = 512

// Learn records an unrecognised prompt for later rule authoring and returns the
// file it wrote. An empty return means the screen was a repeat and was skipped.
func (l *Logger) Learn(ev Event, raw, plain string, now time.Time) (string, error) {
	if l.learnDir == "" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(plain))
	fp := hex.EncodeToString(sum[:])[:12]

	l.mu.Lock()
	if last, ok := l.seen[fp]; ok && now.Sub(last) < learnRetention {
		l.seen[fp] = now
		l.mu.Unlock()
		return "", nil
	}
	l.seen[fp] = now
	l.pruneSeenLocked(now)
	l.mu.Unlock()

	if err := os.MkdirAll(l.learnDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s.txt",
		now.UTC().Format("20060102T150405Z"), sanitize(ev.Target), fp)
	path := filepath.Join(l.learnDir, name)

	var b bytes.Buffer
	fmt.Fprintf(&b, "# agentsitter learn capture\n")
	fmt.Fprintf(&b, "# time:    %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "# target:  %s\n", ev.Target)
	fmt.Fprintf(&b, "# pane:    %s (%s)\n", ev.PaneAddr, ev.Pane)
	fmt.Fprintf(&b, "# command: %s\n", ev.Command)
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# No rule claimed this screen. To teach agentsitter about it, add a rule\n")
	fmt.Fprintf(&b, "# whose 'any' matches the prompt text below and whose 'options' match the\n")
	fmt.Fprintf(&b, "# line you would choose, then point rule_files at that file.\n")
	fmt.Fprintf(&b, "\n--- visible text ---\n%s\n", plain)
	fmt.Fprintf(&b, "\n--- raw capture (escape sequences escaped) ---\n%q\n", raw)

	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// pruneSeenLocked trims the dedupe map. The caller holds the lock.
func (l *Logger) pruneSeenLocked(now time.Time) {
	for fp, t := range l.seen {
		if now.Sub(t) > learnRetention {
			delete(l.seen, fp)
		}
	}
	if len(l.seen) <= maxSeen {
		return
	}
	type entry struct {
		fp string
		t  time.Time
	}
	entries := make([]entry, 0, len(l.seen))
	for fp, t := range l.seen {
		entries = append(entries, entry{fp, t})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].t.Before(entries[j].t) })
	for _, e := range entries[:len(entries)-maxSeen] {
		delete(l.seen, e.fp)
	}
}

// notifyTimeout bounds a hook so a wedged notifier cannot stall the poll loop.
const notifyTimeout = 10 * time.Second

// Notify runs the configured notification command with the event as JSON on
// stdin. Failures are returned but are never fatal to the caller.
func (l *Logger) Notify(ctx context.Context, ev Event) error {
	if l.notify == "" {
		return nil
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", l.notify)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"AGENTSITTER_OUTCOME="+string(ev.Outcome),
		"AGENTSITTER_TARGET="+ev.Target,
		"AGENTSITTER_PANE="+ev.PaneAddr,
		"AGENTSITTER_RULE="+ev.Rule,
		"AGENTSITTER_SUMMARY="+ev.String(),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify command: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Excerpt returns the last few non-blank lines of a screen, for log context.
func Excerpt(text string, n int) string {
	lines := strings.Split(text, "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append([]string{strings.TrimSpace(lines[i])}, kept...)
	}
	return strings.Join(kept, " | ")
}

func sanitize(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
