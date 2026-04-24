// Package server is the ledstatusd core: socket listeners, per-session state
// tracking, and the animator that drives the device.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bmurray/ledstatus/internal/config"
	"github.com/bmurray/ledstatus/internal/luxafor"
	"github.com/bmurray/ledstatus/internal/procwatch"
	"github.com/bmurray/ledstatus/internal/protocol"
)

// Config controls how the daemon listens and ages sessions out.
type Config struct {
	UnixPath string        // e.g. /run/user/1000/ledstatus.sock
	TCPAddr  string        // e.g. ":9876"; empty to disable TCP. UNAUTHENTICATED.
	TTL      time.Duration // session state expiry
}

type session struct {
	state    protocol.State
	cwd      string
	lastSeen time.Time

	// pid / pidStart identify a local Claude process to watch for liveness.
	// When pid > 0 the session is evicted by watchPID on process exit, not
	// by the TTL check in winning(). Zero means "no local PID" — fall back
	// to TTL reaping (remote/TCP clients, manual CLI use, etc.).
	pid      int
	pidStart string
	// watchStop is closed to signal the current watcher to exit. Replaced
	// whenever we start tracking a new (pid, pidStart) for this session.
	watchStop chan struct{}
}

// Server owns the listeners, the session map, and the animator's device handle.
type Server struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session

	// animCfg is the current animation config. Swapped atomically so SIGHUP
	// reloads don't need to coordinate with the animator goroutine.
	animCfg atomic.Pointer[config.Config]

	// tick wakes the animator on state changes so transitions are instant
	// rather than waiting for the next frame.
	tick chan struct{}

	// device is owned by the animator goroutine only.
	device *luxafor.Device

	// ctx is the server's lifecycle context. Set in Run(); read by
	// PID-watcher goroutines so they exit on shutdown.
	ctx context.Context
}

func New(cfg Config, log *slog.Logger) *Server {
	if cfg.TTL <= 0 {
		cfg.TTL = protocol.DefaultTTL
	}
	s := &Server{
		cfg:      cfg,
		log:      log,
		sessions: make(map[string]*session),
		tick:     make(chan struct{}, 1),
	}
	s.animCfg.Store(config.Default())
	return s
}

// SetAnimConfig swaps the rendering config atomically and wakes the animator
// so the new values take effect on the next frame.
func (s *Server) SetAnimConfig(c *config.Config) {
	if c == nil {
		return
	}
	s.animCfg.Store(c)
	select {
	case s.tick <- struct{}{}:
	default:
	}
}

// Run starts listeners and the animator. Returns when ctx is done.
func (s *Server) Run(ctx context.Context) error {
	s.ctx = ctx
	_ = os.Remove(s.cfg.UnixPath) // clear stale socket from previous run

	unixLn, err := net.Listen("unix", s.cfg.UnixPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", s.cfg.UnixPath, err)
	}
	defer unixLn.Close()
	defer os.Remove(s.cfg.UnixPath)
	// Allow any local user to write — the socket lives in a user-owned
	// runtime dir, and the daemon itself holds no secrets.
	_ = os.Chmod(s.cfg.UnixPath, 0666)
	s.log.Info("listening", "unix", s.cfg.UnixPath)

	var tcpLn net.Listener
	if s.cfg.TCPAddr != "" {
		tcpLn, err = net.Listen("tcp", s.cfg.TCPAddr)
		if err != nil {
			return fmt.Errorf("listen tcp %s: %w", s.cfg.TCPAddr, err)
		}
		defer tcpLn.Close()
		s.log.Warn("listening on TCP without authentication", "addr", s.cfg.TCPAddr)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.acceptLoop(ctx, unixLn, true) }()
	if tcpLn != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.acceptLoop(ctx, tcpLn, false) }()
	}

	s.runAnimator(ctx)
	wg.Wait()
	return nil
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, isLocal bool) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Debug("accept", "err", err)
			continue
		}
		go s.handleConn(c, isLocal)
	}
}

func (s *Server) handleConn(c net.Conn, isLocal bool) {
	defer c.Close()
	dec := json.NewDecoder(c)
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("decode", "err", err, "remote", c.RemoteAddr())
			}
			return
		}
		if msg.ClaudeID == "" {
			s.log.Debug("msg missing claude_id")
			continue
		}
		s.apply(msg, isLocal)
	}
}

// apply records a state update and kicks the animator.
//
// When isLocal is true and the message carries a ClaudePID, we start (or
// refresh) a watchPID goroutine keyed on (pid, starttime). The session then
// lives until the process exits, ignoring the TTL.
func (s *Server) apply(msg protocol.Message, isLocal bool) {
	s.mu.Lock()
	switch msg.State {
	case protocol.StateOff, "":
		if sess, ok := s.sessions[msg.ClaudeID]; ok {
			if sess.watchStop != nil {
				close(sess.watchStop)
			}
			delete(s.sessions, msg.ClaudeID)
		}
	default:
		sess, ok := s.sessions[msg.ClaudeID]
		if !ok {
			sess = &session{}
			s.sessions[msg.ClaudeID] = sess
		}
		sess.state = msg.State
		sess.cwd = msg.Cwd
		sess.lastSeen = time.Now()

		if isLocal && msg.ClaudePID > 0 {
			start, err := procwatch.StartTime(msg.ClaudePID)
			if err != nil {
				s.log.Debug("pid starttime read failed", "pid", msg.ClaudePID, "err", err)
			} else if sess.pid != msg.ClaudePID || sess.pidStart != start {
				if sess.watchStop != nil {
					close(sess.watchStop)
				}
				sess.pid = msg.ClaudePID
				sess.pidStart = start
				stop := make(chan struct{})
				sess.watchStop = stop
				go s.watchPID(msg.ClaudeID, msg.ClaudePID, start, stop)
			}
		}
	}
	s.mu.Unlock()
	s.log.Debug("apply", "claude", msg.ClaudeID, "state", msg.State,
		"pid", msg.ClaudePID, "local", isLocal)
	select {
	case s.tick <- struct{}{}:
	default:
	}
}

// winning returns the highest-priority live state across all sessions.
// Sessions with pid == 0 are evicted on TTL expiry; sessions with pid > 0
// are kept as long as watchPID hasn't removed them yet.
func (s *Server) winning() protocol.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	best := protocol.StateOff
	bestP := 0
	for id, sess := range s.sessions {
		if sess.pid == 0 && now.Sub(sess.lastSeen) > s.cfg.TTL {
			delete(s.sessions, id)
			continue
		}
		if p := sess.state.Priority(); p > bestP {
			bestP = p
			best = sess.state
		}
	}
	return best
}

// watchPID polls /proc/<pid> every 2s and evicts the session when the
// process exits or its start-time changes (indicating PID reuse). Exits on
// server shutdown or when a newer watcher supersedes this one via stop.
func (s *Server) watchPID(claudeID string, pid int, pidStart string, stop <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			if procwatch.Alive(pid, pidStart) {
				continue
			}
			s.mu.Lock()
			if sess, ok := s.sessions[claudeID]; ok && sess.pid == pid && sess.pidStart == pidStart {
				delete(s.sessions, claudeID)
				s.log.Debug("session evicted: pid exited", "claude", claudeID, "pid", pid)
			}
			s.mu.Unlock()
			select {
			case s.tick <- struct{}{}:
			default:
			}
			return
		}
	}
}
