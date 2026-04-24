package server

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"testing"
	"time"

	"github.com/bmurray/ledstatus/internal/procwatch"
	"github.com/bmurray/ledstatus/internal/protocol"
)

// TestWatchPIDEvictsOnExit spawns a child `sleep`, applies a session for
// that PID, kills the child, and asserts the session disappears from
// winning() within a handful of watcher ticks.
func TestWatchPIDEvictsOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	pid := cmd.Process.Pid

	// Minimal server wired up just enough to drive apply() / winning() /
	// watchPID(); we don't need listeners or the animator.
	s := New(Config{TTL: time.Minute}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.ctx = ctx

	// Sanity: the sleep is alive with a readable starttime.
	start, err := procwatch.StartTime(pid)
	if err != nil {
		t.Fatalf("starttime: %v", err)
	}
	if !procwatch.Alive(pid, start) {
		t.Fatal("sleep should be alive")
	}

	s.apply(protocol.Message{
		ClaudeID:  "test",
		State:     protocol.StateThinking,
		ClaudePID: pid,
	}, true)

	if got := s.winning(); got != protocol.StateThinking {
		t.Fatalf("pre-kill: winning = %q, want thinking", got)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = cmd.Wait()

	// Watcher polls every 2s; give it a generous window.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if s.winning() == protocol.StateOff {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("session not evicted within 6s after process exit")
}

// TestApplyIgnoresPIDOnRemote ensures TCP (isLocal=false) messages never
// trigger the PID watcher path — otherwise the daemon would try to watch
// a PID on a different host.
func TestApplyIgnoresPIDOnRemote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := New(Config{TTL: 50 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.ctx = ctx

	// A PID that is almost certainly not ours; the important thing is that
	// apply(..., false) shouldn't even try to read /proc for it.
	s.apply(protocol.Message{
		ClaudeID:  "remote",
		State:     protocol.StateThinking,
		ClaudePID: 99999999,
	}, false)

	s.mu.Lock()
	sess := s.sessions["remote"]
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("session missing")
	}
	if sess.pid != 0 {
		t.Fatalf("remote session should not carry pid; got %d", sess.pid)
	}

	// TTL should still reap it when it ages out.
	time.Sleep(100 * time.Millisecond)
	if got := s.winning(); got != protocol.StateOff {
		t.Fatalf("expected TTL eviction; winning = %q", got)
	}
}
