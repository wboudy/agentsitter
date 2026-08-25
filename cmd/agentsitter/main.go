// Command agentsitter watches AI coding agents running in tmux and answers the
// prompts that would otherwise leave them blocked until a human looks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/wboudy/agentsitter/internal/audit"
	"github.com/wboudy/agentsitter/internal/config"
	"github.com/wboudy/agentsitter/internal/engine"
	"github.com/wboudy/agentsitter/internal/guard"
	"github.com/wboudy/agentsitter/internal/rules"
	"github.com/wboudy/agentsitter/internal/tmuxio"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `agentsitter watches AI coding agents in tmux and answers the prompts that
would otherwise leave them blocked until someone looks.

Usage:
  agentsitter <command> [flags]

Commands:
  run        Watch continuously (the daemon)
  once       Run a single sweep and exit
  panes      List panes on every target and whether they are watched
  rules      List the active rules, or print the built-in ruleset
  learn      Show prompts that no rule claimed yet
  explain    Show what agentsitter sees in one pane and what it would do
  doctor     Check config, rules, and target reachability
  pause      Stop sending keys; keep watching and logging
  resume     Undo pause
  status     Show paths, limits, and recent activity
  init       Write a starter config file
  service    Print a launchd or systemd unit for this machine
  version    Print the version

Run "agentsitter <command> -h" for the flags of a command.

Common flags:
  -config <path>   Config file (default $AGENTSITTER_CONFIG, then
                   $XDG_CONFIG_HOME/agentsitter/agentsitter.toml)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "once":
		err = cmdOnce(args)
	case "panes":
		err = cmdPanes(args)
	case "rules":
		err = cmdRules(args)
	case "learn":
		err = cmdLearn(args)
	case "explain":
		err = cmdExplain(args)
	case "doctor":
		err = cmdDoctor(args)
	case "pause":
		err = cmdPause(args, true)
	case "resume":
		err = cmdPause(args, false)
	case "status":
		err = cmdStatus(args)
	case "init":
		err = cmdInit(args)
	case "service":
		err = cmdService(args)
	case "version", "-v", "--version":
		fmt.Println("agentsitter", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "agentsitter: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "agentsitter:", err)
		os.Exit(1)
	}
}

// flagSet returns a flag set with the shared -config flag already registered.
func flagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	path := fs.String("config", "", "config file path")
	return fs, path
}

// build loads config and rules and wires up an engine.
func build(configPath string, override func(*config.Config)) (*engine.Engine, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	if override != nil {
		override(&cfg)
	}

	rs, err := rules.Load(cfg.RuleFiles...)
	if err != nil {
		return nil, err
	}
	g, err := guard.New(guard.Limits{
		PaneCooldown:      cfg.Limits.PaneCooldown.Duration,
		PerRulePerHour:    cfg.Limits.PerRulePerHour,
		GlobalPerHour:     cfg.Limits.GlobalPerHour,
		MaxVerifyFailures: cfg.Limits.MaxVerifyFailures,
		Quarantine:        cfg.Limits.Quarantine.Duration,
	}, cfg.StateFile)
	if err != nil {
		// A damaged state file is worth mentioning but not worth refusing to
		// start over.
		fmt.Fprintln(os.Stderr, "agentsitter:", err)
	}
	lg := audit.NewLogger(cfg.AuditFile, cfg.LearnDir, cfg.NotifyCommand)
	return engine.New(cfg, rs, g, lg, os.Stdout), nil
}

func cmdRun(args []string) error {
	fs, configPath := flagSet("run")
	dryRun := fs.Bool("dry-run", false, "decide everything but send no keys")
	verbose := fs.Bool("v", false, "log every decision, including throttled ones")
	interval := fs.Duration("interval", 0, "override the poll interval")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := build(*configPath, func(c *config.Config) {
		if *dryRun {
			c.DryRun = true
		}
		if *interval > 0 {
			c.PollInterval = config.Duration{Duration: *interval}
		}
	})
	if err != nil {
		return err
	}
	e.Verbose = *verbose

	cfg := e.Config()
	fmt.Printf("agentsitter %s watching %d target(s) every %s\n",
		version, len(cfg.Targets), cfg.PollInterval.Duration)
	if cfg.DryRun {
		fmt.Println("dry run: decisions are logged, no keys are sent")
	}
	if e.Paused() {
		fmt.Printf("paused: %s exists, no keys will be sent until it is removed\n", cfg.PauseFile)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := e.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	fmt.Println("agentsitter: stopped")
	return nil
}

func cmdOnce(args []string) error {
	fs, configPath := flagSet("once")
	dryRun := fs.Bool("dry-run", false, "decide everything but send no keys")
	verbose := fs.Bool("v", false, "log every decision, including throttled ones")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := build(*configPath, func(c *config.Config) {
		if *dryRun {
			c.DryRun = true
		}
		// A single sweep has no history to compare against, so requiring a
		// pane to repeat itself would make `once` do nothing at all.
		c.StablePolls = 1
	})
	if err != nil {
		return err
	}
	e.Verbose = *verbose

	res := e.Sweep(context.Background())
	for _, err := range res.Errors {
		fmt.Fprintln(os.Stderr, "agentsitter:", err)
	}
	if err := e.Guard().Save(); err != nil {
		fmt.Fprintln(os.Stderr, "agentsitter:", err)
	}
	fmt.Printf("\n%d pane(s) seen, %d watched, %d evaluated, %d decision(s)\n",
		res.Panes, res.Watched, res.Evaluated, len(res.Events))
	return nil
}

func cmdPanes(args []string) error {
	fs, configPath := flagSet("panes")
	all := fs.Bool("a", false, "include panes that are filtered out")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := build(*configPath, nil)
	if err != nil {
		return err
	}
	panes, errs := e.Inventory(context.Background())
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, "agentsitter:", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TARGET\tPANE\tID\tCOMMAND\tWATCHED\tNOTE")
	shown := 0
	for _, p := range panes {
		if !p.Watched && !*all {
			continue
		}
		shown++
		mark := "yes"
		if !p.Watched {
			mark = "no"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Target, p.Pane.Addr(), p.Pane.ID, p.Pane.Command, mark, p.Reason)
	}
	w.Flush()

	if shown == 0 {
		fmt.Println("\nNo agent panes found. Use -a to see every pane and why each was skipped.")
	}
	return nil
}

func cmdRules(args []string) error {
	fs, configPath := flagSet("rules")
	dump := fs.Bool("dump", false, "print the built-in ruleset and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dump {
		fmt.Print(rules.DefaultTOML())
		return nil
	}

	e, err := build(*configPath, nil)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PRIORITY\tSTATE\tNAME\tDESCRIPTION")
	for _, r := range e.Rules().Rules {
		state := "on"
		if !r.IsEnabled() {
			state = "off"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", r.Priority, state, r.Name, r.Description)
	}
	w.Flush()
	fmt.Println("\nPrint the built-in ruleset with: agentsitter rules -dump")
	return nil
}

func cmdLearn(args []string) error {
	fs, configPath := flagSet("learn")
	limit := fs.Int("n", 10, "how many captures to list")
	show := fs.String("show", "", "print one capture by filename")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.LearnDir == "" {
		return fmt.Errorf("learn_dir is not configured")
	}
	if *show != "" {
		body, err := os.ReadFile(filepath.Join(cfg.LearnDir, filepath.Base(*show)))
		if err != nil {
			return err
		}
		fmt.Print(string(body))
		return nil
	}

	entries, err := os.ReadDir(cfg.LearnDir)
	if os.IsNotExist(err) {
		fmt.Println("No unrecognized prompts recorded yet.")
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		fmt.Println("No unrecognized prompts recorded yet.")
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > *limit {
		names = names[:*limit]
	}
	fmt.Printf("%s\n\n", cfg.LearnDir)
	for _, n := range names {
		fmt.Println(" ", n)
	}
	fmt.Printf("\nInspect one with: agentsitter learn -show %s\n", names[0])
	return nil
}

func cmdDoctor(args []string) error {
	fs, configPath := flagSet("doctor")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *configPath
	if path == "" {
		path = config.DefaultPath()
	}
	expanded, _ := config.ExpandPath(path)
	if _, err := os.Stat(expanded); err == nil {
		fmt.Printf("config      %s\n", expanded)
	} else {
		fmt.Printf("config      %s (absent, using defaults)\n", expanded)
	}

	e, err := build(*configPath, nil)
	if err != nil {
		fmt.Printf("            FAILED: %v\n", err)
		return err
	}
	cfg := e.Config()
	fmt.Printf("rules       %d loaded, %d enabled\n", len(e.Rules().Rules), len(e.Rules().Enabled()))

	for _, dir := range []string{filepath.Dir(cfg.StateFile), filepath.Dir(cfg.AuditFile), cfg.LearnDir} {
		if dir == "" || dir == "." {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Printf("paths       FAILED: %s: %v\n", dir, err)
		}
	}
	fmt.Printf("paths       state=%s\n            audit=%s\n            learn=%s\n",
		cfg.StateFile, cfg.AuditFile, cfg.LearnDir)

	if cfg.DryRun {
		fmt.Println("mode        dry run, no keys will be sent")
	}
	if e.Paused() {
		fmt.Printf("mode        paused (%s exists)\n", cfg.PauseFile)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	failures := 0
	var resolved []config.Target
	for _, t := range cfg.Targets {
		got := e.Resolve(ctx, t)
		if len(got) == 0 {
			fmt.Printf("target      %-24s no tmux sockets found\n", t.Label())
		}
		resolved = append(resolved, got...)
	}
	for _, t := range resolved {
		client := &tmuxio.Client{
			SSH: t.SSH, SSHArgs: t.SSHArgs,
			Socket: t.Socket, SocketPath: t.SocketPath,
		}
		label := t.Label()
		if t.Remote() {
			label += " (ssh)"
		}
		ver, err := client.Version(ctx)
		switch {
		case err == nil:
			panes, _ := client.ListPanes(ctx)
			watched := 0
			for _, p := range panes {
				if t.MatchesSession(p.Session) && t.MatchesCommand(p.Command) &&
					!p.Dead && !p.InMode && !t.Excluded(p.Addr()) {
					watched++
				}
			}
			fmt.Printf("target      %-24s ok, tmux %s, %d pane(s), %d watched\n",
				label, ver, len(panes), watched)
		case strings.Contains(err.Error(), "no tmux server"):
			fmt.Printf("target      %-24s reachable, no tmux server running\n", label)
		default:
			failures++
			fmt.Printf("target      %-24s FAILED: %v\n", label, err)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d target(s) unreachable", failures)
	}
	return nil
}

func cmdPause(args []string, pause bool) error {
	name := "resume"
	if pause {
		name = "pause"
	}
	fs, configPath := flagSet(name)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.PauseFile == "" {
		return fmt.Errorf("pause_file is not configured")
	}

	if pause {
		if err := os.MkdirAll(filepath.Dir(cfg.PauseFile), 0o755); err != nil {
			return err
		}
		body := fmt.Sprintf("paused at %s\n", time.Now().Format(time.RFC3339))
		if err := os.WriteFile(cfg.PauseFile, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("paused: agentsitter keeps watching and logging but will send no keys\n(%s)\n", cfg.PauseFile)
		return nil
	}

	if err := os.Remove(cfg.PauseFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("resumed: agentsitter will answer prompts again")
	return nil
}

func cmdStatus(args []string) error {
	fs, configPath := flagSet("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := build(*configPath, nil)
	if err != nil {
		return err
	}
	cfg := e.Config()
	now := time.Now()

	state := "active"
	switch {
	case e.Paused():
		state = "paused"
	case cfg.DryRun:
		state = "dry run"
	}
	fmt.Printf("state          %s\n", state)
	fmt.Printf("poll           every %s, act after %d stable poll(s)\n",
		cfg.PollInterval.Duration, cfg.StablePolls)
	fmt.Printf("targets        %d\n", len(cfg.Targets))
	fmt.Printf("rules          %d enabled\n", len(e.Rules().Enabled()))
	fmt.Printf("limits         %d/hour globally, %d/hour per rule per pane, %s pane cooldown\n",
		cfg.Limits.GlobalPerHour, cfg.Limits.PerRulePerHour, cfg.Limits.PaneCooldown.Duration)
	fmt.Printf("safety         %s, %d never_match pattern(s)\n",
		onOff(cfg.Safety.Enabled), len(cfg.Safety.NeverMatch))
	fmt.Printf("actions        %d in the last hour\n", e.Guard().ActionsInLastHour(now))
	fmt.Printf("audit log      %s\n", cfg.AuditFile)

	snap := e.Guard().Snapshot()
	var quarantined []string
	for key, p := range snap.Panes {
		if !p.QuarantineUntil.IsZero() && now.Before(p.QuarantineUntil) {
			quarantined = append(quarantined,
				fmt.Sprintf("%s (%s remaining)", key, p.QuarantineUntil.Sub(now).Truncate(time.Second)))
		}
	}
	sort.Strings(quarantined)
	if len(quarantined) > 0 {
		fmt.Printf("quarantined    %s\n", strings.Join(quarantined, "\n               "))
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func cmdInit(args []string) error {
	fs, configPath := flagSet("init")
	force := fs.Bool("force", false, "overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *configPath
	if path == "" {
		path = config.DefaultPath()
	}
	expanded, err := config.ExpandPath(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(expanded); err == nil && !*force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", expanded)
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(expanded, []byte(starterConfig), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n\nNext:\n  agentsitter doctor\n  agentsitter once -dry-run\n", expanded)
	return nil
}

func cmdService(args []string) error {
	fs, configPath := flagSet("service")
	kind := fs.String("kind", defaultServiceKind(), "unit type: launchd or systemd")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "agentsitter"
	}
	cfgArg := ""
	if *configPath != "" {
		cfgArg = *configPath
	}

	switch *kind {
	case "launchd":
		fmt.Print(launchdPlist(exe, cfgArg))
		fmt.Fprint(os.Stderr, "\n# Save to ~/Library/LaunchAgents/com.github.wboudy.agentsitter.plist, then:\n"+
			"#   launchctl load -w ~/Library/LaunchAgents/com.github.wboudy.agentsitter.plist\n")
	case "systemd":
		fmt.Print(systemdUnit(exe, cfgArg))
		fmt.Fprint(os.Stderr, "\n# Save to ~/.config/systemd/user/agentsitter.service, then:\n"+
			"#   systemctl --user daemon-reload && systemctl --user enable --now agentsitter\n")
	default:
		return fmt.Errorf("unknown service kind %q, want launchd or systemd", *kind)
	}
	return nil
}

func cmdExplain(args []string) error {
	fs, configPath := flagSet("explain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentsitter explain <pane-id|session:window.pane>")
	}

	e, err := build(*configPath, nil)
	if err != nil {
		return err
	}
	ex, err := e.Explain(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}

	fmt.Printf("pane      %s %s (%s) on %s\n", ex.Pane.Addr(), ex.Pane.ID, ex.Pane.Command, ex.Target)
	fmt.Printf("selected  %s\n", describeIndex(ex.Selected))
	fmt.Printf("selector  %v\n", ex.Selector)
	if ex.Vetoed != "" {
		fmt.Printf("vetoed    safety.never_match matched %s\n", ex.Vetoed)
	}

	fmt.Printf("\nvisible tail (H=highlighted, M=marker glyph, N=list number):\n")
	for i, line := range ex.Tail.Lines {
		flags := []byte("---")
		if line.Highlighted {
			flags[0] = 'H'
		}
		if line.Marker {
			flags[1] = 'M'
		}
		if ex.Tail.OptionNumber(i) > 0 {
			flags[2] = 'N'
		}
		cursor := " "
		if i == ex.Selected {
			cursor = ">"
		}
		fmt.Printf("%s %s %3d | %s\n", cursor, string(flags), i, line.Text)
	}

	fmt.Printf("\nrules matching this screen: %d\n", len(ex.Candidates))
	for _, c := range ex.Candidates {
		if c.Failure != "" {
			fmt.Printf("  %-32s cannot answer: %s\n", c.Rule, c.Failure)
			continue
		}
		fmt.Printf("  %-32s would send %s", c.Rule, strings.Join(c.Keys, " "))
		if c.Option != "" {
			fmt.Printf("  to choose %q", strings.TrimSpace(c.Option))
		}
		fmt.Println()
	}
	if len(ex.Candidates) == 0 && ex.Selector {
		fmt.Println("  (none; this screen would be recorded under learn_dir)")
	}
	return nil
}

func describeIndex(i int) string {
	if i < 0 {
		return "nothing is highlighted"
	}
	return fmt.Sprintf("line %d", i)
}
