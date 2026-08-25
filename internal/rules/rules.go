// Package rules defines the declarative prompt-recognition ruleset.
//
// A rule answers two separate questions, and keeping them separate is what
// makes unattended operation safe:
//
//   - Does this pane show the prompt I care about? (all / any / none)
//   - Which option should be chosen? (options, in order of preference)
//
// A rule that recognises the prompt but cannot find any of its preferred
// options does nothing at all. It never falls back to a blind keypress.
package rules

import (
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/wboudy/agentsitter/internal/config"
)

//go:embed default.toml
var defaultTOML string

// DefaultTOML returns the built-in ruleset source, for `agentsitter rules --dump`.
func DefaultTOML() string { return defaultTOML }

// Rule is one prompt-recognition entry as written in TOML.
type Rule struct {
	// Name identifies the rule. Redefining a name in a later file replaces the
	// earlier rule, which is how a built-in default gets overridden or disabled.
	Name        string `toml:"name"`
	Description string `toml:"description"`
	// Enabled defaults to true when omitted.
	Enabled *bool `toml:"enabled"`
	// Priority orders rules; higher runs first. Ties keep definition order.
	Priority int `toml:"priority"`

	// Commands restricts the rule to panes whose process name matches.
	Commands []string `toml:"commands"`

	// All patterns must match the pane text; Any needs at least one match;
	// None must not match at all.
	All  []string `toml:"all"`
	Any  []string `toml:"any"`
	None []string `toml:"none"`

	// Options are candidate answers in order of preference. The first pattern
	// that matches a visible line is chosen.
	Options []string `toml:"options"`
	// Keys sends literal tmux key names instead of choosing a menu option.
	// Use it for prompts that have no selectable list.
	Keys []string `toml:"keys"`
	// Confirm requires the highlight to be verified on the chosen option
	// before Enter is sent. Defaults to true; turning it off is discouraged.
	Confirm *bool `toml:"confirm"`
	// Submit is the key sent to accept the highlighted option.
	Submit string `toml:"submit"`
	// VerifyGone must stop matching after the answer, or the attempt counts as
	// a failure. Defaults to the rule's first Any/All pattern.
	VerifyGone string `toml:"verify_gone"`
	// NoVerify suppresses the after-check entirely. It is for prompts that
	// legitimately stay on screen after being answered, such as one whose
	// chosen option is "keep waiting".
	NoVerify bool `toml:"no_verify"`

	// MaxPerHour and Cooldown override the global limits for this rule.
	MaxPerHour int             `toml:"max_per_hour"`
	Cooldown   config.Duration `toml:"cooldown"`
}

// Compiled is a Rule with its patterns ready to use.
type Compiled struct {
	Rule
	commandRes []*regexp.Regexp
	allRes     []*regexp.Regexp
	anyRes     []*regexp.Regexp
	noneRes    []*regexp.Regexp
	optionRes  []*regexp.Regexp
	verifyRe   *regexp.Regexp
}

// IsEnabled reports whether the rule participates in matching.
func (c *Compiled) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NeedsConfirm reports whether the highlight must be verified before Enter.
func (c *Compiled) NeedsConfirm() bool { return c.Confirm == nil || *c.Confirm }

// SubmitKey returns the key used to accept a highlighted option.
func (c *Compiled) SubmitKey() string {
	if c.Submit != "" {
		return c.Submit
	}
	return "Enter"
}

// OptionPatterns returns the compiled preference-ordered option patterns.
func (c *Compiled) OptionPatterns() []*regexp.Regexp { return c.optionRes }

// VerifyPattern returns the pattern that must vanish after answering, or nil.
func (c *Compiled) VerifyPattern() *regexp.Regexp { return c.verifyRe }

