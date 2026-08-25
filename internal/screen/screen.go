// Package screen parses raw tmux pane captures into structured lines.
//
// Terminal UIs mark the selected item in a menu with SGR attributes (reverse
// video, or a non-default background) rather than with plain text, so a
// plain-text capture cannot tell you which option the cursor is sitting on.
// Parsing the escape sequences lets agentsitter confirm the highlight landed on
// the intended option before it commits to pressing Enter.
package screen

import (
	"regexp"
	"strconv"
	"strings"
)

// Line is a single row of a pane capture.
type Line struct {
	Raw         string // original row, escape sequences intact
	Text        string // row with all escape sequences removed
	Highlighted bool   // reverse video or a non-default background covered visible text
	Marker      bool   // row begins with a menu pointer glyph
}

// Selected reports whether this line appears to be the active menu entry.
func (l Line) Selected() bool { return l.Highlighted || l.Marker }

// Screen is an ordered set of parsed lines, top row first.
type Screen struct {
	Lines []Line
}

// markerGlyphs are pointer characters that agent TUIs use to mark the active
// menu entry. It deliberately excludes ">" and "›": Codex draws its idle
// composer as "› Ask Codex to do anything", and treating that as a
// selection would make every idle pane look like an open menu.
const markerGlyphs = "❯▶▸●◆➤"

var (
	markerRe  = regexp.MustCompile(`^\s*[` + markerGlyphs + `]\s+\S`)
	numberRe  = regexp.MustCompile(`^\s*(?:[` + markerGlyphs + `]\s*)?(\d+)[.)]\s+\S`)
	optionRe  = regexp.MustCompile(`^\s*(?:[` + markerGlyphs + `]\s*)?(?:\d+[.)]\s+|\[\d+\]\s+)?\S`)
	digitsRe  = regexp.MustCompile(`\d+`)
	spacesRe  = regexp.MustCompile(`[ \t]+`)
	spinnerRe = regexp.MustCompile(`[\x{2800}-\x{28ff}\x{25d0}-\x{25d3}\x{25e2}-\x{25e5}]`)
	asciiSpin = regexp.MustCompile(`(^|\s)[|/\\-](\s|$)`)
	yesNoRe   = regexp.MustCompile(`(?i)\((?:y/n|yes/no)\)|\[y/n\]|press enter to|esc to (?:dismiss|cancel|skip)`)
)

// Parse splits a raw capture into lines and records SGR highlighting per line.
func Parse(raw string) Screen {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	rows := strings.Split(raw, "\n")
	lines := make([]Line, 0, len(rows))
	for _, row := range rows {
		text, hl := scanRow(row)
		lines = append(lines, Line{
			Raw:         row,
			Text:        strings.TrimRight(text, " \t"),
			Highlighted: hl,
			Marker:      markerRe.MatchString(text),
		})
	}
	return Screen{Lines: lines}
}

// StripANSI removes escape sequences and returns the visible text.
func StripANSI(s string) string {
	out, _ := scanRow(s)
	return out
}

// sgrState tracks the highlight-relevant subset of SGR attributes.
type sgrState struct {
	reverse bool
	bgSet   bool
}

func (s *sgrState) on() bool { return s.reverse || s.bgSet }

// apply folds one SGR parameter list into the state.
func (s *sgrState) apply(params []int) {
	for i := 0; i < len(params); i++ {
		switch p := params[i]; {
		case p == 0:
			s.reverse, s.bgSet = false, false
		case p == 7:
			s.reverse = true
		case p == 27:
			s.reverse = false
		case p == 49:
			s.bgSet = false
		case p >= 40 && p <= 47, p >= 100 && p <= 107:
			s.bgSet = true
		case p == 48:
			// Extended background: 48;5;N or 48;2;R;G;B.
			s.bgSet = true
			i += extendedSkip(params, i)
		case p == 38:
			// Extended foreground. Its arguments are skipped so that a colour
			// component such as the 40 in "38;2;40;40;40" is not misread as a
			// background attribute.
			i += extendedSkip(params, i)
		}
	}
}

// extendedSkip returns how many parameters after index i belong to a 38/48
// extended-colour introducer.
func extendedSkip(params []int, i int) int {
	if i+1 >= len(params) {
		return 0
	}
	switch params[i+1] {
	case 5:
		return 2
	case 2:
		return 4
	}
	return 0
}

