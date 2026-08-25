package screen

import (
	"regexp"
	"strings"
	"testing"
)

const (
	esc = "\x1b"
	bel = "\x07"
)

func TestStripANSIRemovesCSIAndOSC(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"sgr", esc + "[1;31mred" + esc + "[0m", "red"},
		{"osc8_st", esc + "]8;;https://example.invalid" + esc + `\link` + esc + "]8;;" + esc + `\`, "link"},
		{"osc8_bel", esc + "]8;;https://example.invalid" + bel + "link" + esc + "]8;;" + bel, "link"},
		{"cursor_move", "a" + esc + "[2Kb", "ab"},
		{"dangling_esc", "tail" + esc, "tail"},
		{"private_mode", esc + "[?25lvisible", "visible"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripANSI(c.in); got != c.want {
				t.Fatalf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestHighlightDetection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"reverse_video", esc + "[7mSelected" + esc + "[0m", true},
		{"bg_256", esc + "[48;5;24mSelected" + esc + "[49m", true},
		{"bg_truecolor", esc + "[48;2;10;20;30mSelected" + esc + "[0m", true},
		{"bg_basic", esc + "[44mSelected" + esc + "[0m", true},
		{"plain", "Not selected", false},
		{"bold_only", esc + "[1mBold" + esc + "[0m", false},
		// A truecolor foreground carries components that overlap the basic
		// background range; they must not be mistaken for a background.
		{"fg_truecolor_with_bg_lookalike", esc + "[38;2;40;40;40mForeground" + esc + "[0m", false},
		{"fg_256", esc + "[38;5;6mForeground" + esc + "[39m", false},
		{"reverse_cleared", esc + "[7m" + esc + "[27mAfter", false},
		{"highlight_over_spaces_only", esc + "[7m   " + esc + "[0m", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Parse(c.in)
			if got := s.Lines[0].Highlighted; got != c.want {
				t.Fatalf("Highlighted = %v, want %v (line %q)", got, c.want, c.in)
			}
		})
	}
}

func TestMarkerExcludesCodexComposer(t *testing.T) {
	// Codex renders its idle input line with a single angle glyph. Treating it
	// as a menu pointer would make every idle pane look like an open menu.
	s := Parse("› Ask Codex to do anything")
	if s.Lines[0].Marker {
		t.Fatal("codex composer line must not count as a menu marker")
	}
	if got := Parse("❯ Yes, continue").Lines[0]; !got.Marker {
		t.Fatal("pointer glyph line should count as a menu marker")
	}
}

func TestSelectedIndexPrefersHighlightOverMarker(t *testing.T) {
	raw := strings.Join([]string{
		"❯ First option",
		esc + "[7mSecond option" + esc + "[0m",
	}, "\n")
	if got := Parse(raw).SelectedIndex(); got != 1 {
		t.Fatalf("SelectedIndex = %d, want 1 (SGR highlight is authoritative)", got)
	}
}

func TestSelectedIndexNoneWhenNothingSelected(t *testing.T) {
	if got := Parse("just\nsome\noutput").SelectedIndex(); got != -1 {
		t.Fatalf("SelectedIndex = %d, want -1", got)
	}
}

func TestFindReturnsLowestMatch(t *testing.T) {
	// The same phrase can appear in scrollback above the live menu; the menu is
	// always the lower occurrence.
	raw := strings.Join([]string{
		"earlier mention of Switch to another model",
		"...",
		"Switch to another model",
	}, "\n")
	re := regexp.MustCompile(`Switch to another model`)
	if got := Parse(raw).Find(re); got != 2 {
		t.Fatalf("Find = %d, want 2", got)
	}
}

func TestOptionNumber(t *testing.T) {
	s := Parse("  2. Keep current model\nnot numbered\n❯ 3) Third")
	if got := s.OptionNumber(0); got != 2 {
		t.Fatalf("OptionNumber(0) = %d, want 2", got)
	}
	if got := s.OptionNumber(1); got != 0 {
		t.Fatalf("OptionNumber(1) = %d, want 0", got)
	}
	if got := s.OptionNumber(2); got != 3 {
		t.Fatalf("OptionNumber(2) = %d, want 3", got)
	}
	if got := s.OptionNumber(99); got != 0 {
		t.Fatalf("OptionNumber(out of range) = %d, want 0", got)
	}
}

func TestFingerprintIgnoresVolatileCounters(t *testing.T) {
	a := "Context 24% used · weekly 72% left\n─ Worked for 15m 09s ─"
	b := "Context 25% used · weekly 71% left\n─ Worked for 15m 10s ─"
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatalf("fingerprints should match across counter churn:\n%q\n%q", Fingerprint(a), Fingerprint(b))
	}
	if Fingerprint(a) == Fingerprint("a completely different screen") {
		t.Fatal("fingerprints of unrelated screens should differ")
	}
}

func TestFingerprintIgnoresColourChanges(t *testing.T) {
	plain := "Keep current model"
	coloured := esc + "[7mKeep current model" + esc + "[0m"
	if Fingerprint(plain) != Fingerprint(coloured) {
		t.Fatal("fingerprint should be colour-insensitive so highlight motion does not read as churn")
	}
}

func TestLooksLikeSelector(t *testing.T) {
	menu := strings.Join([]string{
		"You've hit your usage limit for gpt-x.",
		hl("❯ Keep current model"),
		"  Switch to another model",
	}, "\n")
	if !Parse(menu).LooksLikeSelector() {
		t.Fatal("a highlighted multi-option block should look like a selector")
	}

	idle := strings.Join([]string{
		"  - Packaged self-test: PASS",
		"",
		"› Ask Codex to do anything",
		"",
		"  Context 24% used · weekly 72% left",
	}, "\n")
	if Parse(idle).LooksLikeSelector() {
		t.Fatal("an idle Codex pane must not look like a selector")
	}

	if !Parse("Overwrite the file? (y/n)").LooksLikeSelector() {
		t.Fatal("a y/n prompt should look like a selector")
	}
}

// idleAgentPane reproduces the shape of a real idle agent screen: ordinary
// output that happens to contain a numbered list, and a composer input line
// that the TUI draws with a background attribute so it reads as highlighted.
func idleAgentPane() string {
	return strings.Join([]string{
		"  The change touches three files.",
		"",
		"  1. Update the parser entry point.",
		"  2. Thread the new option through.",
		"  3. Add a regression test.",
		"",
		"─ Worked for 15m 09s ────────────────────",
		"",
		hl("› Ask the agent to do anything"),
		"",
		"  Context 24% used",
	}, "\n")
}

func hl(s string) string { return esc + "[7m" + s + esc + "[0m" }

func TestIdleAgentPaneIsNotASelector(t *testing.T) {
	// A lone highlighted line surrounded by blanks is not a menu, however
	// many numbered lines appear elsewhere in the output.
	if Parse(idleAgentPane()).LooksLikeSelector() {
		t.Fatal("an idle agent pane must not register as an open menu")
	}
}

func TestComposerDoesNotWinSelectionOverRealMenu(t *testing.T) {
	// If the composer were allowed to count as the selection, the distance to
	// the intended option would be measured from the wrong row and agentsitter
	// would walk the cursor to the wrong entry.
	raw := strings.Join([]string{
		"You've hit your usage limit.",
		"  Yes, switch models",
		hl("  No, keep current model"),
		"",
		hl("› Ask the agent to do anything"),
	}, "\n")

	s := Parse(raw)
	if got := s.SelectedIndex(); got != 2 {
		t.Fatalf("SelectedIndex = %d, want 2 (the menu entry, not the composer)", got)
	}
	if !s.LooksLikeSelector() {
		t.Fatal("a real menu should still register as a selector")
	}
}

func TestComposerStillUsableAsLastResort(t *testing.T) {
	// TUIs that genuinely point with an angle bracket keep working, because
	// the composer shape is deprioritised rather than ignored.
	s := Parse(hl("> The only highlighted line"))
	if got := s.SelectedIndex(); got != 0 {
		t.Fatalf("SelectedIndex = %d, want 0", got)
	}
}

func TestBlankLineBreaksAnOptionBlock(t *testing.T) {
	isolated := strings.Join([]string{"context above", "", hl("Alone"), "", "context below"}, "\n")
	if Parse(isolated).LooksLikeSelector() {
		t.Fatal("a highlighted line fenced by blanks is not an option block")
	}

	adjacent := strings.Join([]string{"context above", "", hl("Chosen"), "Sibling", ""}, "\n")
	if !Parse(adjacent).LooksLikeSelector() {
		t.Fatal("a highlighted line with an adjacent sibling is an option block")
	}
}

func TestLongProseLineIsNotAnOption(t *testing.T) {
	long := strings.Repeat("prose ", 30)
	raw := strings.Join([]string{"leading line", hl(long), "trailing line"}, "\n")
	if Parse(raw).LooksLikeSelector() {
		t.Fatal("a highlighted line too long to be a menu entry is not a selector")
	}
}

func TestPartialHighlightIsNotSelection(t *testing.T) {
	// Agent TUIs paint small floating affordances partway through a line of
	// ordinary prose. A menu entry, by contrast, is highlighted across its row.
	overlay := "  the allowlist there is nothing" + hl(" Jump to bottom (click) ") + "left to repair here"
	raw := strings.Join([]string{
		"  Decision three is repair. Today the plan says that if a check fails,",
		"  a model puts something back and we loop around and try again.",
		overlay,
	}, "\n")

	s := Parse(raw)
	if s.Lines[2].SpansHighlight() {
		t.Fatal("a highlighted fragment mid-line should not count as a spanning highlight")
	}
	if got := s.SelectedIndex(); got != -1 {
		t.Fatalf("SelectedIndex = %d, want -1", got)
	}
	if s.LooksLikeSelector() {
		t.Fatal("prose carrying an inline overlay is not a menu")
	}
}

func TestHighlightToEndOfRowCounts(t *testing.T) {
	// Only the label is highlighted, not the leading indent or pointer, which
	// is common. It still reaches the end of the row, so it is a selection.
	s := Parse("  ❯ " + hl("Keep current model"))
	if !s.Lines[0].SpansHighlight() {
		t.Fatal("a highlight running to the end of the row is a selection")
	}
}

func TestBoxedComposerIsNotAMenu(t *testing.T) {
	// One agent CLI draws its composer with the same pointer glyph it uses for
	// menu selection, inside a box. Text mid-typing must not read as a menu.
	raw := strings.Join([]string{
		"  Some earlier output from the agent.",
		"",
		"────────────────────────────────────────",
		"❯ we should rename these two pipelines",
		"  and document what the lint step does",
		"────────────────────────────────────────",
		"  bypass permissions on",
	}, "\n")

	s := Parse(raw)
	if !s.inComposerBlock(3) {
		t.Fatal("a pointer line fenced by rules should be recognised as a composer")
	}
	if s.LooksLikeSelector() {
		t.Fatal("a composer mid-typing must not register as an open menu")
	}
}

func TestBoxedMenuStillCountsWhenSeveralOptionsAreMarked(t *testing.T) {
	// A framed dialog is only treated as a composer when exactly one pointer
	// sits on the first line of the block. A real list is unaffected.
	raw := strings.Join([]string{
		"  Do you want to proceed?",
		"────────────────────────────────────────",
		hl("❯ Yes"),
		"❯ No",
		"────────────────────────────────────────",
	}, "\n")

	s := Parse(raw)
	if s.inComposerBlock(2) {
		t.Fatal("a block with several pointers is a menu, not a composer")
	}
	if !s.LooksLikeSelector() {
		t.Fatal("a framed menu should still register as a selector")
	}
}

func TestIsRuleLine(t *testing.T) {
	for _, in := range []string{
		"────────────────────────",
		"------------------------",
		"========================",
	} {
		if !isRuleLine(in) {
			t.Errorf("isRuleLine(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "---", "  Yes, continue", "─ Worked for 15m 09s ─"} {
		if isRuleLine(in) {
			t.Errorf("isRuleLine(%q) = true, want false", in)
		}
	}
}

func TestEchoedUserMessageIsNotAMenu(t *testing.T) {
	// Agent CLIs echo past user messages with the same pointer glyph they use
	// for menu selection. A wrapped paragraph is a long marked block; a menu
	// is a short one.
	lines := []string{"  Crunched for 1m 38s", "", hl("❯ we should rename these two pipelines and also")}
	for i := 0; i < 12; i++ {
		lines = append(lines, "  another wrapped continuation line of the same message")
	}
	if Parse(strings.Join(lines, "\n")).LooksLikeSelector() {
		t.Fatal("a long wrapped user message must not register as an open menu")
	}

	// The same shape truncated to menu length is a menu.
	short := strings.Join([]string{
		"  Do you want to proceed?",
		hl("❯ Yes"),
		"  No",
	}, "\n")
	if !Parse(short).LooksLikeSelector() {
		t.Fatal("a short marked block is still a menu")
	}
}

func TestWrappedProseBlockIsNotAMenu(t *testing.T) {
	// Agent CLIs echo queued and past user messages with the same pointer glyph
	// used for menu selection. Reflowed prose fills to the wrap width on every
	// line but the last; menu labels are short and vary in length.
	raw := strings.Join([]string{
		"  Ran 4 shell commands",
		"",
		hl("  ❯ so explain in one tight paragraph the build stages end to end"),
		"    so i can give some feedback and we can pivot the worker a bit and",
		"    do the same with the other build as well since it is not so clear",
		"    how to choose the environment spec decisions over there",
	}, "\n")
	if Parse(raw).LooksLikeSelector() {
		t.Fatal("a wrapped message block must not register as an open menu")
	}
}

func TestShortVariedOptionsAreStillAMenu(t *testing.T) {
	raw := strings.Join([]string{
		"Do you want to proceed?",
		hl("❯ Yes"),
		"  Yes, and don't ask again",
		"  No",
	}, "\n")
	if !Parse(raw).LooksLikeSelector() {
		t.Fatal("short varied options are a menu")
	}
}

func TestOptionNumberReadsPreselectedRow(t *testing.T) {
	// Codex marks the selected row with the same angle character it uses for
	// the composer. That glyph must not count as a selection marker, but the
	// list number behind it still has to parse, or a menu whose first option
	// is preselected cannot be answered at all.
	s := Parse("› 1. Yes, proceed (y)\n  2. No, and tell it what to do differently")
	if got := s.OptionNumber(0); got != 1 {
		t.Fatalf("OptionNumber(0) = %d, want 1", got)
	}
	if got := s.OptionNumber(1); got != 2 {
		t.Fatalf("OptionNumber(1) = %d, want 2", got)
	}
	if s.Lines[0].Marker {
		t.Fatal("the angle glyph must still not count as a menu marker")
	}
}

func TestRenderedDiffIsNotAMenu(t *testing.T) {
	// An added line in a rendered diff is painted edge to edge, exactly like a
	// selected menu entry. Agent output is full of diffs, so this would fire
	// constantly if extent alone decided the question.
	raw := strings.Join([]string{
		"    ⋮",
		"    624          public_path: public",
		hl("    625 +        published_answer: $db.published_answer"),
		"    626        output_format:",
	}, "\n")

	s := Parse(raw)
	if !s.isDiffLine(2) {
		t.Fatal("a numbered diff line should be recognised as diff output")
	}
	if got := s.SelectedIndex(); got != -1 {
		t.Fatalf("SelectedIndex = %d, want -1 (line %q)", got, s.Lines[got].Text)
	}
	if s.LooksLikeSelector() {
		t.Fatal("rendered diff output must not register as an open menu")
	}
}

func TestUnifiedDiffIsNotAMenu(t *testing.T) {
	raw := strings.Join([]string{
		" context line",
		hl("+added line one"),
		"+added line two",
		"-removed line",
	}, "\n")
	if Parse(raw).LooksLikeSelector() {
		t.Fatal("a unified diff hunk must not register as an open menu")
	}
}

func TestDashBulletedMenuStillCounts(t *testing.T) {
	// A lone dash-bulleted line is a plausible menu option, so the bare
	// diff shape must require a like-shaped neighbour before it disqualifies.
	raw := strings.Join([]string{
		"Do you want to proceed?",
		hl("- Yes, continue"),
		"  No",
	}, "\n")
	s := Parse(raw)
	if s.isDiffLine(1) {
		t.Fatal("a single dash-bulleted option is not diff output")
	}
	if !s.LooksLikeSelector() {
		t.Fatal("a dash-bulleted menu should still register")
	}
}
