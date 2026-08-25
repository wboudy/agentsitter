// Package engine is agentsitter's poll loop.
//
// The safety property that matters here is that Enter is never pressed
// speculatively. Answering a menu is split in two: move the highlight, confirm
// by re-reading the pane that the highlight actually landed on the intended
// option, and only then submit. If the screen changed underneath in between,
// the attempt is abandoned rather than completed against an unknown state.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/wboudy/agentsitter/internal/audit"
	"github.com/wboudy/agentsitter/internal/config"
	"github.com/wboudy/agentsitter/internal/guard"
	"github.com/wboudy/agentsitter/internal/rules"
	"github.com/wboudy/agentsitter/internal/screen"
	"github.com/wboudy/agentsitter/internal/tmuxio"
)

// maxMove bounds how far the cursor will be walked to reach an option. A
// larger distance means the screen is not the menu agentsitter thinks it is.
const maxMove = 24

// PaneClient is the slice of tmux the engine depends on. Keeping it narrow
// lets the decide-and-act logic, which is the part that types into live
// sessions, be tested against scripted screens.
type PaneClient interface {
	ListPanes(ctx context.Context) ([]tmuxio.Pane, error)
	Capture(ctx context.Context, paneID string, lines int) (string, error)
	SendKeys(ctx context.Context, paneID string, keys ...string) error
}

// Engine polls targets and answers recognised prompts.
type Engine struct {
	cfg    config.Config
	rules  *rules.Set
	guard  *guard.Guard
	logger *audit.Logger
	out    io.Writer

	clients  map[string]PaneClient
	trackers map[string]*tracker
	sockets  map[string]socketCache

	// NewClient builds the transport for a target. Tests replace it.
	NewClient func(config.Target) PaneClient

	// Now is injectable so tests do not depend on wall-clock time.
	Now func() time.Time
	// Verbose logs every pane considered, not just acted-on ones.
	Verbose bool
}

// tracker remembers a pane between polls so agentsitter can tell a settled screen
// from one that is still being redrawn.
type tracker struct {
	fingerprint string
	stable      int
	lastSeen    time.Time
}

// Result summarises one sweep.
type Result struct {
	Events    []audit.Event
	Errors    []error
	Panes     int // panes seen on all targets
	Watched   int // panes passing the filters
	Evaluated int // panes settled enough to match rules against
}

// New builds an engine.
func New(cfg config.Config, rs *rules.Set, g *guard.Guard, lg *audit.Logger, out io.Writer) *Engine {
	return &Engine{
		cfg:       cfg,
		rules:     rs,
		guard:     g,
		logger:    lg,
		out:       out,
		clients:   map[string]PaneClient{},
		trackers:  map[string]*tracker{},
		sockets:   map[string]socketCache{},
		Now:       time.Now,
		NewClient: dialTmux,
	}
}

// dialTmux is the production transport factory.
func dialTmux(t config.Target) PaneClient {
	return &tmuxio.Client{
		SSH:        t.SSH,
		SSHArgs:    t.SSHArgs,
		Socket:     t.Socket,
		SocketPath: t.SocketPath,
	}
}

// Client returns the tmux client for a target, creating it once and reusing it
// so ssh multiplexing has something to share.
func (e *Engine) Client(t config.Target) PaneClient {
	key := t.Label() + "|" + t.Socket + "|" + t.SocketPath
	if c, ok := e.clients[key]; ok {
		return c
	}
	c := e.NewClient(t)
	e.clients[key] = c
	return c
}

// Paused reports whether the pause file exists. While paused agentsitter keeps
// observing and logging but sends no keys.
func (e *Engine) Paused() bool {
	if e.cfg.PauseFile == "" {
		return false
	}
	_, err := os.Stat(e.cfg.PauseFile)
	return err == nil
}

// Run polls until the context is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.cfg.PollInterval.Duration)
	defer ticker.Stop()

	prune := time.NewTicker(10 * time.Minute)
	defer prune.Stop()

	e.sweepAndReport(ctx)
	for {
		select {
		case <-ctx.Done():
			if err := e.guard.Save(); err != nil {
				fmt.Fprintf(e.out, "agentsitter: saving state: %v\n", err)
			}
			return ctx.Err()
		case <-prune.C:
			e.guard.Prune(e.Now())
			e.pruneTrackers()
		case <-ticker.C:
			e.sweepAndReport(ctx)
		}
	}
}