// scanRow strips escape sequences from one row and reports whether any visible,
// non-space character was drawn while a highlight attribute was active.
func scanRow(row string) (string, bool) {
	var b strings.Builder
	b.Grow(len(row))

	var st sgrState
	lit := false
	runes := []rune(row)
	n := len(runes)

	isFinal := func(r rune) bool { return r >= 0x40 && r <= 0x7e }

	// skipTo advances past a string-terminated escape sequence.
	skipTo := func(i int, stop func(int) (bool, int)) int {
		for i < n {
			if ok, adv := stop(i); ok {
				return i + adv
			}
			i++
		}
		return n
	}
	atST := func(k int) (bool, int) {
		if runes[k] == 0x1b && k+1 < n && runes[k+1] == '\\' {
			return true, 2
		}
		return false, 0
	}

	for i := 0; i < n; {
		r := runes[i]
		if r != 0x1b {
			b.WriteRune(r)
			if st.on() && r != ' ' && r != '\t' {
				lit = true
			}
			i++
			continue
		}
		if i+1 >= n {
			break // dangling ESC at end of row
		}
		switch runes[i+1] {
		case '[': // CSI parameters intermediates final
			j := i + 2
			for j < n && !isFinal(runes[j]) {
				j++
			}
			if j >= n {
				i = n
				break
			}
			if runes[j] == 'm' {
				st.apply(parseParams(string(runes[i+2 : j])))
			}
			i = j + 1
		case ']': // OSC terminated by BEL or ST
			i = skipTo(i+2, func(k int) (bool, int) {
				if runes[k] == 0x07 {
					return true, 1
				}
				return atST(k)
			})
		case 'P', '_', '^', 'X': // DCS / APC / PM / SOS terminated by ST
			i = skipTo(i+2, atST)
		default: // two-character escape
			i += 2
		}
	}
	return b.String(), lit
}

// parseParams converts an SGR parameter string into integers. An omitted
// parameter means zero, matching terminal behaviour. A nil return means the
// sequence carries no SGR meaning and should be ignored.
func parseParams(s string) []int {
	if s == "" {
		return []int{0}
	}
	switch s[0] {
	case '?', '>', '<', '=':
		return nil // private-mode introducer
	}
	var out []int
	for _, part := range strings.Split(s, ";") {
		for _, sub := range strings.Split(part, ":") {
			if sub == "" {
				out = append(out, 0)
				continue
			}
			v, err := strconv.Atoi(sub)
			if err != nil {
				return nil
			}
			out = append(out, v)
		}
	}
	return out
}

// Tail returns at most the last n lines.
func (s Screen) Tail(n int) Screen {
	if n <= 0 || n >= len(s.Lines) {
		return s
	}
	return Screen{Lines: s.Lines[len(s.Lines)-n:]}
}

// Text renders the screen back to plain text.
func (s Screen) Text() string {
	parts := make([]string, len(s.Lines))
	for i, l := range s.Lines {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}

// Find returns the index of the lowest line matching re, or -1. The lowest
// match wins because menus are drawn at the bottom of a pane and the same
// string may well appear earlier in scrollback.
func (s Screen) Find(re *regexp.Regexp) int {
	for i := len(s.Lines) - 1; i >= 0; i-- {
		if re.MatchString(s.Lines[i].Text) {
			return i
		}
	}
	return -1
}

// SelectedIndex returns the index of the active menu entry, or -1 when nothing
// looks selected. SGR highlighting is authoritative; pointer glyphs are a
// fallback for TUIs that colour nothing.
func (s Screen) SelectedIndex() int {
	for i := len(s.Lines) - 1; i >= 0; i-- {
		if s.Lines[i].Highlighted {
			return i
		}
	}
	for i := len(s.Lines) - 1; i >= 0; i-- {
		if s.Lines[i].Marker {
			return i
		}
	}
	return -1
}

// OptionNumber returns the leading list number of a line, or 0 when the line
// carries none. Numbered menus can be answered with a single keypress, which
// avoids walking the cursor entirely.
func (s Screen) OptionNumber(idx int) int {
	if idx < 0 || idx >= len(s.Lines) {
		return 0
	}
	m := numberRe.FindStringSubmatch(s.Lines[idx].Text)
	if m == nil {
		return 0
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return v
}

// LooksLikeSelector reports whether the screen plausibly shows an interactive
// prompt. It is intentionally loose: it only decides whether an unrecognised
// screen is worth recording for later rule authoring, never whether to act.
func (s Screen) LooksLikeSelector() bool {
	if yesNoRe.MatchString(s.Text()) {
		return true
	}
	if s.SelectedIndex() < 0 {
		return false
	}
	run := 0
	for _, l := range s.Lines {
		t := strings.TrimSpace(l.Text)
		switch {
		case t == "":
			// A blank row inside a menu does not break the run.
		case len(t) < 90 && optionRe.MatchString(l.Text):
			run++
			if run >= 2 {
				return true
			}
		default:
			run = 0
		}
	}
	return false
}

// Fingerprint returns a comparison key for deciding whether a pane has settled.
// Digits and spinner glyphs are erased so that a live elapsed-time counter or
// token gauge does not make an otherwise static menu look like it is still
// redrawing.
func Fingerprint(raw string) string {
	txt := StripANSI(raw)
	txt = spinnerRe.ReplaceAllString(txt, "")
	txt = asciiSpin.ReplaceAllString(txt, " ")
	txt = digitsRe.ReplaceAllString(txt, "#")
	txt = spacesRe.ReplaceAllString(txt, " ")
	lines := strings.Split(txt, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}
