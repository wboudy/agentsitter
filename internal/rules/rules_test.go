package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRulesetCompiles(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatalf("built-in ruleset must compile: %v", err)
	}
	if len(set.Rules) == 0 {
		t.Fatal("built-in ruleset is empty")
	}
	for _, r := range set.Rules {
		if r.Description == "" {
			t.Errorf("rule %q has no description", r.Name)
		}
	}
}

func TestKeepWaitingOutranksDowngradeRule(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var keepWaiting, decline int = -1, -1
	for i, r := range set.Rules {
		switch r.Name {
		case "prefer-keep-waiting":
			keepWaiting = i
		case "decline-model-downgrade":
			decline = i
		}
	}
	if keepWaiting < 0 || decline < 0 {
		t.Fatal("expected both rules present in defaults")
	}
	if keepWaiting > decline {
		t.Fatal("prefer-keep-waiting must be evaluated before decline-model-downgrade")
	}
}

func TestDowngradeRuleRecognizesUsageLimitScreen(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	screen := strings.Join([]string{
		"You've hit your usage limit for the current model.",
		"Switch to another model now, or wait for the limit to reset.",
		"",
		"  Yes, switch models",
		"❯ No, keep current model",
	}, "\n")

	got := set.Candidates("codex", screen)
	if len(got) == 0 {
		t.Fatal("no rule recognised a usage-limit screen")
	}
	if got[0].Name != "decline-model-downgrade" {
		t.Fatalf("first candidate = %q, want decline-model-downgrade", got[0].Name)
	}

	// The preferred option must actually locate the decline line.
	var matched bool
	for _, re := range got[0].OptionPatterns() {
		if re.MatchString("❯ No, keep current model") {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatal("decline rule did not match its own decline option line")
	}
}

func TestCandidatesIgnoreUnrelatedScreens(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	idle := "› Ask the agent to do anything\n\n  Context 24% used"
	if got := set.Candidates("codex", idle); len(got) != 0 {
		t.Fatalf("idle pane matched %d rule(s): %v", len(got), got[0].Name)
	}
}

func TestCommandFilterScopesRules(t *testing.T) {
	set, err := Compile([]Rule{{
		Name:     "only-codex",
		Commands: []string{`^codex$`},
		Any:      []string{"prompt text"},
		Options:  []string{"yes"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Candidates("codex", "prompt text")) != 1 {
		t.Fatal("rule should apply to a codex pane")
	}
	if len(set.Candidates("bash", "prompt text")) != 0 {
		t.Fatal("rule should not apply to a non-matching pane command")
	}
}

func TestNoneVetoes(t *testing.T) {
	set, err := Compile([]Rule{{
		Name:    "vetoed",
		Any:     []string{"proceed"},
		None:    []string{"production"},
		Options: []string{"yes"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Candidates("codex", "proceed with the change")) != 1 {
		t.Fatal("rule should match without the veto term")
	}
	if len(set.Candidates("codex", "proceed against production")) != 0 {
		t.Fatal("none pattern should veto the match")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want string
	}{
		{"no name", Rule{Any: []string{"x"}, Options: []string{"y"}}, "name is required"},
		{"no conditions", Rule{Name: "a", Options: []string{"y"}}, "at least one"},
		{"no action", Rule{Name: "a", Any: []string{"x"}}, "either 'options' or 'keys'"},
		{"both actions", Rule{Name: "a", Any: []string{"x"}, Options: []string{"y"}, Keys: []string{"Enter"}}, "not both"},
		{"bad regex", Rule{Name: "a", Any: []string{"(unclosed"}, Options: []string{"y"}}, "bad any pattern"},
		{"verify conflict", Rule{Name: "a", Any: []string{"x"}, Options: []string{"y"}, NoVerify: true, VerifyGone: "z"}, "not both"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Compile([]Rule{c.rule})
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestVerifyGoneDefaultsToRecognitionPattern(t *testing.T) {
	set, err := Compile([]Rule{{Name: "a", Any: []string{"the prompt"}, Options: []string{"yes"}}})
	if err != nil {
		t.Fatal(err)
	}
	re := set.Rules[0].VerifyPattern()
	if re == nil || !re.MatchString("the prompt is still here") {
		t.Fatal("verify_gone should default to the first recognition pattern")
	}

	set, err = Compile([]Rule{{Name: "a", Any: []string{"the prompt"}, Options: []string{"yes"}, NoVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	if set.Rules[0].VerifyPattern() != nil {
		t.Fatal("no_verify should suppress the after-check")
	}
}

func TestLayeredFileOverridesBuiltIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extra.toml")
	body := `
[[rules]]
name = "decline-model-downgrade"
description = "disabled locally"
enabled = false
any = ["placeholder"]
options = ["placeholder"]

[[rules]]
name = "local-rule"
description = "added locally"
priority = 500
any = ["custom prompt"]
options = ["yes"]
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range set.Enabled() {
		if r.Name == "decline-model-downgrade" {
			t.Fatal("redeclaring a rule with enabled = false should disable it")
		}
	}
	if len(set.Candidates("codex", "a custom prompt appeared")) != 1 {
		t.Fatal("locally added rule should match")
	}
	// Overriding must not duplicate the entry.
	seen := 0
	for _, r := range set.Rules {
		if r.Name == "decline-model-downgrade" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("rule appears %d times after override, want 1", seen)
	}
}
