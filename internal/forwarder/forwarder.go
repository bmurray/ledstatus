// Package forwarder runs a local ledstatusd that owns no LED. It accepts
// messages from the ledstatus CLI on a Unix socket and forwards them to a
// remote ledstatusd over TCP (or another Unix path). For each Claude
// session that arrives with a PID, the forwarder watches that process
// locally and pumps a keepalive to the remote once a minute, so the
// remote's TTL never expires a session that's still alive on this host.
// When the process exits, it sends StateOff to the remote and stops.
package forwarder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bmurray/ledstatus/internal/procwatch"
	"github.com/bmurray/ledstatus/internal/protocol"
)

const (
	// keepaliveInterval must stay well under the server's TTL (default 5m)
	// so a single dropped keepalive doesn't evict a live session.
	keepaliveInterval = 60 * time.Second
	pidPollInterval   = 2 * time.Second
	dialTimeout       = 500 * time.Millisecond
	writeTimeout      = 500 * time.Millisecond
)

// Config is the forwarder's runtime configuration.
type Config struct {
	// UnixPath is where the forwarder listens for local CLI messages.
	UnixPath string
	// ForwardTo is the remote ledstatusd address. Forms:
	// "tcp://host:port", "host:port", or "unix:///path".
	ForwardTo string
}

// Forwarder listens, forwards, and keeps remote sessions alive.
type Forwarder struct {
	cfg Config
	log *slog.Logger

	network string
	target  string

	mu       sync.Mutex
	sessions map[string]*fwdSession

	ctx context.Context
}

type fwdSession struct {
	pid      int
	pidStart string
	lastMsg  protocol.Message
	stop     chan struct{}
}

// New validates the forward address and returns a ready Forwarder.
func New(cfg Config, log *slog.Logger) (*Forwarder, error) {
	network, target, err := parseForwardAddr(cfg.ForwardTo)
	if err != nil {
		return nil, err
	}
	return &Forwarder{
		cfg:      cfg,
		log:      log,
		network:  network,
		target:   target,
		sessions: make(map[string]*fwdSession),
	}, nil
}

// Run starts the Unix listener and serves until ctx is done.
func (f *Forwarder) Run(ctx context.Context) error {
	f.ctx = ctx
	_ = os.Remove(f.cfg.UnixPath)
	ln, err := net.Listen("unix", f.cfg.UnixPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", f.cfg.UnixPath, err)
	}
	defer ln.Close()
	defer os.Remove(f.cfg.UnixPath)
	_ = os.Chmod(f.cfg.UnixPath, 0666)
	f.log.Info("forwarder listening",
		"unix", f.cfg.UnixPath,
		"forward_to", f.cfg.ForwardTo,
		"network", f.network, "target", f.target)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			f.log.Debug("accept", "err", err)
			continue
		}
		go f.handleConn(c)
	}
}

func (f *Forwarder) handleConn(c net.Conn) {
	defer c.Close()
	dec := json.NewDecoder(c)
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			if !errors.Is(err, io.EOF) {
				f.log.Debug("decode", "err", err)
			}
			return
		}
		if msg.ClaudeID == "" {
			continue
		}
		f.handleMsg(msg)
	}
}

// handleMsg forwards the incoming message to the remote and, for
// PID-bearing messages, registers or refreshes the local watcher.
func (f *Forwarder) handleMsg(msg protocol.Message) {
	f.send(msg)

	f.mu.Lock()
	defer f.mu.Unlock()

	if msg.State == protocol.StateOff || msg.State == "" {
		if sess, ok := f.sessions[msg.ClaudeID]; ok {
			close(sess.stop)
			delete(f.sessions, msg.ClaudeID)
		}
		return
	}

	sess, ok := f.sessions[msg.ClaudeID]
	if !ok {
		sess = &fwdSession{}
		f.sessions[msg.ClaudeID] = sess
	}
	sess.lastMsg = msg

	if msg.ClaudePID > 0 {
		start, err := procwatch.StartTime(msg.ClaudePID)
		if err != nil {
			f.log.Debug("pid starttime read failed", "pid", msg.ClaudePID, "err", err)
			return
		}
		if sess.pid == msg.ClaudePID && sess.pidStart == start {
			return // watcher already running for this (pid, start)
		}
		if sess.stop != nil {
			close(sess.stop)
		}
		sess.pid = msg.ClaudePID
		sess.pidStart = start
		sess.stop = make(chan struct{})
		go f.watch(msg.ClaudeID, msg.ClaudePID, start, sess.stop)
	}
}

// watch polls the tracked PID, pumps a keepalive every keepaliveInterval,
// and on PID death sends StateOff to the remote and exits.
func (f *Forwarder) watch(claudeID string, pid int, pidStart string, stop <-chan struct{}) {
	poll := time.NewTicker(pidPollInterval)
	defer poll.Stop()
	ka := time.NewTicker(keepaliveInterval)
	defer ka.Stop()

	for {
		select {
		case <-f.ctx.Done():
			return
		case <-stop:
			return
		case <-poll.C:
			if procwatch.Alive(pid, pidStart) {
				continue
			}
			f.mu.Lock()
			if sess, ok := f.sessions[claudeID]; ok && sess.pid == pid && sess.pidStart == pidStart {
				delete(f.sessions, claudeID)
			}
			f.mu.Unlock()
			f.send(protocol.Message{ClaudeID: claudeID, State: protocol.StateOff})
			f.log.Debug("forwarded off: pid exited", "claude", claudeID, "pid", pid)
			return
		case <-ka.C:
			f.mu.Lock()
			sess, ok := f.sessions[claudeID]
			var msg protocol.Message
			if ok && sess.pid == pid && sess.pidStart == pidStart {
				msg = sess.lastMsg
			}
			f.mu.Unlock()
			if msg.ClaudeID == "" {
				continue
			}
			f.send(msg)
			f.log.Debug("keepalive", "claude", claudeID, "state", msg.State)
		}
	}
}

// send dials the remote, writes a single JSON message, and closes. Errors
// are logged, never fatal: a CLI hook must not fail because the remote is
// unreachable.
func (f *Forwarder) send(msg protocol.Message) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(f.ctx, f.network, f.target)
	if err != nil {
		f.log.Debug("forward dial", "err", err)
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := json.NewEncoder(conn).Encode(&msg); err != nil {
		f.log.Debug("forward write", "err", err)
	}
}

func parseForwardAddr(s string) (network, target string, err error) {
	if s == "" {
		return "", "", errors.New("forward-to: empty address")
	}
	if u, perr := url.Parse(s); perr == nil && u.Scheme != "" {
		switch u.Scheme {
		case "unix":
			return "unix", u.Path, nil
		case "tcp", "tcp4", "tcp6":
			return u.Scheme, u.Host, nil
		default:
			return "", "", fmt.Errorf("forward-to: unsupported scheme %q", u.Scheme)
		}
	}
	if strings.HasPrefix(s, "/") {
		return "unix", s, nil
	}
	return "tcp", s, nil
}
