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

	// HighlightCount is how many visible, non-space characters were drawn
	// under a highlight attribute, and HighlightEnd is the position just past
	// the last of them. Together they say whether the highlight covers the row
	// or merely decorates part of it.
	HighlightCount int
	HighlightEnd   int
}

// SpansHighlight reports whether the highlight covers this row rather than
// decorating a fragment of it.
//
// A selected menu entry is highlighted across its label, usually to the end of
// the row. Agent TUIs also paint small floating affordances (a "jump to
// bottom" chip, an inline badge) partway through a line of ordinary prose. The
// difference between those two is extent, not colour.
func (l Line) SpansHighlight() bool {
	if l.HighlightCount == 0 {
		return false
	}
	n := len([]rune(strings.TrimRight(l.Text, " \t")))
	if n == 0 {
		return false
	}
	if l.HighlightEnd >= n {
		return true // runs to the end of the row
	}
	return float64(l.HighlightCount)/float64(n) >= 0.6
}

// Selected reports whether this line appears to be the active menu entry.
func (l Line) Selected() bool { return l.SpansHighlight() || l.Marker }

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
	digitsRe  = regexp.MustCompile(`\d+`)
	spacesRe  = regexp.MustCompile(`[ \t]+`)
	spinnerRe = regexp.MustCompile(`[\x{2800}-\x{28ff}\x{25d0}-\x{25d3}\x{25e2}-\x{25e5}]`)
	asciiSpin = regexp.MustCompile(`(^|\s)[|/\\-](\s|$)`)
	yesNoRe   = regexp.MustCompile(`(?i)\((?:y/n|yes/no)\)|\[y/n\]|press enter to|esc to (?:dismiss|cancel|skip)`)

	// composerRe matches an agent's text-input line. Agent CLIs commonly draw
	// it with a background attribute, which makes it look highlighted even
	// though nothing is selected and no menu is open.
	composerRe = regexp.MustCompile(`^\s*[\x{203a}>\x{00bb}]\s`)
)

// maxOptionWidth is the longest a line can be and still plausibly be a menu
// entry rather than prose.
const maxOptionWidth = 90

// maxOptionBlock is the largest run of lines that still plausibly reads as a
// list of choices. Agent CLIs echo past user messages with the same pointer
// glyph they use for menu selection, and a wrapped paragraph produces a long
// marked block. Menus are short.
const maxOptionBlock = 8

// wrapWidth is the length at which a line starts to look like reflowed prose
// rather than a menu label.
const wrapWidth = 55

// Parse splits a raw capture into lines and records SGR highlighting per line.
func Parse(raw string) Screen {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	rows := strings.Split(raw, "\n")
	lines := make([]Line, 0, len(rows))
	for _, row := range rows {
		text, count, end := scanRow(row)
		lines = append(lines, Line{
			Raw:            row,
			Text:           strings.TrimRight(text, " \t"),
			Highlighted:    count > 0,
			Marker:         markerRe.MatchString(text),
			HighlightCount: count,
			HighlightEnd:   end,
		})
	}
	return Screen{Lines: lines}
}