func (e *Engine) sweepAndReport(ctx context.Context) {
	res := e.Sweep(ctx)
	for _, err := range res.Errors {
		fmt.Fprintf(e.out, "agentsitter: %v\n", err)
	}
	if err := e.guard.Save(); err != nil {
		fmt.Fprintf(e.out, "agentsitter: saving state: %v\n", err)
	}
}

// pruneTrackers forgets panes that have not been seen recently.
func (e *Engine) pruneTrackers() {
	cutoff := e.Now().Add(-1 * time.Hour)
	for k, t := range e.trackers {
		if t.lastSeen.Before(cutoff) {
			delete(e.trackers, k)
		}
	}
}

// socketCacheTTL bounds how long a discovered socket list is reused before
// agentsitter looks again, so a tmux server started after startup is picked up.
const socketCacheTTL = 30 * time.Second

// socketCache remembers one target's discovered sockets.
type socketCache struct {
	sockets []string
	at      time.Time
}

// socketLister is implemented by transports that can enumerate tmux sockets.
type socketLister interface {
	ListSockets(ctx context.Context) ([]string, error)
}

// Resolve expands a target into one concrete target per tmux socket. A target
// naming its socket resolves to itself; one asking to discover resolves to
// every socket present on the machine.
func (e *Engine) Resolve(ctx context.Context, t config.Target) []config.Target {
	if t.SocketPath != "" || t.Socket != config.DiscoverSockets {
		return []config.Target{t}
	}
	lister, ok := e.Client(t).(socketLister)
	if !ok {
		return []config.Target{t}
	}

	key := t.Label()
	now := e.Now()
	cached, hit := e.sockets[key]
	if !hit || now.Sub(cached.at) > socketCacheTTL {
		names, err := lister.ListSockets(ctx)
		if err != nil {
			// Fall back to whatever was last known rather than going blind.
			if !hit {
				return nil
			}
		} else {
			cached = socketCache{sockets: names, at: now}
			e.sockets[key] = cached
		}
	}

	out := make([]config.Target, 0, len(cached.sockets))
	for _, name := range cached.sockets {
		sub := t
		sub.Socket = name
		sub.Name = t.Label() + ":" + name
		out = append(out, sub)
	}
	return out
}

// Sweep polls every target once.
func (e *Engine) Sweep(ctx context.Context) Result {
	var res Result
	for _, target := range e.cfg.Targets {
		for _, resolved := range e.Resolve(ctx, target) {
			e.sweepTarget(ctx, resolved, &res)
		}
	}
	return res
}

func (e *Engine) sweepTarget(ctx context.Context, target config.Target, res *Result) {
	client := e.Client(target)
	panes, err := client.ListPanes(ctx)
	if err != nil {
		if errors.Is(err, tmuxio.ErrNoServer) {
			// A machine with no agents on it has no tmux server. Not a fault.
			return
		}
		res.Errors = append(res.Errors, err)
		return
	}

	res.Panes += len(panes)
	for _, pane := range panes {
		if !e.watchable(target, pane) {
			continue
		}
		res.Watched++
		e.sweepPane(ctx, target, client, pane, res)
	}
}

// watchable applies the target's filters to a pane.
func (e *Engine) watchable(t config.Target, p tmuxio.Pane) bool {
	switch {
	case p.Dead:
		return false
	case p.InMode:
		// The pane is scrolled back in copy mode, so what is visible is not
		// the live screen and keys would go to the copy-mode handler.
		return false
	case e.cfg.SkipActivePane && p.Active:
		return false
	case !t.MatchesSession(p.Session):
		return false
	case !t.MatchesCommand(p.Command):
		return false
	case t.Excluded(p.Addr()):
		return false
	}
	return true
}

