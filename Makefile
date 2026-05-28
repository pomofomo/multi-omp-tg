.PHONY: build build-all install install-models install-systemd restart setup start test tidy clean install-deps lint

GO ?= go

# TRD_BIN is the installed binary path. Used directly in tmux send-keys
# so `make setup` / `make restart` / `make start` don't depend on the
# user having ~/.local/bin on $PATH yet.
TRD_BIN ?= $(HOME)/.local/bin/trd

build:
	CGO_ENABLED=1 $(GO) build -o bin/trd ./cmd/trd

install: build
	mkdir -p $(HOME)/.local/bin
	rm -f $(TRD_BIN)
	cp bin/trd $(TRD_BIN)
	@case ":$$PATH:" in \
		*":$(HOME)/.local/bin:"*) ;; \
		*) echo ""; echo "[warn] $(HOME)/.local/bin is not on \$$PATH — add to your shell rc:"; \
			echo "       export PATH=\"\$$HOME/.local/bin:\$$PATH\""; echo "" ;; \
	esac

# Download whisper + TTS models to ~/.trd/models/ (~200MB total).
install-models:
	@echo "Downloading whisper model (base.en, ~165MB)..."
	mkdir -p ~/.trd/models/whisper
	curl -SL https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-whisper-base.en.tar.bz2 | \
		tar xj --strip-components=1 -C ~/.trd/models/whisper
	@echo "Downloading TTS model (lessac-high, ~109MB)..."
	mkdir -p ~/.trd/models/tts
	rm -rf ~/.trd/models/tts/*
	curl -SL https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-en_US-lessac-high.tar.bz2 | \
		tar xj --strip-components=1 -C ~/.trd/models/tts
	@echo "Models installed to ~/.trd/models/"

# Rebuild and bounce the dispatcher.
#
# Preferred path: when the systemd --user unit is active, restart it.
# That triggers the unit's KillMode=mixed -> SIGTERM, trd's
# Shutdown() drain, then Restart=always re-launches the freshly
# installed binary.
#
# Fallback: legacy tmux send-keys for hosts that haven't run
# `make install-systemd` yet. Note this path is racy when the agent
# itself triggers it — see DEBUG.md "The restart-self problem".
restart: install
	@if command -v systemctl >/dev/null 2>&1 && systemctl --user is-active --quiet trd 2>/dev/null; then \
		echo "Restarting trd via systemd --user..."; \
		systemctl --user restart trd; \
	else \
		echo "Restarting trd dispatcher in tmux session 'trd' (legacy path)..."; \
		tmux send-keys -t trd C-c 2>/dev/null || true; \
		sleep 1; \
		tmux send-keys -t trd '$(TRD_BIN) start' Enter; \
	fi

# Install and enable the user systemd unit. After this runs:
#   - trd is supervised by systemd --user (crashes/reboots respawn it)
#   - `make restart` switches over to `systemctl --user restart trd`
#   - Live logs: `journalctl --user -u trd -f`
install-systemd: install
	bash scripts/install-systemd.sh

# First-time setup: builds, installs, and starts trd in an operator tmux
# session. The tmux is purely for keeping the dispatcher process alive
# across an SSH disconnect; agents are spawned per-message, not in tmux.
#
# Usage: make setup TELEGRAM_BOT_TOKEN=123456:ABCDEF...
setup: install
	@if [ -z "$(TELEGRAM_BOT_TOKEN)" ]; then \
		echo "Usage: make setup TELEGRAM_BOT_TOKEN=<your-token>"; \
		echo "Get a token from @BotFather on Telegram."; \
		exit 1; \
	fi
	@command -v tmux >/dev/null || { echo "tmux is required for 'make setup'. Install it or run '$(TRD_BIN) start' under your own process supervisor."; exit 1; }
	@command -v omp  >/dev/null || { echo "omp is required at runtime (the dispatcher spawns 'omp -p' per message). Install with: npm install -g @oh-my-pi/pi-coding-agent"; exit 1; }
	@echo "Creating tmux session 'trd' for the dispatcher..."
	tmux new-session -d -s trd 2>/dev/null || true
	tmux send-keys -t trd "export TELEGRAM_BOT_TOKEN=$(TELEGRAM_BOT_TOKEN)" Enter
	tmux send-keys -t trd '$(TRD_BIN) start' Enter
	@echo ""
	@echo "TRD is running in tmux session 'trd'."
	@echo "  tmux attach -t trd      # see logs"
	@echo "  make restart            # rebuild + restart after code changes"
	@echo ""
	@echo "Your token is saved in the database."
	@echo "Future restarts need no env vars — just: make start"

# Start trd (reads saved config from database — no env vars needed after setup).
start: install
	@command -v tmux >/dev/null || { echo "tmux is required for 'make start'. Install it or run '$(TRD_BIN) start' under your own process supervisor."; exit 1; }
	tmux new-session -d -s trd 2>/dev/null || true
	tmux send-keys -t trd '$(TRD_BIN) start' Enter
	@echo "TRD started in tmux session 'trd'. Attach with: tmux attach -t trd"

# Build native binary only. Cross-compile is disabled — the project
# uses cgo (sherpa-onnx, libopus), so producing portable binaries needs
# per-target toolchains. Use `make build` on each target host instead.
build-all: build
	@echo "Cross-compile not supported (cgo). Built only for the current host."

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

lint:
	$(GO) vet ./...

clean:
	rm -f bin/trd bin/trd-*

install-deps:
	bash scripts/install.sh
