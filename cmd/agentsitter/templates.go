package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// starterConfig is what `agentsitter init` writes. Everything in it is commented
// to its default, so the file doubles as documentation.
const starterConfig = `# agentsitter configuration.
#
# Every value below is set to its default. Delete anything you do not want to
# override. Start with:
#
#   agentsitter doctor
#   agentsitter once -dry-run
#
# and only then let it run for real.

# How often to sweep every target.
poll_interval = "3s"

# Rows pulled from the bottom of each pane, and how many of those rows rules
# are matched against. Keeping match_lines smaller anchors matching near the
# prompt so old scrollback cannot trigger a rule.
capture_lines = 80
match_lines   = 32

# A pane must look unchanged for this many consecutive polls before agentsitter
# will act on it. This keeps it off a screen that is still being drawn.
stable_polls = 2

# How long to wait after a keypress before reading the pane back.
settle = "800ms"

# Decide everything, send nothing. Useful for a first run.
dry_run = false

# Record prompts that no rule claimed, so you can turn them into rules.
learn = true

# Leave the pane you are looking at alone.
skip_active_pane = false

# While this file exists agentsitter keeps watching and logging but sends no
# keys. Managed by "agentsitter pause" and "agentsitter resume".
pause_file = "~/.local/state/agentsitter/paused"

state_file = "~/.local/state/agentsitter/state.json"
audit_file = "~/.local/state/agentsitter/audit.jsonl"
learn_dir  = "~/.local/state/agentsitter/learn"

# Extra rule files layered over the built-in ruleset. Redeclaring a built-in
# rule by name replaces it; redeclare with enabled = false to switch it off.
# See the built-in ruleset with: agentsitter rules -dump
rule_files = []

# Runs on notable events (a veto, a failed verification, an unrecognized
# prompt, a transport error) with the event as JSON on stdin. A few
# AGENTSITTER_* environment variables are set as well.
#   macOS:  notify_command = "osascript -e 'display notification \"'\"$AGENTSITTER_SUMMARY\"'\" with title \"agentsitter\"'"
#   Linux:  notify_command = "notify-send agentsitter \"$AGENTSITTER_SUMMARY\""
notify_command = ""

[limits]
# Bounds that turn a rule which matches but never resolves into a logged
# complaint rather than an endless keypress loop.
pane_cooldown       = "6s"
per_rule_per_hour   = 6
global_per_hour     = 60
max_verify_failures = 3
quarantine          = "10m"

[safety]
# Checked after a rule matches and before any key is sent. A pane whose text
# trips one of these is left for a human no matter what the ruleset says.
# Set enabled = false to turn the whole check off.
enabled = true
# Uncomment to replace the built-in list rather than use it.
# never_match = [
#   '(?i)\brm\s+-[a-z]*[rf]',
#   '(?i)\bgit\s+push\b.*--force',
#   '(?i)\bDROP\s+(TABLE|DATABASE|SCHEMA)\b',
# ]

# One target per tmux server. Declaring any target replaces the built-in
# local default, so list the local machine explicitly if you still want it.
[[targets]]
name   = "local"
socket = "default"
# Which panes count as agent panes, by foreground process name. These are
# regexes. The bare version-number pattern is not a typo: some agent CLIs run
# from a version-named path, so tmux reports the process as e.g. "2.1.243".
commands = ['^codex', '^claude', '^gemini', '^node$', '^\d+\.\d+\.\d+']
# sessions      = ['^agents$']
# exclude_panes = ['^scratch:']

# A remote machine. agentsitter shells out to ssh and turns on connection
# multiplexing, so the short poll interval does not mean a new connection
# every few seconds. Any host alias from your ssh_config works.
#
# Running agentsitter on the remote machine itself is usually better: it keeps
# working when your laptop sleeps or changes network.
#
# [[targets]]
# name   = "build-box"
# ssh    = "user@build-box.example"
# socket = "default"
# ssh_args = ["-i", "~/.ssh/id_ed25519"]
`

// defaultServiceKind picks the unit format for the current platform.
func defaultServiceKind() string {
	if runtime.GOOS == "darwin" {
		return "launchd"
	}
	return "systemd"
}

// home returns the user's home directory, or "~" when it cannot be determined.
func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "~"
	}
	return h
}

// launchdPlist renders a macOS user agent.
func launchdPlist(exe, configPath string) string {
	args := []string{exe, "run"}
	if configPath != "" {
		args = append(args, "-config", configPath)
	}
	var b strings.Builder
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	logPath := filepath.Join(home(), "Library", "Logs", "agentsitter.log")

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.github.wboudy.agentsitter</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
</dict>
</plist>
`, b.String(), xmlEscape(logPath), xmlEscape(logPath))
}

// systemdUnit renders a systemd user service.
func systemdUnit(exe, configPath string) string {
	cmd := exe + " run"
	if configPath != "" {
		cmd += " -config " + configPath
	}
	return fmt.Sprintf(`[Unit]
Description=agentsitter, unattended prompt watchdog for tmux agent panes
After=default.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=5
# agentsitter is a poller; it should never be the reason a box is busy.
Nice=10

[Install]
WantedBy=default.target
`, cmd)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
