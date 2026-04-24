// Package protocol defines the wire format between the ledstatus CLI and the
// ledstatusd daemon.
//
// Messages are newline-delimited JSON. A single TCP or Unix connection may
// carry one or many messages; the daemon applies each immediately.
package protocol

import "time"

// State describes what one Claude session is doing.
// The daemon folds states across all sessions to pick what to show.
type State string

const (
	StateOff               State = "off"
	StateThinking          State = "thinking"
	StateWaitingPermission State = "waiting_permission"
	StateWaitingInput      State = "waiting_input"
)

// DefaultTTL is how long a session's state remains valid without a refresh.
// Guards against a stuck LED when a terminal hook (SessionEnd) fails to fire.
const DefaultTTL = 5 * time.Minute

// Message is the wire frame: one JSON object per line.
//
// ClaudePID, when non-zero, identifies the Claude process on the sender's
// host. It's authoritative only when the sender is on the same host as the
// daemon (i.e. connected via Unix socket) — the daemon uses it to watch the
// process for liveness and evict the session the moment Claude exits. On TCP
// connections the field is ignored and sessions fall back to TTL reaping.
type Message struct {
	ClaudeID  string `json:"claude_id"`
	State     State  `json:"state"`
	Cwd       string `json:"cwd,omitempty"`
	ClaudePID int    `json:"claude_pid,omitempty"`
}

// Priority orders states so the most urgent signal wins when multiple Claudes
// are active. Higher value wins.
func (s State) Priority() int {
	switch s {
	case StateWaitingPermission:
		return 40
	case StateWaitingInput:
		return 30
	case StateThinking:
		return 20
	default:
		return 0
	}
}
