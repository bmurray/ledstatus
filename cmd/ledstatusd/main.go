// ledstatusd is the LED status daemon. It listens for state updates from one
// or more Claude sessions and drives a Luxafor Flag to show the highest-
// priority live state.
//
// With --forward-to it runs in forwarder mode instead: accepts local
// messages and relays them to a remote ledstatusd (with PID-tracked
// keepalives) rather than driving any hardware.
//
// SIGHUP re-reads the config file so colors / brightness can be tweaked
// without a restart. (No-op in forwarder mode.)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bmurray/ledstatus/internal/config"
	"github.com/bmurray/ledstatus/internal/forwarder"
	"github.com/bmurray/ledstatus/internal/server"
)

func main() {
	var (
		srvCfg    server.Config
		forwardTo string
	)
	flag.StringVar(&srvCfg.UnixPath, "socket", defaultSocketPath(), "unix socket path")
	flag.StringVar(&srvCfg.TCPAddr, "tcp-addr", "",
		"optional TCP listen address, e.g. :9876 or 0.0.0.0:9876. UNAUTHENTICATED.")
	flag.DurationVar(&srvCfg.TTL, "ttl", 5*time.Minute,
		"evict a non-PID-tracked session if we haven't heard from it for this long")
	flag.StringVar(&forwardTo, "forward-to", "",
		"run as a forwarder: relay local messages to this remote ledstatusd address "+
			"(tcp://host:port, host:port, or unix:///path). Mutually exclusive with --tcp-addr.")
	configPath := flag.String("config", defaultConfigPath(),
		"path to JSON config file; missing file is OK (defaults are used). Ignored in forwarder mode.")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("LEDSTATUS_LOG") == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if forwardTo != "" {
		if srvCfg.TCPAddr != "" {
			fmt.Fprintln(os.Stderr, "ledstatusd: --forward-to and --tcp-addr are mutually exclusive")
			os.Exit(2)
		}
		runForwarder(ctx, srvCfg.UnixPath, forwardTo, log)
		return
	}

	runServer(ctx, srvCfg, *configPath, log)
}

func runServer(ctx context.Context, cfg server.Config, configPath string, log *slog.Logger) {
	animCfg, err := config.LoadFile(configPath)
	if err != nil {
		log.Error("config load failed", "path", configPath, "err", err)
		os.Exit(1)
	}
	log.Info("config loaded", "path", configPath, "brightness", animCfg.Brightness)

	s := server.New(cfg, log)
	s.SetAnimConfig(animCfg)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				newCfg, err := config.LoadFile(configPath)
				if err != nil {
					log.Error("config reload failed", "err", err)
					continue
				}
				s.SetAnimConfig(newCfg)
				log.Info("config reloaded", "path", configPath, "brightness", newCfg.Brightness)
			}
		}
	}()

	if err := s.Run(ctx); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func runForwarder(ctx context.Context, unixPath, forwardTo string, log *slog.Logger) {
	f, err := forwarder.New(forwarder.Config{
		UnixPath:  unixPath,
		ForwardTo: forwardTo,
	}, log)
	if err != nil {
		log.Error("forwarder init failed", "err", err)
		os.Exit(1)
	}
	if err := f.Run(ctx); err != nil {
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
