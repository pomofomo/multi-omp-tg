.PHONY: build build-all install install-models restart setup start test tidy clean install-deps lint

GO ?= go

build:
	CGO_ENABLED=1 $(GO) build -o bin/trd ./cmd/trd

install: build
	mkdir -p ~/.local/bin
	rm -f ~/.local/bin/trd
	cp bin/trd ~/.local/bin/trd

# Download whisper + TTS models to ~/.trd/models/ (~200MB total).
install-models:
	@echo "Downloading whisper model (base.en, ~165MB)..."
	mkdir -p ~/.trd/models/whisper
	curl -SL https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-whisper-base.en.tar.bz2 | \
		tar xj --strip-components=1 -C ~/.trd/models/whisper
	@echo "Downloading TTS model (lessac-medium, ~64MB)..."
	mkdir -p ~/.trd/models/tts
	curl -SL https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-en_US-lessac-medium.tar.bz2 | \
		tar xj --strip-components=1 -C ~/.trd/models/tts
	@echo "Models installed to ~/.trd/models/"

# Rebuild and bounce the dispatcher running in operator tmux session 'trd'.
# Operator tmux is unrelated to per-instance agent management (which is now
# one-shot omp subprocesses with no persistent state).
restart: install
	@echo "Restarting trd dispatcher in tmux session 'trd'..."
	tmux send-keys -t trd C-c 2>/dev/null || true
	sleep 1
	tmux send-keys -t trd 'trd start' Enter

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
	@echo "Creating tmux session 'trd' for the dispatcher..."
	tmux new-session -d -s trd 2>/dev/null || true
	tmux send-keys -t trd "export TELEGRAM_BOT_TOKEN=$(TELEGRAM_BOT_TOKEN)" Enter
	tmux send-keys -t trd 'trd start' Enter
	@echo ""
	@echo "TRD is running in tmux session 'trd'."
	@echo "  tmux attach -t trd      # see logs"
	@echo "  make restart            # rebuild + restart after code changes"
	@echo ""
	@echo "Your token is saved in the database."
	@echo "Future restarts need no env vars — just: make start"

# Start trd (reads saved config from database — no env vars needed after setup).
start: install
	tmux new-session -d -s trd 2>/dev/null || true
	tmux send-keys -t trd 'trd start' Enter
	@echo "TRD started in tmux session 'trd'. Attach with: tmux attach -t trd"

build-all:
	bash scripts/build-binaries.sh

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
