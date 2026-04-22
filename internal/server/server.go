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
	go func() { defer wg.Done(); s.acceptLoop(ctx, unixLn) }()
	if tcpLn != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.acceptLoop(ctx, tcpLn) }()
	}

	s.runAnimator(ctx)
	wg.Wait()
	return nil
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) {
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
		go s.handleConn(c)
	}
}

func (s *Server) handleConn(c net.Conn) {
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
		s.apply(msg)
	}
}

// apply records a state update and kicks the animator.
func (s *Server) apply(msg protocol.Message) {
	s.mu.Lock()
	switch msg.State {
	case protocol.StateOff, "":
		delete(s.sessions, msg.ClaudeID)
	default:
		s.sessions[msg.ClaudeID] = &session{
			state:    msg.State,
			cwd:      msg.Cwd,
			lastSeen: time.Now(),
		}
	}
	s.mu.Unlock()
	s.log.Debug("apply", "claude", msg.ClaudeID, "state", msg.State)
	select {
	case s.tick <- struct{}{}:
	default:
	}
}

// winning returns the highest-priority live state across all sessions,
// evicting ones that have aged out.
func (s *Server) winning() protocol.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	best := protocol.StateOff
	bestP := 0
	for id, sess := range s.sessions {
		if now.Sub(sess.lastSeen) > s.cfg.TTL {
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
