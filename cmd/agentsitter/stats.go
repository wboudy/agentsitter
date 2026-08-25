package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/wboudy/agentsitter/internal/audit"
	"github.com/wboudy/agentsitter/internal/config"
)

// cmdStats summarises the audit log: how often prompts actually appear, which
// ones, and on which agents.
func cmdStats(args []string) error {
	fs, configPath := flagSet("stats")
	since := fs.Duration("since", 24*time.Hour, "how far back to look")
	asJSON := fs.Bool("json", false, "emit the summary as JSON")
	top := fs.Int("top", 10, "how many rules and panes to list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	events, err := readEvents(cfg.AuditFile)
	if err != nil {
		return err
	}

	cutoff := time.Now().Add(-*since)
	sum := summarize(events, cutoff, *since)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sum)
	}
	sum.render(os.Stdout, *top)
	return nil
}

// readEvents loads the audit log. A missing log is an empty history, not an
// error: it just means nothing has happened yet.
func readEvents(path string) ([]audit.Event, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []audit.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev audit.Event
		// A truncated final line is possible if the daemon is writing right
		// now; skip it rather than failing the whole report.
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, sc.Err()
}

// Summary is the aggregated picture of a window of audit history.
type Summary struct {
	Window     string         `json:"window"`
	From       time.Time      `json:"from"`
	To         time.Time      `json:"to"`
	Total      int            `json:"total"`
	Outcomes   map[string]int `json:"outcomes"`
	ByRule     map[string]int `json:"answered_by_rule"`
	ByPane     map[string]int `json:"answered_by_pane"`
	ByHour     map[int]int    `json:"answered_by_hour"`
	Answered   int            `json:"answered"`
	FirstEvent time.Time      `json:"first_event,omitzero"`
	LastEvent  time.Time      `json:"last_event,omitzero"`

	window time.Duration
}

// summarize aggregates events at or after cutoff.
func summarize(events []audit.Event, cutoff time.Time, window time.Duration) Summary {
	s := Summary{
		Window:   window.String(),
		From:     cutoff,
		To:       time.Now(),
		Outcomes: map[string]int{},
		ByRule:   map[string]int{},
		ByPane:   map[string]int{},
		ByHour:   map[int]int{},
		window:   window,
	}
	for _, ev := range events {
		if ev.Time.Before(cutoff) {
			continue
		}
		s.Total++
		s.Outcomes[string(ev.Outcome)]++
		if s.FirstEvent.IsZero() || ev.Time.Before(s.FirstEvent) {
			s.FirstEvent = ev.Time
		}
		if ev.Time.After(s.LastEvent) {
			s.LastEvent = ev.Time
		}
		if ev.Outcome != audit.OutcomeAnswered {
			continue
		}
		s.Answered++
		if ev.Rule != "" {
			s.ByRule[ev.Rule]++
		}
		label := ev.PaneAddr
		if ev.Command != "" {
			label += "  (" + ev.Command + ")"
		}
		s.ByPane[label]++
		s.ByHour[ev.Time.Hour()]++
	}
	return s
}

// render writes the human-readable report.
func (s Summary) render(w *os.File, top int) {
	fmt.Fprintf(w, "window     last %s  (%s to %s)\n",
		humanInterval(s.window), s.From.Format("Jan 2 15:04"), s.To.Format("Jan 2 15:04"))

	if s.Total == 0 {
		fmt.Fprintln(w, "\nNothing recorded in this window.")
		fmt.Fprintln(w, "\nIf agentsitter has only just started, that is expected: it logs a prompt")
		fmt.Fprintln(w, "when one appears, so an idle swarm produces an empty report. Widen the")
		fmt.Fprintln(w, "window with -since 7d once it has been running a while.")
		return
	}

	fmt.Fprintf(w, "prompts    %d answered", s.Answered)
	if s.Answered > 0 {
		fmt.Fprintf(w, ", about one every %s", humanInterval(s.window/time.Duration(s.Answered)))
		fmt.Fprintf(w, "  (%.1f per day)", float64(s.Answered)/s.window.Hours()*24)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "events     %d total\n", s.Total)
	for _, kv := range sortedCounts(s.Outcomes) {
		fmt.Fprintf(w, "  %-18s %d%s\n", kv.key, kv.n, outcomeNote(kv.key))
	}

	if len(s.ByRule) > 0 {
		fmt.Fprintln(w, "\nanswered by rule")
		for _, kv := range topN(sortedCounts(s.ByRule), top) {
			fmt.Fprintf(w, "  %-30s %d\n", kv.key, kv.n)
		}
	}
	if len(s.ByPane) > 0 {
		fmt.Fprintln(w, "\nanswered by agent")
		for _, kv := range topN(sortedCounts(s.ByPane), top) {
			fmt.Fprintf(w, "  %-30s %d\n", kv.key, kv.n)
		}
	}
	if len(s.ByHour) > 0 {
		fmt.Fprintln(w, "\nanswered by hour of day")
		peak := 0
		for _, n := range s.ByHour {
			if n > peak {
				peak = n
			}
		}
		hours := make([]int, 0, len(s.ByHour))
		for h := range s.ByHour {
			hours = append(hours, h)
		}
		sort.Ints(hours)
		for _, h := range hours {
			n := s.ByHour[h]
			bar := strings.Repeat("█", 1+(n*24)/max(peak, 1))
			fmt.Fprintf(w, "  %02d:00  %-26s %d\n", h, bar, n)
		}
	}
}

// outcomeNote explains the less obvious outcomes inline.
func outcomeNote(outcome string) string {
	switch audit.Outcome(outcome) {
	case audit.OutcomeUnresolved:
		return "  (recognized, but no acceptable option was on screen)"
	case audit.OutcomeUnknownPrompt:
		return "  (looked like a prompt, no rule claimed it; see `agentsitter learn`)"
	case audit.OutcomeVetoed:
		return "  (blocked by safety.never_match)"
	case audit.OutcomeThrottled:
		return "  (a rate limit or quarantine held it back)"
	case audit.OutcomeAborted:
		return "  (backed out before submitting; the screen moved)"
	case audit.OutcomeVerifyFailed:
		return "  (answered, but the prompt did not clear)"
	case audit.OutcomeDryRun:
		return "  (would have answered; no keys sent)"
	}
	return ""
}

// humanInterval renders a duration in the largest sensible unit.
func humanInterval(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f minutes", d.Minutes())
	default:
		return d.Round(time.Second).String()
	}
}

type kv struct {
	key string
	n   int
}

// sortedCounts orders a count map by descending count, then by key.
func sortedCounts(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, n := range m {
		out = append(out, kv{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].key < out[j].key
	})
	return out
}

func topN(items []kv, n int) []kv {
	if n > 0 && len(items) > n {
		return items[:n]
	}
	return items
}
