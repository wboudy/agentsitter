// Package tmuxio runs tmux commands against a local or remote tmux server.
//
// Remote targets shell out to ssh. Connection multiplexing is enabled by
// default because agentsitter polls on a short interval, and without a shared
// control socket every poll would pay for a fresh TCP and authentication
// round trip.
package tmuxio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrNoServer means the tmux server is not running on the target. It is an
// ordinary condition, not a failure: a machine with no agents on it has no
// tmux server.
var ErrNoServer = errors.New("no tmux server running")

// Client talks to one tmux server.
type Client struct {
	// SSH is an ssh destination. Empty means the local machine.
	SSH string
	// SSHArgs are placed before agentsitter's own ssh options. ssh keeps the
	// first value it sees for a given option, so these win over the defaults.
	SSHArgs []string
	// Socket is a tmux socket name (tmux -L).
	Socket string
	// SocketPath is a tmux socket path (tmux -S). It takes priority.
	SocketPath string
	// Binary overrides the tmux executable name.
	Binary string
	// Timeout bounds a single tmux invocation.
	Timeout time.Duration
	// ControlPath is the ssh multiplexing socket. "%C" keeps it short enough
	// to stay under the unix socket path limit.
	ControlPath string
	// ControlPersist is how long an idle multiplexed connection is kept.
	ControlPersist string
}

// Pane is one tmux pane and the metadata agentsitter filters on.
type Pane struct {
	ID      string // unique pane id, such as "%7"
	Session string
	Window  string
	Index   string
	Command string // foreground process name
	Title   string
	Path    string
	Active  bool
	Dead    bool
	InMode  bool // pane is in copy mode, so its visible text is not live
}

// Addr is the human-readable pane address, "session:window.pane".
func (p Pane) Addr() string {
	return fmt.Sprintf("%s:%s.%s", p.Session, p.Window, p.Index)
}

const paneFormat = "#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_index}\t" +
	"#{pane_current_command}\t#{pane_active}\t#{pane_dead}\t#{pane_in_mode}\t" +
	"#{pane_current_path}\t#{pane_title}"

const paneFieldCount = 10

func (c *Client) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "tmux"
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// Remote reports whether this client reaches tmux over ssh.
func (c *Client) Remote() bool { return c.SSH != "" }

// Label identifies the client in logs.
func (c *Client) Label() string {
	if c.Remote() {
		return c.SSH
	}
	return "local"
}

// tmuxArgs returns the tmux invocation with its socket selection prefix.
func (c *Client) tmuxArgs(sub ...string) []string {
	args := []string{c.binary()}
	switch {
	case c.SocketPath != "":
		args = append(args, "-S", c.SocketPath)
	case c.Socket != "":
		args = append(args, "-L", c.Socket)
	}
	return append(args, sub...)
}

// command builds the process to execute for a tmux subcommand.
func (c *Client) command(ctx context.Context, sub ...string) *exec.Cmd {
	tmuxArgv := c.tmuxArgs(sub...)
	if !c.Remote() {
		return exec.CommandContext(ctx, tmuxArgv[0], tmuxArgv[1:]...)
	}

	// ssh joins its trailing arguments into a single string handed to the
	// remote login shell, so every token has to be quoted here.
	quoted := make([]string, len(tmuxArgv))
	for i, a := range tmuxArgv {
		quoted[i] = ShellQuote(a)
	}

	argv := append([]string{}, c.SSHArgs...)
	argv = append(argv,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+c.controlPath(),
		"-o", "ControlPersist="+c.controlPersist(),
		c.SSH, "--", strings.Join(quoted, " "),
	)
	return exec.CommandContext(ctx, "ssh", argv...)
}

func (c *Client) controlPath() string {
	if c.ControlPath != "" {
		return c.ControlPath
	}
	return "/tmp/agentsitter-%C"
}

func (c *Client) controlPersist() string {
	if c.ControlPersist != "" {
		return c.ControlPersist
	}
	return "60s"
}

// run executes a tmux subcommand and returns stdout.
func (c *Client) run(ctx context.Context, sub ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	cmd := c.command(ctx, sub...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	msg := strings.TrimSpace(stderr.String())
	if isNoServer(msg) {
		return nil, ErrNoServer
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: tmux %s timed out after %s", c.Label(), sub[0], c.timeout())
	}
	if msg == "" {
		msg = err.Error()
	}
	return nil, fmt.Errorf("%s: tmux %s: %s", c.Label(), strings.Join(sub, " "), msg)
}

func isNoServer(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no server running") ||
		strings.Contains(s, "error connecting to") ||
		strings.Contains(s, "no such file or directory") && strings.Contains(s, "tmux")
}

// Ping checks that the target's tmux server is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.run(ctx, "display-message", "-p", "#{version}")
	return err
}

// Version returns the remote tmux version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "display-message", "-p", "#{version}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ListPanes returns every pane on the target's tmux server.
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := c.run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	return ParsePanes(string(out)), nil
}

// ParsePanes turns list-panes output into Pane values. Malformed rows are
// skipped rather than failing the sweep, since one odd pane should not stop
// agentsitter from watching the rest.
func ParsePanes(out string) []Pane {
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The title is last and may itself contain tabs, so the split is
		// bounded and the remainder kept intact.
		f := strings.SplitN(line, "\t", paneFieldCount)
		if len(f) < paneFieldCount {
			continue
		}
		panes = append(panes, Pane{
			ID:      f[0],
			Session: f[1],
			Window:  f[2],
			Index:   f[3],
			Command: f[4],
			Active:  f[5] == "1",
			Dead:    f[6] == "1",
			InMode:  f[7] == "1",
			Path:    f[8],
			Title:   f[9],
		})
	}
	return panes
}

// Capture returns the bottom `lines` rows of a pane with escape sequences
// preserved, which is what makes highlight detection possible.
func (c *Client) Capture(ctx context.Context, paneID string, lines int) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	out, err := c.run(ctx, "capture-pane", "-p", "-e",
		"-t", paneID, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// SendKeys sends named keys such as "Up", "Down", or "Enter" to a pane.
func (c *Client) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	args := append([]string{"send-keys", "-t", paneID}, keys...)
	_, err := c.run(ctx, args...)
	return err
}

// SendLiteral sends text as literal characters rather than key names.
func (c *Client) SendLiteral(ctx context.Context, paneID, text string) error {
	_, err := c.run(ctx, "send-keys", "-t", paneID, "-l", text)
	return err
}

// ShellQuote wraps a string for safe use inside a remote shell command.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// tmux identifiers are sigil-prefixed (%pane, @window, $session), so those
	// characters are quoted too even though a POSIX shell would not expand
	// them. The cost is nothing and it keeps unusual remote shells out of the
	// picture.
	if !strings.ContainsAny(s, " \t\n\"'\\$`&;|<>()*?[]{}!#~^%@") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