// MatchesCommand reports whether the rule applies to a pane process name.
func (c *Compiled) MatchesCommand(cmd string) bool {
	if len(c.commandRes) == 0 {
		return true
	}
	for _, re := range c.commandRes {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// Matches reports whether the pane text satisfies this rule's conditions.
func (c *Compiled) Matches(text string) bool {
	for _, re := range c.noneRes {
		if re.MatchString(text) {
			return false
		}
	}
	for _, re := range c.allRes {
		if !re.MatchString(text) {
			return false
		}
	}
	if len(c.anyRes) > 0 {
		hit := false
		for _, re := range c.anyRes {
			if re.MatchString(text) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	// A rule with no conditions at all would match every pane, which is never
	// what an author means.
	return len(c.allRes)+len(c.anyRes) > 0
}

// Set is an ordered collection of compiled rules.
type Set struct {
	Rules []*Compiled
}

// file is the on-disk shape of a rules file.
type file struct {
	Rules []Rule `toml:"rules"`
}

// Load builds a rule set from the built-in defaults plus any extra files,
// layered in order. A rule redefined by name in a later file replaces the
// earlier one, so users disable a default by redeclaring it with
// enabled = false rather than by editing the binary's copy.
func Load(extra ...string) (*Set, error) {
	var raw []Rule

	var base file
	if _, err := toml.Decode(defaultTOML, &base); err != nil {
		return nil, fmt.Errorf("parse built-in ruleset: %w", err)
	}
	raw = append(raw, base.Rules...)

	for _, path := range extra {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read rules %s: %w", path, err)
		}
		var f file
		if _, err := toml.Decode(string(data), &f); err != nil {
			return nil, fmt.Errorf("parse rules %s: %w", path, err)
		}
		raw = append(raw, f.Rules...)
	}
	return Compile(raw)
}

// Compile validates rules and prepares them for matching.
func Compile(raw []Rule) (*Set, error) {
	// Later definitions of the same name win, while the original position in
	// the list is preserved so ordering stays predictable.
	order := make([]string, 0, len(raw))
	byName := make(map[string]Rule, len(raw))
	for _, r := range raw {
		if strings.TrimSpace(r.Name) == "" {
			return nil, fmt.Errorf("rule %d: name is required", len(order)+1)
		}
		if _, seen := byName[r.Name]; !seen {
			order = append(order, r.Name)
		}
		byName[r.Name] = r
	}

	set := &Set{}
	for _, name := range order {
		c, err := compileRule(byName[name])
		if err != nil {
			return nil, err
		}
		set.Rules = append(set.Rules, c)
	}

	sort.SliceStable(set.Rules, func(i, j int) bool {
		return set.Rules[i].Priority > set.Rules[j].Priority
	})
	return set, nil
}

func compileRule(r Rule) (*Compiled, error) {
	c := &Compiled{Rule: r}
	var err error
	fields := []struct {
		name string
		src  []string
		dst  *[]*regexp.Regexp
	}{
		{"commands", r.Commands, &c.commandRes},
		{"all", r.All, &c.allRes},
		{"any", r.Any, &c.anyRes},
		{"none", r.None, &c.noneRes},
		{"options", r.Options, &c.optionRes},
	}
	for _, f := range fields {
		if *f.dst, err = compileAll(r.Name, f.name, f.src); err != nil {
			return nil, err
		}
	}

	if len(c.allRes)+len(c.anyRes) == 0 {
		return nil, fmt.Errorf("rule %q: needs at least one 'all' or 'any' pattern", r.Name)
	}
	if len(c.optionRes) == 0 && len(r.Keys) == 0 {
		return nil, fmt.Errorf("rule %q: needs either 'options' or 'keys'", r.Name)
	}
	if len(c.optionRes) > 0 && len(r.Keys) > 0 {
		return nil, fmt.Errorf("rule %q: set 'options' or 'keys', not both", r.Name)
	}

	if r.NoVerify {
		if r.VerifyGone != "" {
			return nil, fmt.Errorf("rule %q: set 'verify_gone' or 'no_verify', not both", r.Name)
		}
		return c, nil
	}

	verify := r.VerifyGone
	if verify == "" {
		// Default to the first recognition pattern: if the prompt text is
		// still on screen, the answer did not land.
		switch {
		case len(r.Any) > 0:
			verify = r.Any[0]
		case len(r.All) > 0:
			verify = r.All[0]
		}
	}
	if verify != "" {
		if c.verifyRe, err = regexp.Compile(verify); err != nil {
			return nil, fmt.Errorf("rule %q: bad verify_gone %q: %w", r.Name, verify, err)
		}
	}
	return c, nil
}

func compileAll(rule, field string, pats []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(pats))
	for _, p := range pats {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("rule %q: bad %s pattern %q: %w", rule, field, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// Candidates returns every enabled rule that recognises this pane, in priority
// order. The engine walks the list and uses the first whose preferred option it
// can actually locate on screen, so a narrowly-worded rule can sit in front of
// a general one without shadowing it when it fails to resolve.
func (s *Set) Candidates(paneCommand, text string) []*Compiled {
	var out []*Compiled
	for _, r := range s.Rules {
		if !r.IsEnabled() || !r.MatchesCommand(paneCommand) {
			continue
		}
		if r.Matches(text) {
			out = append(out, r)
		}
	}
	return out
}

// Enabled returns the rules that participate in matching.
func (s *Set) Enabled() []*Compiled {
	var out []*Compiled
	for _, r := range s.Rules {
		if r.IsEnabled() {
			out = append(out, r)
		}
	}
	return out
}
