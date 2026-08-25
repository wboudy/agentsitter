package tmuxio

import (
	"context"
	"strings"
	"testing"
)

func TestParsePanes(t *testing.T) {
	out := strings.Join([]string{
		"%7\tagents\t1\t2\tcodex\t1\t0\t0\t/home/u/proj\tresearcher",
		"%3\tagents\t1\t3\t2.1.243\t0\t0\t1\t/home/u/proj\ttitle\twith\ttabs",
		"malformed row",
		"",
	}, "\n")

	panes := ParsePanes(out)
	if len(panes) != 2 {
		t.Fatalf("parsed %d panes, want 2 (malformed rows are skipped)", len(panes))
	}

	first := panes[0]
	if first.ID != "%7" || first.Command != "codex" || !first.Active || first.Dead || first.InMode {
		t.Fatalf("unexpected first pane: %+v", first)
	}
	if got := first.Addr(); got != "agents:1.2" {
		t.Fatalf("Addr = %q, want agents:1.2", got)
	}

	second := panes[1]
	if second.Command != "2.1.243" {
		t.Fatalf("Command = %q, want a version-named binary", second.Command)
	}
	if !second.InMode {
		t.Fatal("second pane should be flagged as in copy mode")
	}
	// A title containing tabs must survive intact, since it is the last field.
	if second.Title != "title\twith\ttabs" {
		t.Fatalf("Title = %q, want tabs preserved", second.Title)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":             "''",
		"plain":        "plain",
		"list-panes":   "list-panes",
		"has space":    "'has space'",
		"it's":         `'it'\''s'`,
		"semi;colon":   "'semi;colon'",
		"$(subshell)":  "'$(subshell)'",
		"back`tick`":   "'back`tick`'",
		"#{pane_id}":   "'#{pane_id}'",
		"a\tb":         "'a\tb'",
		"tilde~expand": "'tilde~expand'",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTmuxArgsSocketSelection(t *testing.T) {
	byName := (&Client{Socket: "agents"}).tmuxArgs("list-panes")
	if strings.Join(byName, " ") != "tmux -L agents list-panes" {
		t.Fatalf("got %v", byName)
	}

	// An explicit socket path takes priority over a socket name.
	byPath := (&Client{Socket: "agents", SocketPath: "/tmp/s"}).tmuxArgs("list-panes")
	if strings.Join(byPath, " ") != "tmux -S /tmp/s list-panes" {
		t.Fatalf("got %v", byPath)
	}

	bare := (&Client{}).tmuxArgs("kill-server")
	if strings.Join(bare, " ") != "tmux kill-server" {
		t.Fatalf("got %v", bare)
	}
}

func TestLocalCommandInvokesTmuxDirectly(t *testing.T) {
	c := &Client{Socket: "agents"}
	cmd := c.command(context.Background(), "capture-pane", "-p")
	if got := strings.Join(cmd.Args, " "); got != "tmux -L agents capture-pane -p" {
		t.Fatalf("local command = %q", got)
	}
}

func TestRemoteCommandQuotesAndMultiplexes(t *testing.T) {
	c := &Client{SSH: "user@example.invalid", Socket: "agents"}
	cmd := c.command(context.Background(), "capture-pane", "-p", "-t", "%7")

	if cmd.Args[0] != "ssh" {
		t.Fatalf("expected ssh, got %q", cmd.Args[0])
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"BatchMode=yes",      // a daemon cannot answer a password prompt
		"ControlMaster=auto", // short poll interval needs a shared connection
		"ControlPersist=60s",
		"user@example.invalid",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("remote command missing %q: %s", want, joined)
		}
	}

	// The remote command is one argument, fully quoted.
	remote := cmd.Args[len(cmd.Args)-1]
	if remote != "tmux -L agents capture-pane -p -t '%7'" {
		t.Fatalf("remote command = %q", remote)
	}
}

func TestUserSSHArgsPrecedeDefaults(t *testing.T) {
	// ssh keeps the first value it sees for an option, so user arguments have
	// to come before agentsitter's defaults to be able to override them.
	c := &Client{SSH: "host", SSHArgs: []string{"-o", "ConnectTimeout=1"}}
	cmd := c.command(context.Background(), "list-panes")

	userIdx := indexOf(cmd.Args, "ConnectTimeout=1")
	defaultIdx := indexOf(cmd.Args, "ConnectTimeout=10")
	if userIdx < 0 || defaultIdx < 0 {
		t.Fatalf("expected both values present: %v", cmd.Args)
	}
	if userIdx > defaultIdx {
		t.Fatal("user ssh_args must be placed before agentsitter defaults")
	}
}

func TestIsNoServer(t *testing.T) {
	if !isNoServer("no server running on /tmp/tmux-1000/default") {
		t.Fatal("should recognise a missing tmux server")
	}
	if isNoServer("can't find pane: %99") {
		t.Fatal("an ordinary tmux error is not a missing server")
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

func TestTailLines(t *testing.T) {
	body := "a\nb\nc\nd\ne"
	if got := TailLines(body, 2); got != "d\ne" {
		t.Fatalf("TailLines = %q", got)
	}
	if got := TailLines(body, 99); got != body {
		t.Fatalf("asking for more lines than exist should return everything, got %q", got)
	}
	if got := TailLines(body, 0); got != body {
		t.Fatalf("a non-positive count should return everything, got %q", got)
	}
}

func TestRemoteCommandIsCleared(t *testing.T) {
	// A host alias may carry RemoteCommand in ssh_config to auto-attach to a
	// session. ssh then refuses to run a command alongside it, failing with
	// "Cannot execute command-line and remote command". Clearing it means a
	// user does not have to keep a second bare alias just for this tool.
	c := &Client{SSH: "attach-alias"}
	joined := strings.Join(c.command(context.Background(), "list-panes").Args, " ")
	for _, want := range []string{"RemoteCommand=none", "RequestTTY=no"} {
		if !strings.Contains(joined, want) {
			t.Errorf("remote command missing %q: %s", want, joined)
		}
	}
}
