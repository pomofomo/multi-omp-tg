# Developer Guide

How to contribute to TRD, build from source, and debug issues.

## Build and test

```bash
make build              # CGO_ENABLED=1 go build → bin/trd
make install            # build + copy to ~/.local/bin/trd
make test               # go test ./...
make lint               # go vet ./...
make install-models     # download whisper + TTS models (~230MB)
make restart            # rebuild + restart dispatcher (systemd --user or tmux)
make start              # start dispatcher (reads saved config from DB)
make install-systemd    # one-time: install ~/.config/systemd/user/trd.service (recommended for headless hosts)
```

First-time setup:

```bash
make setup TELEGRAM_BOT_TOKEN=123456:ABCDEF...
```

This builds, installs the binary, starts the dispatcher in an operator tmux session named `trd`, and saves the token to bbolt. The operator tmux exists only so the dispatcher process survives an SSH disconnect — agents are not spawned in tmux, they are one-shot `omp -p` subprocesses.

### Build dependencies

| Package | Why | Install |
|---------|-----|---------|
| Go 1.22+ | compiles the dispatcher | [go.dev/dl](https://go.dev/dl) |
| `libopus-dev` + `libopusfile-dev` | Opus codec for voice (CGo) | `apt install libopus-dev libopusfile-dev` |
| `omp` | the headless coding agent | `npm install -g @oh-my-pi/pi-coding-agent` |

CGo is required (`CGO_ENABLED=1`) because of sherpa-onnx (whisper + TTS) and libopus.

## Code layout

```
cmd/trd/main.go                  CLI entry point, subcommand dispatch
internal/
  dispatcher/dispatcher.go       The hub — Telegram poll, command handlers, per-instance FIFO run queue
  agent/agent.go                 omp -p wrapper: NDJSON parser, classified events, cancellation
  api/api.go                     HTTP control plane for the CLI
  storage/storage.go             bbolt wrapper (instances, allowlist, settings)
  media/media.go                 Whisper STT + VITS TTS (sherpa-onnx embedded)
  audio/audio.go                 OGG/Opus decode/encode (replaces ffmpeg)
  telegram/telegram.go           Minimal Telegram Bot API client
  config/                        Paths, per-repo agent.json, gitignore
```

### Package dependency graph

```
cmd/trd → dispatcher → storage, telegram, config, media, agent, api
                       media → audio
```

No cycles. Don't introduce them. Leaf packages (`config`, `storage`, `audio`, `telegram`, `agent`, `api`) have no inter-internal deps.

## Contributing

1. Fork this repo and clone it.
2. Set up TRD pointing at your fork — you can develop it through Telegram using TRD itself.
3. Make changes, test with `make test`, submit a PR.

### Self-development workflow

TRD can manage its own repo:

1. Create a topic in your Telegram group.
2. Send `/start git@github.com:you/multi-omp-tg.git` to clone your fork.
3. Talk to the agent in that topic. It edits your fork directly (the repo is cloned to `~/.trd/repos/<instance-id>`).
4. When ready, push and `make restart` to rebuild the dispatcher with your changes.

There is no longer a "self-modify" target — the channel plugin is gone, so no live indirection is needed.

## Conventions

- **Telegram client** is a hand-rolled `net/http` wrapper. Keep it minimal — only methods TRD actually calls.
- **One run per instance at a time.** Concurrent messages queue FIFO. Don't reintroduce a persistent agent without an RFC.
- **omp wire format** is documented in `porting/PLAN.md` Appendix A. Any new event types go through `agent.classify`.
- **`<repo>/.trd/agent.json`** is the source of truth for per-instance model/thinking overrides.
- **`.trd/` is auto-gitignored** in cloned repos. Don't remove.
- **Env vars are persisted** to bbolt settings bucket on first start. See `persistedEnvKeys` in `cmd/trd/main.go`.

## Adding a new Telegram command

1. Add the case in `handleMessage`'s switch block in `dispatcher.go`.
2. Write the `cmd<Name>` handler method.
3. Add the command to `SetMyCommands` in `pollLoop` for Telegram autocomplete.
4. Update the README Telegram commands table and the `/help` text in `cmdHelp`.

## Adding a new agent event type

1. Add the `Ev*` constant in `internal/agent/agent.go`.
2. Map the source NDJSON shape in `agent.classify`.
3. Handle the event in `dispatcher.driveAgentRun`'s switch block.
4. Add a unit test in `agent_test.go` (the test binary doubles as a fake omp via `TRD_AGENT_TEST_FAKE_OMP_MODE`).

## Debugging

Three log sources to check:

### 1. TRD dispatcher logs

```bash
tmux attach -t trd              # live logs (Ctrl+B, D to detach)
tail -f ~/.trd/trd.log          # from another terminal
trd start --debug               # verbose mode (or TRD_DEBUG=1)
```

Toggle debug at runtime: send `/debug` in any Telegram topic.

### 2. Per-instance agent stderr

```bash
trd watch <instance-name>       # via the CLI
# or
tail -f ~/.trd/logs/<instance-id>.log
```

`/watch` in a Telegram topic also tails this file.

### 3. omp session JSONL

```bash
ls -lt ~/.omp/agent/sessions/    # most recently used cwd-mangled dirs first
```

Each session file is the full conversation. `omp -p --resume <id>` replays it.

### Quick debug checklist

| Symptom | Check |
|---------|-------|
| Message not arriving | `trd.log`: look for "tg recv" → "tg->agent forward". |
| Agent spawn failing | `trd.log`: "agent.Start failed". Under tmux: verify `omp` is on `$PATH` or set `TRD_OMP_BIN`. Under systemd: re-run `make install-systemd` so the drop-in at `~/.config/systemd/user/trd.service.d/env.conf` re-pins PATH and `TRD_OMP_BIN` (especially after switching node/nvm versions). |
| No reply | `trd watch <name>`: check omp stderr for crashes / API errors. |
| Session not resuming | `/reset` to clear the session id; next message starts fresh. omp can't resume a `.tmp` session file (left behind by a killed run). |
| TTS/Whisper broken | `trd.log`: search for "whisper:" entries. Verify models in `~/.trd/models/`. |

## Environment variables

| Variable | Purpose | Persisted |
|----------|---------|-----------|
| `TELEGRAM_BOT_TOKEN` | Bot authentication | Yes |
| `TRD_PORT` | Dispatcher HTTP API port (default 7777) | No |
| `TRD_OMP_BIN` | omp binary path (default `omp` on PATH). Under systemd, auto-pinned by `make install-systemd` to the absolute path of `omp` at install time. | Yes |
| `TRD_WHISPER_MODEL_DIR` | Whisper model directory | Yes |
| `TRD_TTS_MODEL_DIR` | TTS model directory | Yes |
| `TRD_OPENAI_API_KEY` | OpenAI API fallback for STT/TTS | Yes |
| `TRD_ALLOWED_USERNAMES` | Comma-separated allowlist | Yes |
| `TRD_DEBUG` | Set to "1" for debug mode | No |

"Persisted" means saved to bbolt on first start and restored on future starts when the env var isn't set.

## Security model

- **Authorization:** Supergroup membership = authorized. User allowlist (`trd allow/deny`) adds fine-grained control.
- **Networking:** Dispatcher HTTP API on `127.0.0.1` only. No external exposure.
- **Private repos:** SSH agent / `~/.ssh/` config.
- **Agent capabilities:** `omp` runs with whatever filesystem/network permissions the dispatcher itself has. Run the dispatcher under a dedicated user if you're worried.
