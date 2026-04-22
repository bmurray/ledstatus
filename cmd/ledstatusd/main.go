// ledstatusd is the LED status daemon. It listens for state updates from one
// or more Claude sessions and drives a Luxafor Flag to show the highest-
// priority live state.
//
// SIGHUP re-reads the config file, so colors / brightness can be tweaked
// without a restart.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bmurray/ledstatus/internal/config"
	"github.com/bmurray/ledstatus/internal/server"
)

func main() {
	var cfg server.Config
	flag.StringVar(&cfg.UnixPath, "socket", defaultSocketPath(), "unix socket path")
	flag.StringVar(&cfg.TCPAddr, "tcp-addr", "",
		"optional TCP listen address, e.g. :9876 or 0.0.0.0:9876. UNAUTHENTICATED.")
	flag.DurationVar(&cfg.TTL, "ttl", 5*time.Minute,
		"evict a session's state if we haven't heard from it for this long")
	configPath := flag.String("config", defaultConfigPath(),
		"path to JSON config file; missing file is OK (defaults are used)")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("LEDSTATUS_LOG") == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	animCfg, err := config.LoadFile(*configPath)
	if err != nil {
		log.Error("config load failed", "path", *configPath, "err", err)
		os.Exit(1)
	}
	log.Info("config loaded", "path", *configPath, "brightness", animCfg.Brightness)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := server.New(cfg, log)
	s.SetAnimConfig(animCfg)

	// SIGHUP → reload config file in place.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				newCfg, err := config.LoadFile(*configPath)
				if err != nil {
					log.Error("config reload failed", "err", err)
					continue
				}
				s.SetAnimConfig(newCfg)
				log.Info("config reloaded", "path", *configPath, "brightness", newCfg.Brightness)
			}
		}
	}()

	if err := s.Run(ctx); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func defaultSocketPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "ledstatus.sock")
	}
	return "/tmp/ledstatus.sock"
}

// defaultConfigPath follows XDG: $XDG_CONFIG_HOME/ledstatus/config.json,
// else $HOME/.config/ledstatus/config.json.
func defaultConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "ledstatus", "config.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "ledstatus", "config.json")
	}
	return ""
}