func (e *Engine) sweepPane(ctx context.Context, target config.Target, client PaneClient, pane tmuxio.Pane, res *Result) {
	now := e.Now()
	key := target.Label() + "/" + pane.ID

	base := audit.Event{
		Time:     now,
		Target:   target.Label(),
		Pane:     pane.ID,
		PaneAddr: pane.Addr(),
		Command:  pane.Command,
	}

	raw, err := client.Capture(ctx, pane.ID, e.cfg.CaptureLines)
	if err != nil {
		if errors.Is(err, tmuxio.ErrNoServer) {
			return
		}
		ev := base
		ev.Outcome = audit.OutcomeError
		ev.Reason = err.Error()
		e.emit(ctx, ev, res)
		return
	}

	// A pane must look the same for several consecutive polls before it is
	// considered settled. This keeps agentsitter off a screen that is mid-redraw,
	// where the option list may be only partly drawn.
	fp := screen.Fingerprint(raw)
	tr, ok := e.trackers[key]
	if !ok {
		tr = &tracker{}
		e.trackers[key] = tr
	}
	tr.lastSeen = now
	if tr.fingerprint == fp {
		tr.stable++
	} else {
		tr.fingerprint = fp
		tr.stable = 1
	}
	if tr.stable < e.cfg.StablePolls {
		return
	}
	res.Evaluated++

	tail := screen.Parse(raw).Tail(e.cfg.MatchLines)
	text := tail.Text()

	candidates := e.rules.Candidates(pane.Command, text)
	if len(candidates) == 0 {
		e.maybeLearn(ctx, base, tail, raw, text, now, res)
		return
	}

	for _, rule := range candidates {
		ev, done := e.applyRule(ctx, client, rule, base, tail, key, text, now)
		e.emit(ctx, ev, res)
		if done {
			return
		}
	}
}

// maybeLearn records a screen that looks like a prompt but that no rule claims.
func (e *Engine) maybeLearn(ctx context.Context, base audit.Event, tail screen.Screen, raw, text string, now time.Time, res *Result) {
	if !e.cfg.Learn || !tail.LooksLikeSelector() {
		return
	}
	ev := base
	ev.Outcome = audit.OutcomeUnknownPrompt
	ev.Excerpt = audit.Excerpt(text, 4)

	path, err := e.logger.Learn(ev, raw, text, now)
	if err != nil {
		ev.Reason = fmt.Sprintf("writing learn capture: %v", err)
	}
	if path == "" && err == nil {
		return // already recorded this screen recently
	}
	ev.Capture = path
	e.emit(ctx, ev, res)
}

// applyRule attempts one rule. The bool reports whether the pane is settled for
// this sweep, meaning no further candidate should be tried.
func (e *Engine) applyRule(ctx context.Context, client PaneClient, rule *rules.Compiled, base audit.Event, tail screen.Screen, key, text string, now time.Time) (audit.Event, bool) {
	ev := base
	ev.Rule = rule.Name
	ev.Excerpt = audit.Excerpt(text, 3)

	// The safety veto runs after recognition and before anything is sent, so
	// it applies no matter which rule matched or how it was written.
	if re := e.cfg.Safety.Veto(text); re != nil {
		ev.Outcome = audit.OutcomeVetoed
		ev.Reason = "safety.never_match matched " + re.String()
		return ev, true
	}

	decision := e.guard.Allow(key, rule.Name, rule.MaxPerHour, rule.Cooldown.Duration, now)
	if !decision.Allowed {
		ev.Outcome = audit.OutcomeThrottled
		ev.Reason = decision.Reason
		return ev, true
	}

	plan, err := e.plan(rule, tail)
	if err != nil {
		// This rule cannot resolve an answer on this screen. Fall through to
		// the next candidate rather than pressing anything.
		ev.Outcome = audit.OutcomeUnresolved
		ev.Reason = err.Error()
		return ev, false
	}
	ev.Option = plan.optionText
	ev.Keys = plan.keys

	if e.cfg.DryRun || e.Paused() {
		ev.Outcome = audit.OutcomeDryRun
		if e.Paused() {
			ev.Reason = "paused"
		}
		return ev, true
	}

	e.guard.RecordAction(key, rule.Name, now)
	if err := e.execute(ctx, client, rule, plan, base.Pane); err != nil {
		var abort *abortErr
		if errors.As(err, &abort) {
			ev.Outcome = audit.OutcomeAborted
			ev.Reason = abort.Error()
			e.guard.RecordFailure(key, now)
			return ev, true
		}
		ev.Outcome = audit.OutcomeError
		ev.Reason = err.Error()
		return ev, true
	}

	if rule.VerifyPattern() != nil {
		time.Sleep(e.cfg.Settle.Duration)
		after, err := client.Capture(ctx, base.Pane, e.cfg.CaptureLines)
		if err != nil {
			ev.Outcome = audit.OutcomeError
			ev.Reason = fmt.Sprintf("re-reading pane after answering: %v", err)
			return ev, true
		}
		if rule.VerifyPattern().MatchString(screen.Parse(after).Tail(e.cfg.MatchLines).Text()) {
			ev.Outcome = audit.OutcomeVerifyFailed
			ev.Reason = "prompt still present after answering: " + rule.VerifyPattern().String()
			if e.guard.RecordFailure(key, now) {
				ev.Reason += "; pane quarantined"
			}
			return ev, true
		}
	}

	e.guard.RecordSuccess(key)
	ev.Outcome = audit.OutcomeAnswered
	return ev, true
}

