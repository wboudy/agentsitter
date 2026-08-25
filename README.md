# agentsitter

Unattended prompt watchdog for AI coding agents running in tmux.

Agents that run for hours stop for questions. A model quota dialog offers to
drop you to a weaker model. A confirmation asks whether to proceed. Whatever
the reason, the agent sits there and does nothing until someone looks at the
pane. If you are running several agents at once, or any of them are on a
machine you are not watching, that idle time is invisible and it adds up.

agentsitter watches those panes and answers the prompts for you, according to a
ruleset you control. Its default policy is to never accept a downgrade: when an
agent is offered a weaker model, agentsitter declines and lets it ride the good
model to zero.

```
$ agentsitter panes
TARGET      PANE       ID   COMMAND  WATCHED  NOTE
work:agents agents:1.2 %7   codex    yes
work:agents agents:1.3 %3   claude   yes
work:agents agents:2.1 %9   btop     no       process btop is not an agent

$ agentsitter run
agentsitter 1.0.0 watching 1 target(s) every 3s
answered       work:agents agents:1.2 (%7) rule=decline-model-downgrade option="No, keep current model" keys=Down,Enter
```

## Install

```sh
go install github.com/wboudy/agentsitter/cmd/agentsitter@latest
```

Or build from a checkout:

```sh
git clone https://github.com/wboudy/agentsitter
cd agentsitter
make build          # ./dist/agentsitter
make install        # into $GOBIN, or $(go env GOPATH)/bin
```

The only runtime dependency is `tmux`. For remote targets you also need `ssh`.

## Quick start

```sh
agentsitter doctor          # check config, rules, and that targets are reachable
agentsitter panes           # confirm it can see your agent panes
agentsitter once -dry-run   # one sweep, decide everything, send nothing
agentsitter run             # watch continuously
```

Start with `-dry-run` and read what it decides before letting it type. If
something looks wrong, `agentsitter explain <pane>` shows exactly what agentsitter
sees and why it would act.

## How it decides

The design assumption is that typing into someone's live agent session is
dangerous and should be hard to get wrong. Four things follow from that.

**Enter is never pressed speculatively.** Answering a menu happens in two
steps. agentsitter moves the highlight to the option it wants, then re-reads the
pane to confirm the highlight actually landed there, and only then submits. If
the menu scrolled or repainted in between, the attempt is abandoned. The cost
of a race is a retry, not a wrong answer.

**Recognizing a prompt and choosing an answer are separate.** A rule says both
which prompt it matches and which options it prefers, in order. A rule that
recognizes a prompt but finds none of its preferred options on screen does
nothing at all. It never falls back to a guess. If nothing is highlighted and
the menu is not numbered, agentsitter declines to act.

**A pane must hold still first.** Panes are compared against their previous
state and must look unchanged for several consecutive polls before they are
eligible. A half-drawn menu is not answered. Digits and spinner glyphs are
ignored in that comparison so a live token counter does not make a static menu
look like it is still moving.

**Everything is bounded and logged.** Per-pane cooldowns, per-rule hourly caps,
and a global hourly cap mean a rule that matches a prompt it cannot dismiss
degrades into a logged complaint rather than an endless keypress loop. Three
failed verifications quarantine a pane. Every decision, including the ones that
declined to act, is written to a JSONL audit log before the keys are sent.

## The safety veto

`safety.never_match` is checked after a rule matches and before any key is
sent. It applies no matter which rule matched or how the rule was written. By
default it refuses to answer any prompt whose visible text mentions a recursive
delete, a force push, a schema drop, a credential request, and similar.

```toml
[safety]
enabled = true
never_match = ['(?i)\brm\s+-[a-z]*[rf]', '(?i)\bDROP\s+(TABLE|DATABASE)\b']
```

Set `enabled = false` to turn it off. Think about that one first: with it off,
agentsitter will answer a command-approval prompt affirmatively no matter what
command is quoted in it.

## Configuration

```sh
agentsitter init      # writes a fully commented config with every default shown
```

Config lives at `$XDG_CONFIG_HOME/agentsitter/agentsitter.toml`, or wherever
`$AGENTSITTER_CONFIG` points. Every setting has a working default, so the file is
optional.

A target is one tmux server. Declaring any target replaces the built-in local
default, so list the local machine explicitly if you still want it.

```toml
[[targets]]
name   = "local"
socket = "*"        # "*" means every socket with a live server
commands = ['^codex', '^claude', '^gemini', '^node$', '^\d+\.\d+\.\d+']
```

`socket = "*"` is the default and it matters more than it looks. Agents do not
always live on tmux's `default` socket, and a watcher that assumes they do
watches nothing while appearing to work.

Two things about that filter are deliberate. The agent names are prefix
matches because orchestrators routinely launch agents through a wrapper, so the
process in tmux is something like `codex-dispatch` rather than `codex`; an
anchored match would watch nothing while appearing to work. And the bare
version-number pattern is not a typo: some agent CLIs run their binary from a
version-named path, so tmux reports the process as something like `2.1.243`.

