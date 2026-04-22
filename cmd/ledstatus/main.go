// ledstatus is the client CLI for ledstatusd. It's designed to be wired into
// Claude Code hooks: `ledstatus hook <state>` reads the hook's JSON from
// stdin and forwards a message to the daemon.
//
// Connection failures are silent (logged to stderr, exit 0) so a down daemon
// never breaks a Claude turn.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmurray/ledstatus/internal/protocol"
)

const usage = `ledstatus — send Claude session state to ledstatusd

usage:
  ledstatus hook <state>            for use as a Claude Code hook; reads session_id from stdin JSON
  ledstatus set  <state> [flags]    manually set state (uses --claude-id / $CLAUDE_ID / fallback)
  ledstatus off  [flags]            clear this session's state
  ledstatus test                    cycle through all states locally (no claude needed)

states:
  thinking              solid green
  waiting_permission    pulsing blue
  waiting_input         pulsing red
  off                   remove session from the daemon

environment:
  LEDSTATUS_ADDR   where to reach the daemon. Default $XDG_RUNTIME_DIR/ledstatus.sock.
                   Forms: /tmp/ledstatus.sock  unix:///path  tcp://host:port  host:port
  CLAUDE_ID        override session id for 'set' and 'off'.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "hook":
		runHook(os.Args[2:])
	case "set":
		runSet(os.Args[2:])
	case "off":
		runOff(os.Args[2:])
	case "test":
		runTest(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func runHook(args []string) {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "ledstatus hook: missing <state>")
		os.Exit(2)
	}
	state := protocol.State(fs.Arg(0))

	var hi struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}
	_ = json.NewDecoder(os.Stdin).Decode(&hi) // tolerate empty/invalid stdin

	id := hi.SessionID
	if id == "" {
		id = fallbackID()
	}
	send(protocol.Message{ClaudeID: id, State: state, Cwd: hi.Cwd})
}

func runSet(args []string) {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	idFlag := fs.String("claude-id", os.Getenv("CLAUDE_ID"), "claude session id")
	cwd := fs.String("cwd", "", "working directory tag (optional)")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "ledstatus set: missing <state>")
		os.Exit(2)
	}
	id := *idFlag
	if id == "" {
		id = fallbackID()
	}
	send(protocol.Message{ClaudeID: id, State: protocol.State(fs.Arg(0)), Cwd: *cwd})
}

func runOff(args []string) {
	fs := flag.NewFlagSet("off", flag.ExitOnError)
	idFlag := fs.String("claude-id", os.Getenv("CLAUDE_ID"), "claude session id")
	fs.Parse(args)
	id := *idFlag
	if id == "" {
		id = fallbackID()
	}
	send(protocol.Message{ClaudeID: id, State: protocol.StateOff})
}

// runTest cycles through every state using a dedicated test session id, so
// you can eyeball the LED without running Claude.
func runTest(_ []string) {
	id := "ledstatus-test-" + time.Now().Format("150405")
	seq := []struct {
		state protocol.State
		label string
	}{
		{protocol.StateThinking, "thinking (solid green)"},
		{protocol.StateWaitingPermission, "waiting_permission (pulsing blue)"},
		{protocol.StateWaitingInput, "waiting_input (pulsing red)"},
		{protocol.StateOff, "off"},
	}
	for _, step := range seq {
		fmt.Println(step.label)
		send(protocol.Message{ClaudeID: id, State: step.state})
		time.Sleep(3 * time.Second)
	}
}

func send(msg protocol.Message) {
	if msg.ClaudeID == "" {
		return
	}
	network, target := parseAddr(os.Getenv("LEDSTATUS_ADDR"))
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.Dial(network, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledstatus: dial:", err)
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	if err := json.NewEncoder(conn).Encode(&msg); err != nil {
		fmt.Fprintln(os.Stderr, "ledstatus: write:", err)
	}
}

func parseAddr(s string) (network, target string) {
	if s == "" {
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			return "unix", filepath.Join(xdg, "ledstatus.sock")
		}
		return "unix", "/tmp/ledstatus.sock"
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "unix":
			return "unix", u.Path
		case "tcp", "tcp4", "tcp6":
			return u.Scheme, u.Host
		}
	}
	if strings.HasPrefix(s, "/") {
		return "unix", s
	}
	return "tcp", s
}

// fallbackID builds a best-effort session id when none was provided — useful
// for manual `ledstatus set` calls outside of Claude.
func fallbackID() string {
	u, _ := user.Current()
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	name := "unknown"
	if u != nil {
		name = u.Username
	}
	return fmt.Sprintf("%s@%s:%s", name, host, cwd)
}