// plan is a resolved answer: either literal keys, or a menu move plus submit.
type plan struct {
	keys       []string // literal key rule
	moves      []string // Up/Down presses to reach the option
	submit     string
	optionText string
	optionRe   *regexp.Regexp
	digit      string // numbered-menu shortcut, when no highlight was found
}

// plan works out how to answer, without touching the pane.
func (e *Engine) plan(rule *rules.Compiled, tail screen.Screen) (plan, error) {
	if len(rule.Keys) > 0 {
		return plan{keys: rule.Keys}, nil
	}

	targetIdx := -1
	var chosen *regexp.Regexp
	for _, re := range rule.OptionPatterns() {
		if idx := tail.Find(re); idx >= 0 {
			targetIdx, chosen = idx, re
			break
		}
	}
	if targetIdx < 0 {
		return plan{}, errors.New("none of the rule's preferred options are on screen")
	}

	p := plan{
		submit:     rule.SubmitKey(),
		optionText: tail.Lines[targetIdx].Text,
		optionRe:   chosen,
	}

	selected := tail.SelectedIndex()
	if selected < 0 {
		// Nothing is highlighted. A numbered menu can still be answered by
		// its number; anything else is not safe to guess at.
		if n := tail.OptionNumber(targetIdx); n > 0 {
			p.digit = strconv.Itoa(n)
			return p, nil
		}
		return plan{}, errors.New("no highlighted option and the menu is not numbered")
	}

	delta := targetIdx - selected
	if delta > maxMove || delta < -maxMove {
		return plan{}, fmt.Errorf("option is %d rows from the cursor, beyond the %d row limit", delta, maxMove)
	}
	key := "Down"
	if delta < 0 {
		key, delta = "Up", -delta
	}
	for i := 0; i < delta; i++ {
		p.moves = append(p.moves, key)
	}
	return p, nil
}

// abortErr means agentsitter backed out before submitting.
type abortErr struct{ msg string }

func (a *abortErr) Error() string { return a.msg }

// execute performs a plan against a live pane.
func (e *Engine) execute(ctx context.Context, client PaneClient, rule *rules.Compiled, p plan, paneID string) error {
	if len(p.keys) > 0 {
		return client.SendKeys(ctx, paneID, p.keys...)
	}
	if p.digit != "" {
		return client.SendKeys(ctx, paneID, p.digit)
	}

	if len(p.moves) > 0 {
		if err := client.SendKeys(ctx, paneID, p.moves...); err != nil {
			return err
		}
		time.Sleep(e.cfg.Settle.Duration)
	}

	// Confirm the highlight is where it was aimed. Until this passes, no
	// submit key is sent, so a menu that scrolled or repainted under us costs
	// nothing worse than an abandoned attempt.
	if rule.NeedsConfirm() {
		raw, err := client.Capture(ctx, paneID, e.cfg.CaptureLines)
		if err != nil {
			return &abortErr{fmt.Sprintf("re-reading pane before submitting: %v", err)}
		}
		after := screen.Parse(raw).Tail(e.cfg.MatchLines)
		idx := after.SelectedIndex()
		if idx < 0 {
			return &abortErr{"no option is highlighted after moving the cursor"}
		}
		if !p.optionRe.MatchString(after.Lines[idx].Text) {
			return &abortErr{fmt.Sprintf("highlight landed on %q, not the intended option", after.Lines[idx].Text)}
		}
	}
	return client.SendKeys(ctx, paneID, p.submit)
}

// emit records an event and notifies when it is worth a human's attention.
func (e *Engine) emit(ctx context.Context, ev audit.Event, res *Result) {
	res.Events = append(res.Events, ev)

	if err := e.logger.Log(ev); err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("writing audit log: %w", err))
	}
	if e.Verbose || ev.Outcome != audit.OutcomeThrottled {
		fmt.Fprintln(e.out, ev.String())
	}
	if ev.Notable() {
		if err := e.logger.Notify(ctx, ev); err != nil {
			res.Errors = append(res.Errors, err)
		}
	}
}

// PaneInfo is one pane and whether agentsitter would watch it.
type PaneInfo struct {
	Target  string
	Pane    tmuxio.Pane
	Watched bool
	Reason  string
}

