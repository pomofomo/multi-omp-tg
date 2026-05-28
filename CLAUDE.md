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
make restart            # systemctl --user restart trd (when unit active), else tmux fallback
make install-systemd    # one-time: install ~/.config/systemd/user/trd.service & enable it
make install-models     # download whisper + TTS models (~230MB)
```

## Key files

| File | Role |
|------|------|
| `cmd/trd/main.go` | CLI entry, subcommand dispatch, persisted settings |
| `internal/dispatcher/dispatcher.go` | **The hub** — Telegram poll, command handlers, per-instance FIFO run queue |
| `internal/agent/agent.go` | Wraps `omp -p --mode json`, parses NDJSON, exposes classified events |
| `internal/api/api.go` | HTTP control plane — `/api/instances`, `/api/allowed/*`, `/api/instances/{id}/cancel`, `/api/instances/{id}/promote`, `/api/tg/react`, `/api/restart`, `/healthz` |
| `internal/agent/extension/` | Embedded omp TS extension (`tg_react`, `trd_restart` tools) + system-prompt snippet, written to `~/.trd/ext/tg.ts` on startup |
| `internal/media/media.go` | Whisper STT + VITS TTS via sherpa-onnx (CGo), OpenAI API fallback |
| `internal/audio/audio.go` | OGG/Opus decode/encode (replaces ffmpeg) |
| `internal/storage/storage.go` | bbolt: instances (with `SessionID`, `Controller`), allowlist, settings, `deferred_prompts` |
| `internal/config/config.go` | Path helpers (`~/.trd/...`) + per-repo `<repo>/.trd/agent.json` (model/thinking) |
| `internal/telegram/telegram.go` | Minimal hand-rolled Telegram Bot API client |

## Package dependency graph

```
cmd/trd → dispatcher → storage, telegram, config, media, agent, api
                       media → audio
```

No cycles. Leaf packages (`config`, `storage`, `audio`, `telegram`, `agent`, `api`) have no inter-internal deps beyond the standard library.

## Dispatcher command handlers

Telegram: `/start`, `/stop`, `/restart`, `/restart_self`, `/reset`, `/status`, `/watch`, `/cancel`, `/model`, `/effort`, `/debug`, `/forget`, `/help`. Non-commands → `routeToInstance` → `enqueueOrRun` → `driveAgentRun`.

## Agent event types

`internal/agent` emits `Event{Kind: …}` over a channel:

- `EvSessionID` — first line of every run; persists to `Instance.SessionID`.
- `EvAssistantDelta` — streaming text chunk (collected, not yet streamed to Telegram).
- `EvAssistantFinal` — finalized assistant message; canonical reply content.
- `EvError` — API errors (overloaded, rate limit) and parse failures.
- `EvDone` — terminal event; channel closes immediately after.

See `porting/PLAN.md` Appendix A for the source NDJSON shapes.

## Storage buckets

`instances`, `by_topic`, `by_secret` (legacy), `allowed_users`, `settings` (incl. `last_update_id`), `deferred_prompts`. Always use Store methods — never read buckets directly.

## Conventions

- **Two-mark visibility on every user message.** The dispatcher sets 👀 in `enqueueOrRun` (deterministic, before any agent work); the LLM is instructed via the system prompt to call `tg_react("👍")` as its first action ("model has seen it"). The LLM MAY later upgrade to 🎉 / 😅 / ❌. The pre-port queued-message 👀 still works (it's now just a special case of the universal mark).
- **omp is spawned per message.** Don't reintroduce a persistent agent without RFC.
- **Session id is captured from the first NDJSON line.** Pass it as `--resume` on subsequent runs in that instance.
- **`<repo>/.trd/agent.json`** is the source of truth for per-instance model/thinking overrides.
- **`.trd/` is auto-gitignored** in cloned repos.
- **CGo required** for sherpa-onnx (whisper + TTS) and libopus (audio codec).
- **Env vars are persisted** to bbolt settings bucket on first start. Future restarts read from DB.
- **omp gets a TS extension per spawn.** `internal/agent/extension/tg.ts` is embedded into the binary and written to `~/.trd/ext/tg.ts` on dispatcher start. Each `omp -p` run is invoked with `--extension <path>` + `--append-system-prompt <snippet>` and per-spawn env (`TRD_CHAT_ID`, `TRD_THREAD_ID`, `TRD_MESSAGE_ID`, `TRD_DISPATCHER_URL`, `TRD_INSTANCE_ID`, `TRD_TTS_AVAILABLE`). Tools registered: `tg_react(emoji)`, `trd_restart()` (controller-only), and `tg_voice(text)` (TTS → OGG/Opus voice memo, requires TTS configured on dispatcher).
- **Graceful shutdown.** On SIGINT/SIGTERM the dispatcher calls `Shutdown(5s)`: SIGINTs each in-flight omp child's pgid in parallel, waits for them to drain (SIGKILL on timeout), then waits for the `driveAgentRun` goroutines so the final Telegram reply lands before the process exits. Pending queued prompts are dropped.
- **Self-restart.** `RequestRestart(callerID)` sets `pendingRestart`, flushes `pendingQueue` to bbolt `deferred_prompts`, and — when in-flight runs hit 0 — cancels Run so `cmdStart` can `syscall.Exec` the same binary in place. Authorisation: caller's instance must have `Controller=true` (set via `trd promote`). The successor process drains `deferred_prompts` before resuming the long-poll. See DEBUG.md §"Self-restart (implemented)".
- **Streaming replies.** When omp emits text deltas, `driveAgentRun` constructs a `streamingReply` (see `internal/dispatcher/stream.go`) that sends a placeholder via `sendMessage`, debounces edits to ~1.5s using `editMessage`, splits at the 4000-char limit by freezing the current message and rolling over to a fresh one, and force-edits with the canonical `EvAssistantFinal` text once the channel closes. A turn that produces only `EvAssistantFinal` (no deltas) skips the stream and uses the legacy one-shot `splitMessage`+`sendMessage` path.

## Debugging

| Source | Location |
|--------|----------|
| Dispatcher | `tmux attach -t trd` or `~/.trd/trd.log` |
| Per-instance agent stderr | `~/.trd/logs/<instance-id>.log` |
| omp session JSONL | `~/.omp/agent/sessions/<cwd-mangled>/<timestamp>_<session-id>.jsonl` |

Toggle verbose: `/debug` in Telegram or `trd start --debug`.

## Tests

The agent package re-invokes the test binary itself as a fake `omp` (via `TRD_AGENT_TEST_FAKE_OMP_MODE`); see `internal/agent/agent_test.go`. The dispatcher's routing tests use the `runner` test seam (no real omp, no real Telegram) — see `internal/dispatcher/routing_test.go`.
