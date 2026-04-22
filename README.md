# ledstatus

Drive a [Luxafor Flag](https://luxafor.com/) USB LED from Claude Code hooks so you can see what Claude is doing from across the room.

- **Solid green** — Claude is thinking / running tools
- **Pulsing blue** — Claude needs permission (or sent a notification)
- **Pulsing red** — Claude finished and is waiting for your next prompt
- **Off** — no active Claude session

Multiple concurrent Claude sessions are tracked separately, and the most urgent state wins (`waiting_permission` > `waiting_input` > `thinking`). A Claude chewing through a long tool loop won't stomp a different Claude that's waiting for you.

Pure Go, no cgo, no external Go dependencies.

---

## How it works

Two small binaries:

- **`ledstatusd`** — long-running daemon. Listens on a Unix socket (and optionally a TCP port for remote clients). Tracks each Claude session's last-reported state with a 5-minute TTL. An animator goroutine owns the `/dev/hidrawN` handle and renders the winning state at ~30fps.
- **`ledstatus`** — tiny CLI. Wired into Claude Code hooks; reads the hook's JSON on stdin, extracts `session_id`, and fires a message to the daemon. Silent on connection failure so a down daemon never breaks a Claude turn.

Wire protocol is newline-delimited JSON:

```json
{"claude_id": "abc-123", "state": "thinking", "cwd": "/home/me/project"}
```

States: `thinking`, `waiting_permission`, `waiting_input`, `off`.

---

## Requirements

- Linux
- Go 1.26+
- A Luxafor Flag (USB VID `04D8`, PID `F372`). Other Luxafor models aren't tested — the HID protocol may or may not match.

---

## Install

```sh
git clone https://github.com/bmurray/ledstatus
cd ledstatus

make install               # -> ~/.local/bin/{ledstatusd,ledstatus}
make install-udev          # sudo: grants your user write access to /dev/hidrawN
make install-user-systemd  # optional: enables ledstatusd.service for your user
```

After `install-udev`, **unplug and replug the Luxafor once** so the new permissions apply.

Verify the daemon can open the device:

```sh
journalctl --user -u ledstatusd -n 10
# look for: level=INFO msg="device opened" path=/dev/hidrawN
```

Smoke test without Claude:

```sh
ledstatus test             # cycles green → blue pulse → red pulse → off, 3s each
```

---

## Wire into Claude Code

Copy the `hooks` block from [`.claude/settings.json`](.claude/settings.json) into your own `~/.claude/settings.json` (merged with any existing keys):

```json
{
  "hooks": {
    "UserPromptSubmit": [{ "hooks": [{ "type": "command", "command": "ledstatus hook thinking" }] }],
    "PreToolUse":       [{ "hooks": [{ "type": "command", "command": "ledstatus hook thinking" }] }],
    "PostToolUse":      [{ "hooks": [{ "type": "command", "command": "ledstatus hook thinking" }] }],
    "Notification":     [{ "hooks": [{ "type": "command", "command": "ledstatus hook waiting_permission" }] }],
    "Stop":             [{ "hooks": [{ "type": "command", "command": "ledstatus hook waiting_input" }] }],
    "SessionEnd":       [{ "hooks": [{ "type": "command", "command": "ledstatus hook off" }] }]
  }
}
```

Or drop the file verbatim at `~/.claude/settings.json` if you have no other hooks.

`ledstatus` must be on the `PATH` of the shell Claude Code launches hooks from. `~/.local/bin` usually is.

---

## Configuration

Optional JSON file at `~/.config/ledstatus/config.json` (or `$XDG_CONFIG_HOME/ledstatus/config.json`, or pass `--config=<path>`). Missing file = built-in defaults. Every field is optional; anything you omit keeps its default.

```json
{
  "brightness": 1.0,
  "states": {
    "thinking":           { "color": "#005500", "effect": "solid" },
    "waiting_permission": { "color": "#0000ff", "effect": "pulse",
                            "period": 1.6,
                            "min_brightness": 0.15, "max_brightness": 1.0 },
    "waiting_input":      { "color": "#ff0000", "effect": "pulse",
                            "period": 2.0,
                            "min_brightness": 0.15, "max_brightness": 1.0 }
  }
}
```

- `brightness` — global 0..1 scalar applied to the final rendered color.
- `color` — `#rrggbb` hex.
- `effect` — `"solid"` or `"pulse"`.
- `period` — pulse cycle length in seconds.
- `min_brightness` / `max_brightness` — pulse envelope bounds (0..1); ignored for solid.

### Reloading without a restart

```sh
systemctl --user kill -s HUP ledstatusd
```

The daemon re-reads the config file on `SIGHUP` and swaps it in atomically.

---

## Daemon flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--socket <path>` | `$XDG_RUNTIME_DIR/ledstatus.sock` | Unix socket path |
| `--tcp-addr <host:port>` | *(disabled)* | Optional **unauthenticated** TCP listener. e.g. `:9876` |
| `--ttl <duration>` | `5m` | Evicts a session's state if no update arrives within this window |
| `--config <path>` | `$XDG_CONFIG_HOME/ledstatus/config.json` | JSON config file path |

Environment:

- `LEDSTATUS_LOG=debug` (daemon) — enables debug logs.
- `LEDSTATUS_ADDR` (CLI) — where clients dial. Forms: `/path/to.sock`, `unix:///path`, `tcp://host:port`, or bare `host:port`. Default matches the daemon's `--socket`.

---

## Remote / multi-machine

Point several Claude sessions on different hosts at one Luxafor.

On the host with the Luxafor, start the daemon with a TCP listener:

```sh
ledstatusd --tcp-addr :9876
```

Or edit `systemd/ledstatusd.service`'s `ExecStart=` line and restart:

```
ExecStart=%h/.local/bin/ledstatusd --tcp-addr :9876
```

```sh
systemctl --user daemon-reload
systemctl --user restart ledstatusd
```

On each client machine, set the CLI address before starting Claude:

```sh
export LEDSTATUS_ADDR=tcp://<host>:9876
```

> The TCP listener has **no authentication**. Only bind it on trusted networks.

---

## Manual use

```sh
ledstatus set thinking                 # uses a fallback session id
ledstatus set waiting_permission --claude-id=my-session
ledstatus off --claude-id=my-session
```

Useful for scripting other status sources: long builds, CI watches, etc.

---

## Development

```sh
make dev          # build + run daemon in foreground with debug logging
make build        # just build into ./bin/
make vet
make test
```

Stop the systemd service before running `make dev`, or they'll race for the same socket:

```sh
systemctl --user stop ledstatusd
```

Project layout:

```
cmd/ledstatusd/        daemon entrypoint
cmd/ledstatus/         CLI entrypoint
internal/luxafor/      hidraw discovery + HID writes
internal/protocol/     wire types + state priority
internal/server/       listeners, session tracker, animator
internal/config/       JSON config loader
udev/                  udev rule (installed by `make install-udev`)
systemd/               user unit (installed by `make install-user-systemd`)
```

---

## License

MIT — see [LICENSE](LICENSE).
