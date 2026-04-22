package server

import (
	"context"
	"math"
	"time"

	"github.com/bmurray/ledstatus/internal/config"
	"github.com/bmurray/ledstatus/internal/luxafor"
	"github.com/bmurray/ledstatus/internal/protocol"
)

// ~30 fps. Pulsing brightness is interpolated in software, so we want a smooth
// update rate but not so high we hammer the USB bus.
const frameInterval = 33 * time.Millisecond

type color struct{ r, g, b uint8 }

var colorOff = color{0, 0, 0}

// runAnimator is the only goroutine that touches s.device. It renders the
// winning state at frameInterval, writing only on change, and transparently
// reconnects if the device is unplugged or a write fails.
func (s *Server) runAnimator(ctx context.Context) {
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	start := time.Now()
	var last color
	haveLast := false

	// Back off before retrying a failed Open, so we don't thrash /sys.
	var nextOpenAttempt time.Time

	defer func() {
		if s.device != nil {
			_ = s.device.SetColor(0, 0, 0)
			_ = s.device.Close()
			s.device = nil
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.tick:
			// State or config just changed — drop the last-frame memo so we
			// write the new rendering immediately rather than wait for drift.
			haveLast = false
		}

		if s.device == nil {
			if time.Now().Before(nextOpenAttempt) {
				continue
			}
			if err := s.tryOpen(); err != nil {
				s.log.Debug("device open failed", "err", err)
				nextOpenAttempt = time.Now().Add(3 * time.Second)
				continue
			}
		}

		t := time.Since(start).Seconds()
		frame := render(s.winning(), t, s.animCfg.Load())

		if haveLast && frame == last {
			continue
		}
		if err := s.device.SetColor(frame.r, frame.g, frame.b); err != nil {
			s.log.Warn("device write failed; will reconnect", "err", err)
			_ = s.device.Close()
			s.device = nil
			nextOpenAttempt = time.Now().Add(1 * time.Second)
			haveLast = false
			continue
		}
		last = frame
		haveLast = true
	}
}

func (s *Server) tryOpen() error {
	path, err := luxafor.Discover()
	if err != nil {
		return err
	}
	if path == "" {
		return errNotFound
	}
	dev, err := luxafor.Open(path)
	if err != nil {
		return err
	}
	s.device = dev
	s.log.Info("device opened", "path", path)
	return nil
}

var errNotFound = &openError{"luxafor device not found"}

type openError struct{ s string }

func (e *openError) Error() string { return e.s }

// render resolves a state to the RGB frame to write, using cfg for colors,
// effects, and the global brightness scalar.
func render(state protocol.State, t float64, cfg *config.Config) color {
	sc := cfg.State(state)
	if sc == nil {
		return colorOff
	}
	var b float64
	switch sc.Effect {
	case config.EffectPulse:
		period := sc.Period
		if period <= 0 {
			period = 2.0
		}
		min := sc.MinBrightness
		if min <= 0 {
			min = 0.15
		}
		max := sc.MaxBrightness
		if max <= 0 {
			max = 1.0
		}
		phase := 2 * math.Pi * t / period
		// Scale sine's [-1,+1] into [min,max].
		b = min + (max-min)*0.5*(1+math.Sin(phase))
	default: // solid
		b = 1.0
	}
	b *= cfg.Brightness
	if b < 0 {
		b = 0
	} else if b > 1 {
		b = 1
	}
	return color{
		r: uint8(float64(sc.R) * b),
		g: uint8(float64(sc.G) * b),
		b: uint8(float64(sc.B) * b),
	}
}
