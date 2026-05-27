# CLAUDE.md

Key context for an agent editing this repo. For full details see [ARCHITECTURE.md](./ARCHITECTURE.md) and [DEV.md](./DEV.md).

## What this is

`trd` (Telegram Repo Dispatcher) routes between a Telegram supergroup and multiple [omp](https://github.com/oh-my-pi/oh-my-pi) coding agent instances. Each forum topic is bound to one cloned repo. `/start <git-url>` clones; every subsequent user message spawns `omp -p --resume <session-id>` in that repo and forwards the reply.

**There is no persistent agent.** No tmux per instance, no WebSocket bridge, no MCP channel plugin. Conversation continuity lives in omp's own session storage (`~/.omp/agent/sessions/`); the dispatcher just remembers the session id per instance in bbolt.

## Build / test / run

```bash
make build              # CGO_ENABLED=1 go build → bin/trd
make install            # build + copy to ~/.local/bin/trd
make test               # go test ./...
make lint               # go vet ./...
make restart            # rebuild + restart dispatcher in operator tmux
make install-models     # download whisper + TTS models (~230MB)
```

## Key files

| File | Role |
|------|------|
| `cmd/trd/main.go` | CLI entry, subcommand dispatch, persisted settings |
| `internal/dispatcher/dispatcher.go` | **The hub** — Telegram poll, command handlers, per-instance FIFO run queue |
| `internal/agent/agent.go` | Wraps `omp -p --mode json`, parses NDJSON, exposes classified events |
| `internal/api/api.go` | HTTP control plane for the CLI — `/api/instances`, `/api/allowed/*`, `/api/instances/{id}/cancel`, `/healthz` |
| `internal/media/media.go` | Whisper STT + VITS TTS via sherpa-onnx (CGo), OpenAI API fallback |
| `internal/audio/audio.go` | OGG/Opus decode/encode (replaces ffmpeg) |
| `internal/storage/storage.go` | bbolt: instances (with `SessionID`), allowlist, settings |
| `internal/config/config.go` | Path helpers (`~/.trd/...`) + per-repo `<repo>/.trd/agent.json` (model/thinking) |
| `internal/telegram/telegram.go` | Minimal hand-rolled Telegram Bot API client |

## Package dependency graph

```
cmd/trd → dispatcher → storage, telegram, config, media, agent, api
                       media → audio
```

No cycles. Leaf packages (`config`, `storage`, `audio`, `telegram`, `agent`, `api`) have no inter-internal deps beyond the standard library.

## Dispatcher command handlers

Telegram: `/start`, `/stop`, `/restart`, `/reset`, `/status`, `/watch`, `/cancel`, `/model`, `/effort`, `/debug`, `/forget`, `/help`. Non-commands → `routeToInstance` → `enqueueOrRun` → `driveAgentRun`.

## Agent event types

`internal/agent` emits `Event{Kind: …}` over a channel:

- `EvSessionID` — first line of every run; persists to `Instance.SessionID`.
- `EvAssistantDelta` — streaming text chunk (collected, not yet streamed to Telegram).
- `EvAssistantFinal` — finalized assistant message; canonical reply content.
- `EvError` — API errors (overloaded, rate limit) and parse failures.
- `EvDone` — terminal event; channel closes immediately after.

See `porting/PLAN.md` Appendix A for the source NDJSON shapes.

## Storage buckets

`instances`, `by_topic`, `by_secret` (kept for backward-compat with old rows; `Secret` is unused after the headless port), `allowed_users`, `settings`. Always use Store methods — never read buckets directly.

## Conventions

- **One run per instance at a time.** A second message while a run is in flight queues FIFO until the first finishes; the queued message gets a 👀 reaction.
- **omp is spawned per message.** Don't reintroduce a persistent agent without RFC.
- **Session id is captured from the first NDJSON line.** Pass it as `--resume` on subsequent runs in that instance.
- **`<repo>/.trd/agent.json`** is the source of truth for per-instance model/thinking overrides.
- **`.trd/` is auto-gitignored** in cloned repos.
- **CGo required** for sherpa-onnx (whisper + TTS) and libopus (audio codec).
- **Env vars are persisted** to bbolt settings bucket on first start. Future restarts read from DB.

## Debugging

| Source | Location |
|--------|----------|
| Dispatcher | `tmux attach -t trd` or `~/.trd/trd.log` |
| Per-instance agent stderr | `~/.trd/logs/<instance-id>.log` |
| omp session JSONL | `~/.omp/agent/sessions/<cwd-mangled>/<timestamp>_<session-id>.jsonl` |

Toggle verbose: `/debug` in Telegram or `trd start --debug`.

## Tests

The agent package re-invokes the test binary itself as a fake `omp` (via `TRD_AGENT_TEST_FAKE_OMP_MODE`); see `internal/agent/agent_test.go`. The dispatcher's routing tests use the `runner` test seam (no real omp, no real Telegram) — see `internal/dispatcher/routing_test.go`.
