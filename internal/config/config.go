// Package config defines the ledstatusd runtime configuration: per-state
// colors, effects, and a global brightness scalar. Values are loaded from a
// JSON file and merged over built-in defaults, so users only need to specify
// the fields they want to override.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bmurray/ledstatus/internal/protocol"
)

// Effect selects how a state's color is rendered over time.
type Effect string

const (
	EffectSolid Effect = "solid"
	EffectPulse Effect = "pulse"
)

// StateConfig is the per-state rendering config.
//
// Zero values are treated as "use the default for this field" so a user can
// override just one thing (e.g. color) without having to re-specify period
// and brightness bounds.
type StateConfig struct {
	// Color is the base RGB as "#rrggbb".
	Color string `json:"color"`
	// Effect is "solid" or "pulse". Default "solid".
	Effect Effect `json:"effect,omitempty"`
	// Period is the pulse cycle length in seconds. Default 2.0.
	Period float64 `json:"period,omitempty"`
	// MinBrightness / MaxBrightness bound the pulse envelope in 0..1.
	// Default 0.15..1.0. Ignored for solid effect.
	MinBrightness float64 `json:"min_brightness,omitempty"`
	MaxBrightness float64 `json:"max_brightness,omitempty"`

	// Derived at load time; not serialized.
	R, G, B uint8 `json:"-"`
}

// Config is the whole file.
type Config struct {
	// Brightness scales the final rendered color. 0..1. Default 1.0.
	Brightness float64 `json:"brightness,omitempty"`
	// States maps state name -> rendering config. Unknown states are ignored.
	States map[protocol.State]*StateConfig `json:"states,omitempty"`
}

// Default returns the built-in defaults — what the daemon uses when no
// config file exists. Colors are pre-resolved so the returned Config is
// immediately usable by the animator.
func Default() *Config {
	c := &Config{
		Brightness: 1.0,
		States: map[protocol.State]*StateConfig{
			protocol.StateThinking: {
				// ~1/3 brightness — full green is glaringly bright on the
				// Flag and "thinking" is the most persistent state.
				Color:  "#005500",
				Effect: EffectSolid,
			},
			protocol.StateWaitingPermission: {
				Color:         "#0000ff",
				Effect:        EffectPulse,
				Period:        1.6,
				MinBrightness: 0.15,
				MaxBrightness: 1.0,
			},
			protocol.StateWaitingInput: {
				Color:         "#ff0000",
				Effect:        EffectPulse,
				Period:        2.0,
				MinBrightness: 0.15,
				MaxBrightness: 1.0,
			},
		},
	}
	_ = c.resolveColors() // defaults are valid by construction
	return c
}

// LoadFile returns a Config built from defaults merged with any overrides
// found in the JSON file at path.
//
// If path is empty or the file doesn't exist, Default() is returned.
func LoadFile(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		if err := cfg.resolveColors(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := cfg.resolveColors(); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return nil, err
	}
	var user Config
	if err := json.Unmarshal(data, &user); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if user.Brightness > 0 {
		cfg.Brightness = user.Brightness
	}
	for state, sc := range user.States {
		if sc == nil {
			continue
		}
		existing, known := cfg.States[state]
		if !known {
			cfg.States[state] = sc
			continue
		}
		merged := *existing
		if sc.Color != "" {
			merged.Color = sc.Color
		}
		if sc.Effect != "" {
			merged.Effect = sc.Effect
		}
		if sc.Period > 0 {
			merged.Period = sc.Period
		}
		if sc.MinBrightness > 0 {
			merged.MinBrightness = sc.MinBrightness
		}
		if sc.MaxBrightness > 0 {
			merged.MaxBrightness = sc.MaxBrightness
		}
		cfg.States[state] = &merged
	}

	if err := cfg.resolveColors(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolveColors parses each state's hex color into RGB bytes.
func (c *Config) resolveColors() error {
	for state, sc := range c.States {
		r, g, b, err := parseHex(sc.Color)
		if err != nil {
			return fmt.Errorf("state %q: color %w", state, err)
		}
		sc.R, sc.G, sc.B = r, g, b
	}
	return nil
}

func parseHex(s string) (r, g, b uint8, err error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return 0, 0, 0, fmt.Errorf("want #rrggbb, got %q", s)
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid hex %q: %w", s, err)
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), nil
}

// State returns config for s, or nil if none defined (render as off).
func (c *Config) State(s protocol.State) *StateConfig {
	if c == nil {
		return nil
	}
	return c.States[s]
}