// StripANSI removes escape sequences and returns the visible text.
func StripANSI(s string) string {
	out, _, _ := scanRow(s)
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

// scanRow strips escape sequences from one row and measures how much visible,
// non-space text was drawn while a highlight attribute was active. It returns
// the plain text, the count of highlighted characters, and the position just
// past the last one.
func scanRow(row string) (string, int, int) {
	var b strings.Builder
	b.Grow(len(row))

	var st sgrState
	var count, end, col int
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
			col++
			if st.on() && r != ' ' && r != '\t' {
				count++
				end = col
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
	return b.String(), count, end
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

// ruleLineRe matches a horizontal rule drawn with box characters or dashes.
var ruleLineRe = regexp.MustCompile(`^[\x{2500}-\x{257f}\-_=~ ]{8,}$`)

// isRuleLine reports whether a line is a horizontal divider.
func isRuleLine(text string) bool {
	t := strings.TrimSpace(text)
	return len([]rune(t)) >= 8 && ruleLineRe.MatchString(t)
}

// breaksBlock reports whether a line ends a run of related rows. Blank lines
// separate blocks, and so do horizontal rules: a rule is the frame around a
// block rather than a member of it.
func breaksBlock(l Line) bool {
	return strings.TrimSpace(l.Text) == "" || isRuleLine(l.Text)
}

// blockBounds returns the run of related lines containing idx.
func (s Screen) blockBounds(idx int) (int, int) {
	start, end := idx, idx
	for start > 0 && !breaksBlock(s.Lines[start-1]) {
		start--
	}
	for end < len(s.Lines)-1 && !breaksBlock(s.Lines[end+1]) {
		end++
	}
	return start, end
}

// inComposerBlock reports whether idx sits inside a boxed text-input area.
//
// This exists because at least one agent CLI draws its composer with the very
// same pointer glyph used for menu selection, inside a box. Without this the
// text a user is halfway through typing reads as an open menu whose first
// entry is selected. A composer is recognised by its frame: a run of lines
// fenced above and below by horizontal rules, with the pointer on the first
// line of the run and nowhere else.
func (s Screen) inComposerBlock(idx int) bool {
	if idx < 0 || idx >= len(s.Lines) {
		return false
	}
	start, end := s.blockBounds(idx)
	if start == 0 || end == len(s.Lines)-1 {
		return false
	}
	if !isRuleLine(s.Lines[start-1].Text) || !isRuleLine(s.Lines[end+1].Text) {
		return false
	}
	markers := 0
	for i := start; i <= end; i++ {
		if s.Lines[i].Marker {
			markers++
		}
	}
	return markers == 1 && s.Lines[start].Marker
}

// SelectedIndex returns the index of the active menu entry, or -1 when nothing
// looks selected.
//
// Preference order matters. An agent's composer is often drawn with a
// background attribute or a pointer glyph, so it reads as selected even with
// no menu open. If it were allowed to win, a menu drawn elsewhere would have
// its cursor distance measured from the wrong row and agentsitter would walk the
// selection to the wrong option. Composer-shaped rows are therefore considered
// only as a last resort, which keeps TUIs that genuinely point with an angle
// bracket working.
func (s Screen) SelectedIndex() int {
	notComposer := func(i int, l Line) bool {
		return !composerRe.MatchString(l.Text) && !s.inComposerBlock(i)
	}
	passes := []func(int, Line) bool{
		func(i int, l Line) bool { return l.SpansHighlight() && notComposer(i, l) },
		func(i int, l Line) bool { return l.Marker && notComposer(i, l) },
		func(_ int, l Line) bool { return l.SpansHighlight() },
		func(_ int, l Line) bool { return l.Marker },
	}
	for _, pred := range passes {
		if i := s.findLast(pred); i >= 0 {
			return i
		}
	}
	return -1
}

// findLast returns the index of the lowest line satisfying pred, or -1.
func (s Screen) findLast(pred func(int, Line) bool) int {
	for i := len(s.Lines) - 1; i >= 0; i-- {
		if pred(i, s.Lines[i]) {
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
//
// The test is that the selected line sits in a contiguous block of at least
// two short lines. A blank row ends the block, which is what separates a real
// option list from a lone highlighted line floating in ordinary output.
func (s Screen) LooksLikeSelector() bool {
	if yesNoRe.MatchString(s.Text()) {
		return true
	}
	sel := s.SelectedIndex()
	if sel < 0 || !isOptionShaped(s.Lines[sel]) || s.inComposerBlock(sel) {
		return false
	}
	if n := s.blockSize(sel); n < 2 || n > maxOptionBlock {
		return false
	}
	start, end := s.blockBounds(sel)
	return !s.looksWrapped(start, end)
}

// isOptionShaped reports whether a line could be a menu entry: present, short,
// not a frame, and not the agent's own input line.
func isOptionShaped(l Line) bool {
	t := strings.TrimSpace(l.Text)
	return t != "" && len(t) < maxOptionWidth &&
		!isRuleLine(l.Text) && !composerRe.MatchString(l.Text)
}

// looksWrapped reports whether a block reads as a wrapped paragraph rather
// than a list of choices.
//
// Reflowed prose fills to the wrap width on every line but the last, while
// menu labels are short and vary in length. This is what separates a genuine
// menu from the message text an agent echoes back with the same pointer glyph
// it uses for selection.
func (s Screen) looksWrapped(start, end int) bool {
	n := end - start + 1
	if n < 3 {
		// Two lines are too few to show a wrap pattern, and a menu may well
		// have one long option.
		return false
	}
	long := 0
	for i := start; i <= end; i++ {
		if len([]rune(strings.TrimSpace(s.Lines[i].Text))) >= wrapWidth {
			long++
		}
	}
	return long >= n-1
}

// blockSize counts the contiguous run of option-shaped lines containing idx.
func (s Screen) blockSize(idx int) int {
	if idx < 0 || idx >= len(s.Lines) || !isOptionShaped(s.Lines[idx]) {
		return 0
	}
	n := 1
	for i := idx - 1; i >= 0 && isOptionShaped(s.Lines[i]); i-- {
		n++
	}
	for i := idx + 1; i < len(s.Lines) && isOptionShaped(s.Lines[i]); i++ {
		n++
	}
	return n
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