// Config exposes the engine's configuration for reporting.
func (e *Engine) Config() config.Config { return e.cfg }

// Rules exposes the active rule set for reporting.
func (e *Engine) Rules() *rules.Set { return e.rules }

// Guard exposes the rate limiter for reporting.
func (e *Engine) Guard() *guard.Guard { return e.guard }

// Inventory lists every pane on every target with the filter verdict, which is
// what `agentsitter panes` reports.
func (e *Engine) Inventory(ctx context.Context) ([]PaneInfo, []error) {
	var (
		out  []PaneInfo
		errs []error
	)
	for _, target := range e.cfg.Targets {
		for _, resolved := range e.Resolve(ctx, target) {
			panes, err := e.Client(resolved).ListPanes(ctx)
			if err != nil {
				if !errors.Is(err, tmuxio.ErrNoServer) {
					errs = append(errs, err)
				}
				continue
			}
			for _, p := range panes {
				out = append(out, PaneInfo{
					Target:  resolved.Label(),
					Pane:    p,
					Watched: e.watchable(resolved, p),
					Reason:  e.skipReason(resolved, p),
				})
			}
		}
	}
	return out, errs
}

// skipReason explains why a pane is not watched, or "" when it is.
func (e *Engine) skipReason(t config.Target, p tmuxio.Pane) string {
	switch {
	case p.Dead:
		return "pane is dead"
	case p.InMode:
		return "pane is in copy mode"
	case e.cfg.SkipActivePane && p.Active:
		return "active pane, skip_active_pane is on"
	case !t.MatchesSession(p.Session):
		return "session does not match the target filter"
	case !t.MatchesCommand(p.Command):
		return "process " + p.Command + " is not an agent"
	case t.Excluded(p.Addr()):
		return "pane is in exclude_panes"
	}
	return ""
}

// Explanation is a decision traced against one pane, for `agentsitter explain`.
// It is the tool for authoring and debugging rules: it shows exactly what
// agentsitter sees, which lines it considers selected, and what it would do.
type Explanation struct {
	Target     string
	Pane       tmuxio.Pane
	Raw        string
	Tail       screen.Screen
	Selected   int
	Selector   bool
	Vetoed     string
	Candidates []Candidate
}

// Candidate is one rule's verdict on a pane.
type Candidate struct {
	Rule    string
	Option  string
	Keys    []string
	Failure string
}

// Explain captures a pane and reports what agentsitter would decide about it,
// without sending anything.
func (e *Engine) Explain(ctx context.Context, paneID string) (*Explanation, error) {
	for _, target := range e.cfg.Targets {
		for _, resolved := range e.Resolve(ctx, target) {
			client := e.Client(resolved)
			panes, err := client.ListPanes(ctx)
			if err != nil {
				continue
			}
			for _, p := range panes {
				if p.ID != paneID && p.Addr() != paneID {
					continue
				}
				raw, err := client.Capture(ctx, p.ID, e.cfg.CaptureLines)
				if err != nil {
					return nil, err
				}
				return e.explainScreen(resolved.Label(), p, raw), nil
			}
		}
	}
	return nil, fmt.Errorf("pane %q not found on any target", paneID)
}

// explainScreen runs the decision logic against an already-captured screen.
func (e *Engine) explainScreen(target string, pane tmuxio.Pane, raw string) *Explanation {
	tail := screen.Parse(raw).Tail(e.cfg.MatchLines)
	text := tail.Text()

	ex := &Explanation{
		Target:   target,
		Pane:     pane,
		Raw:      raw,
		Tail:     tail,
		Selected: tail.SelectedIndex(),
		Selector: tail.LooksLikeSelector(),
	}
	if re := e.cfg.Safety.Veto(text); re != nil {
		ex.Vetoed = re.String()
	}
	for _, rule := range e.rules.Candidates(pane.Command, text) {
		c := Candidate{Rule: rule.Name}
		p, err := e.plan(rule, tail)
		switch {
		case err != nil:
			c.Failure = err.Error()
		case len(p.keys) > 0:
			c.Keys = p.keys
		case p.digit != "":
			c.Option, c.Keys = p.optionText, []string{p.digit}
		default:
			c.Option = p.optionText
			c.Keys = append(append([]string{}, p.moves...), p.submit)
		}
		ex.Candidates = append(ex.Candidates, c)
	}
	return ex
}