## Rules

See what is active, and read the built-in ruleset:

```sh
agentsitter rules
agentsitter rules -dump > my-rules.toml
```

A rule looks like this:

```toml
[[rules]]
name        = "decline-model-downgrade"
description = "Refuse model downgrade and spend-limit fallback offers."
priority    = 100
any = [
  "You've hit your usage limit",
  '(?i)switch to (?:another|a different|a smaller) model',
]
options = [
  '(?i)^\s*(?:[❯▶▸●◆➤]\s*)?(?:\d+[.)]\s*)?(?:no|not now|cancel)\b',
  '(?i)^\s*(?:[❯▶▸●◆➤]\s*)?(?:\d+[.)]\s*)?keep (?:current|using|my|the)',
]
verify_gone = '(?i)hit your usage limit'
```

`all`, `any`, and `none` decide whether the rule recognizes the screen.
`options` are candidate answers in preference order; the first one found on
screen wins, which is how agentsitter prefers a plain "Yes" over a "Yes, and don't
ask again" that would widen permissions permanently. `verify_gone` must stop
matching afterwards or the attempt counts as a failure. Patterns are Go RE2, so
there is no lookaround or backreference.

Point `rule_files` at your own file to layer rules on top of the built-ins.
Redeclaring a built-in rule by name replaces it, so this is also how you turn
one off:

```toml
[[rules]]
name = "confirm-proceed"
enabled = false
any = ["placeholder"]
options = ["placeholder"]
```

### Teaching it a new prompt

agentsitter records screens that look like a prompt but that no rule claims:

```sh
agentsitter learn                      # list them
agentsitter learn -show <file>         # read one, with its raw escape sequences
agentsitter explain %7                 # trace a live pane and see every flag
```

The recognition patterns in the built-in ruleset were taken from strings found
in shipped agent CLI binaries rather than guessed. The option label patterns
are deliberately broader, because label text is harder to pin down than the
prompt text that introduces it. That asymmetry is safe: a rule that recognizes
a prompt but matches none of its options does nothing and records the screen
for you.

## Remote machines

```toml
[[targets]]
name = "build-box"
ssh  = "user@build-box.example"
```

Any host alias from your `ssh_config` works. agentsitter turns on connection
multiplexing by default, so a three second poll interval does not mean a new
connection every three seconds, and it forces `BatchMode` because a background
daemon has no way to answer a password prompt.

Running agentsitter on the remote machine itself is usually better. A central
watcher stops working the moment your laptop sleeps or changes network, which
is exactly when you are least likely to notice.

## Running it as a service

```sh
agentsitter service > ~/Library/LaunchAgents/com.github.wboudy.agentsitter.plist
launchctl load -w ~/Library/LaunchAgents/com.github.wboudy.agentsitter.plist

agentsitter service -kind systemd > ~/.config/systemd/user/agentsitter.service
systemctl --user daemon-reload && systemctl --user enable --now agentsitter
```

## Stopping it

```sh
agentsitter pause     # keep watching and logging, send no keys
agentsitter resume
```

`pause` writes a file, so anything that can create a file can stop agentsitter
from typing, including a script, a hook, or you over ssh.

## Commands

| Command | What it does |
|---|---|
| `run` | Watch continuously |
| `once` | One sweep and exit; pair with `-dry-run` |
| `panes` | Every pane and whether it is watched (`-a` shows skip reasons) |
| `rules` | List active rules; `-dump` prints the built-in ruleset |
| `learn` | Prompts no rule claimed yet |
| `explain` | Trace one pane: flags per row, selection, matching rules |
| `doctor` | Check config, rules, and target reachability |
| `pause` / `resume` | Kill switch |
| `status` | Paths, limits, recent activity, quarantined panes |
| `init` | Write a starter config |
| `service` | Print a launchd or systemd unit |

## Notifications

`notify_command` runs on the events worth a human's attention: a safety veto, a
failed verification, an unrecognized prompt, a transport error. The event is
passed as JSON on stdin, and a few `AGENTSITTER_*` variables are set.

```toml
notify_command = "notify-send agentsitter \"$AGENTSITTER_SUMMARY\""
```

## What it deliberately will not do

Two rules ship disabled, and the reason is worth knowing before you enable
them. Sending Escape into an agent pane is not neutral: in several agent CLIs
it interrupts the turn in progress. A stray Enter lands in the composer and
submits whatever happens to be typed there. Ambient help text like "esc to
dismiss" is often on screen while an agent is working perfectly well, so
agentsitter will not act on it without a deliberate opt-in.

## Development

```sh
make test          # unit tests
make integration   # drives a real tmux server with a fake agent fixture
make check         # fmt, vet, and both test suites
make dist          # cross-compiled binaries
```

The integration test starts tmux on a socket named after the test process, so
it cannot touch a session you are using.

## License

MIT
