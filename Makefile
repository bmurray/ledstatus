BIN_DIR := bin
DAEMON  := $(BIN_DIR)/ledstatusd
CLI     := $(BIN_DIR)/ledstatus
PREFIX  := $(HOME)/.local

GO ?= go

.PHONY: all build build-daemon build-cli dev run-daemon \
        install install-udev install-user-systemd \
        fmt vet test clean

all: build

build: build-daemon build-cli

build-daemon:
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(DAEMON) ./cmd/ledstatusd

build-cli:
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(CLI) ./cmd/ledstatus

# `make dev` per CLAUDE.md: build and run the daemon.
dev: build run-daemon

run-daemon: build-daemon
	LEDSTATUS_LOG=debug $(DAEMON)

install: build
	install -D -m 0755 $(DAEMON) $(PREFIX)/bin/ledstatusd
	install -D -m 0755 $(CLI)    $(PREFIX)/bin/ledstatus

# One-time: grants the logged-in user access to /dev/hidrawN for the Luxafor Flag.
install-udev:
	sudo install -D -m 0644 udev/99-luxafor.rules /etc/udev/rules.d/99-luxafor.rules
	sudo udevadm control --reload
	sudo udevadm trigger

install-user-systemd: install
	install -D -m 0644 systemd/ledstatusd.service $(HOME)/.config/systemd/user/ledstatusd.service
	systemctl --user daemon-reload
	systemctl --user enable --now ledstatusd.service

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

clean:
	rm -rf $(BIN_DIR)
