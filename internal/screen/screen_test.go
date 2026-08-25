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
		esc + "[7m❯ Keep current model" + esc + "[0m",
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
