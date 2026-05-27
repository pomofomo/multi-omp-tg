# Architecture

TRD is a Go binary that bridges a Telegram supergroup with multiple [omp](https://github.com/oh-my-pi/oh-my-pi) coding agent invocations. Each forum topic maps to one cloned repo; each user message spawns a one-shot `omp -p` subprocess in that repo. This document explains the high-level design, message flows, and key decisions.

## System overview

```
Telegram Supergroup                trd (Go binary)
├── Topic: backend ──┐        ┌── Telegram Bot long-poll
├── Topic: frontend ─┼───────>├── HTTP control plane (localhost:7777)
├── Topic: mobile ───┤        ├── bbolt DB (~/.trd/state.db)
└── /start <repo> ──┘         └── Per-instance FIFO run queue
                                         │
                              On each inbound user message:
                                         ▼
                              omp -p --mode json \
                                  --resume <session-id> \
                                  --model <model> \
                                  --thinking <level> \
                                  "<user text>"
                              (cwd = ~/.trd/repos/<instance-id>)
                                         │
                              stdout NDJSON: session_id,
                              message_update text_delta,
                              message_end (final assistant text),
                              agent_end → exit 0
```

There is no persistent agent. The dispatcher remembers the omp session id per instance in bbolt; subsequent messages resume that session via `--resume`. Conversation history lives entirely in omp's own session storage at `~/.omp/agent/sessions/<cwd-mangled>/`.

## Three components

### 1. Dispatcher (`internal/dispatcher/dispatcher.go`)

The hub. A single long-running Go process that:

- **Long-polls Telegram** for messages, routes them by `message_thread_id` to the correct instance.
- **Serves a small HTTP control plane** (`127.0.0.1:7777`) for the CLI (`/api/instances`, `/api/allowed/*`, `/api/instances/{id}/cancel`, `/healthz`).
- **Spawns `omp -p` per message** via `internal/agent` and consumes its NDJSON event stream.
- **Stores state in bbolt** — instance mappings (now including `SessionID`), user allowlist, settings.
- **Serializes runs per instance** — a second message during an in-flight run queues FIFO and gets a 👀 reaction.

### 2. Agent wrapper (`internal/agent/agent.go`)

A focused 350-line package whose only job is to invoke `omp -p --mode json` and emit classified events to a Go channel. It owns:

- Argv assembly (`--resume`, `--model`, `--thinking`, prompt as final arg).
- NDJSON parsing of stdout, with a 16 MB scanner buffer for long `message_update` lines.
- `classify` — maps each line to `EvSessionID`, `EvAssistantDelta`, `EvAssistantFinal`, `EvError`, or swallows it.
- Cancellation via SIGINT to the process group, with a grace timeout before SIGKILL.

See `porting/PLAN.md` Appendix A for the exact omp NDJSON shapes.

### 3. CLI (`cmd/trd/main.go`)

Subcommand dispatch for managing the dispatcher and instances:

- `trd start` — runs the dispatcher.
- `trd status` / `list` — shows all instances (state, session id, running flag).
- `trd stop <name>` — POSTs `/api/instances/{id}/cancel` to interrupt the in-flight run.
- `trd watch <name>` — reads `~/.trd/logs/<instance-id>.log`.
- `trd shell` / `cd` — convenience for working in the cloned repo.
- `trd allow` / `deny` / `allowed` — user allowlist management.

## Message flow

### A new message in a bound topic

```
User types in Topic:backend
  → Telegram delivers Update{message, message_thread_id}
  → Dispatcher looks up (chat_id, thread_id) → Instance in bbolt
  → buildPrompt merges text + voice transcript + attachment notes
  → enqueueOrRun: if a run is in flight, append to pendingQueue + 👀
                   otherwise spawn agent.Start
  → driveAgentRun ranges over the event stream:
      EvSessionID  → persist Instance.SessionID
      EvDelta      → accumulate (Telegram streaming is a follow-up)
      EvFinal      → canonical reply text
      EvError      → annotate or replace reply
  → After channel closes, send reply via SendMessage (splits at 4000 chars)
  → finishRun: dispatches the next queued prompt if any
```

### Voice messages

```
User sends voice note in topic
  → Telegram delivers Update with Voice attachment
  → Dispatcher downloads OGG → Go Opus decoder → PCM
  → sherpa-onnx whisper transcribes in-process
  → Transcript is appended to the prompt that omp receives
```

Outbound TTS (`send_voice`) is **not** wired in the headless port. Future work.

### Attachments

Documents and photos are downloaded to `~/.trd/attachments/` and surfaced to omp as a trailing `[attachment: <name> (<path>)]` line in the prompt. The agent reads them via its normal `read` tool.

## Identity and persistence

### First /start

1. User sends `/start git@github.com:org/repo.git` in a topic.
2. Dispatcher verifies omp is on `$PATH` (or under `TRD_OMP_BIN`).
3. Generates UUID `instance_id`.
4. `git clone` into `~/.trd/repos/<instance-id>/`.
5. `EnsureGitignore` adds `.trd/` and `.omc/` to the repo's `.gitignore`.
6. Persists `Instance{State: running, SessionID: ""}` to bbolt with three indexes (`instance_id`, `chat_id:thread_id`, `secret` — the last unused after the port).
7. No agent process is started yet — the first user message in the topic does that.

### Per-message resumption

- First message: `omp -p` with no `--resume` → fresh session. The session id is captured from the first NDJSON line and persisted to `Instance.SessionID`.
- Subsequent messages: `omp -p --resume <SessionID>` → omp replays the JSONL log and appends the new turn.
- `/reset` clears `SessionID`. The next message starts a new omp session.
- `/forget` deletes the bbolt row but leaves the cloned repo and the omp session files on disk.

### Health

There is no health loop. omp is invoked anew per message; the only thing to monitor is the dispatcher itself, which the operator does via tmux or a process supervisor.

The dispatcher does sweep stale attachments older than 7 days as part of `ListInstances` — eventual, not periodic.

## Storage

bbolt buckets:

| Bucket | Key | Value | Purpose |
|--------|-----|-------|---------|
| `instances` | instance_id | JSON Instance (incl. SessionID, Controller) | Primary store |
| `by_topic` | chat_id:thread_id | instance_id | Topic lookup |
| `by_secret` | secret | instance_id | Legacy (kept for old rows) |
| `allowed_users` | username | "1" | User allowlist |
| `settings` | env var name / `last_update_id` | value | Persistent config + Telegram long-poll cursor |
| `deferred_prompts` | nanos:seq | JSON DeferredPrompt | Prompts captured during a restart drain; redelivered on the next startup |

The `settings` bucket also stores the Telegram long-poll cursor as
`last_update_id` (decimal int64). Together with `deferred_prompts`,
this is what makes the in-place self-restart lossless — see DEBUG.md
§"Self-restart (implemented)".

`Put` transactionally cleans stale index entries when a row changes.

## Per-repo agent config

`<repo>/.trd/agent.json` carries per-instance model and thinking overrides:

```json
{ "model": "opus", "thinking": "high", "updated_at": "2026-05-27T08:30:00Z" }
```

`/model` and `/effort` rewrite this file. The next `omp -p` invocation reads it and passes `--model` / `--thinking` accordingly. Empty fields = omp defaults.


## omp extension (tools injected into the agent)

The dispatcher embeds a tiny TypeScript file (`internal/agent/extension/tg.ts`) into the binary and writes it to `~/.trd/ext/tg.ts` on startup. Each `omp -p` invocation is launched with:

- `--extension ~/.trd/ext/tg.ts` — loads the file via omp's jiti-based extension loader.
- `--append-system-prompt <snippet>` — the ACKNOWLEDGE / REPLY WHEN DONE / ASK QUESTIONS interaction pattern (verbatim from the pre-port `channel/index.ts`) plus the two-mark visibility convention.
- Per-spawn env vars: `TRD_CHAT_ID`, `TRD_MESSAGE_ID`, `TRD_DISPATCHER_URL`, `TRD_INSTANCE_ID`.

The extension registers two tools:
- `tg_react(emoji)` — POSTs `{chat_id, message_id, emoji}` to `/api/tg/react`; the dispatcher calls Telegram's `setMessageReaction`. The bot token never leaves the dispatcher process.
- `trd_restart()` — POSTs to `/api/restart` with `X-Trd-Instance: <id>` from env. Returns 403 if the calling instance isn't flagged as the controller.

### Two-mark visibility chain

Every user message picks up two reactions in sequence so the sender can see exactly where the request is:

| Mark | Set by | When | Meaning |
|------|--------|------|---------|
| 👀 | Dispatcher (deterministic, in `enqueueOrRun`) | Within ~ms of receiving the Telegram update, before `omp -p` is even spawned | "system received it" |
| 👍 | LLM (via `tg_react` per the system prompt) | First action of the turn, before any other tool calls or text | "model has seen it" |

If 👀 appears but 👍 never does, the dispatcher routed the message but the agent failed to act on it — useful diagnostic. The LLM MAY upgrade the emoji later (🎉 / 😅 / ❌) to reflect outcome.

## HTTP API

The control plane has only what the CLI needs:

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/healthz` | GET | Liveness probe |
| `/api/instances` | GET | Instance list (JSON) |
| `/api/allowed` | GET | Allowlist |
| `/api/allowed/{user}` | POST | Add to allowlist |
| `/api/allowed/{user}` | DELETE | Remove from allowlist |
| `/api/instances/{id}/cancel` | POST | Interrupt in-flight run |
| `/api/tg/react` | POST | Add an emoji reaction to a Telegram message (used by the omp `tg_react` tool) |
| `/api/instances/{id}/promote` | POST/DELETE | Set / clear the `Controller` flag (single-controller enforced) |
| `/api/restart` | POST | Graceful in-place restart. Requires `X-Trd-Instance` header matching a controller instance; 403 otherwise. See DEBUG.md §"Self-restart (implemented)". |

No WebSocket. No auth (binds to `127.0.0.1` only).

## Key design decisions

- **One-shot omp per message.** No persistent agent. Simpler operational story, bounded memory, no watchdog needed.
- **omp owns conversation continuity.** Session id captured per run, persisted in bbolt, replayed via `--resume`. No JSONL parsing on our side.
- **Per-instance FIFO queue.** Concurrent messages in one topic are serialised so the user sees ordered replies.
- **Dispatcher does all Telegram calls.** Inbound messages, outbound replies, voice transcription, reactions — all in the Go process.
- **One topic = one repo.** Enforced — `/start` in an already-bound topic is rejected.
- **Forum supergroup required.** Non-forum chats are rejected with a helpful error.
- **HTTP API on localhost only.** No external network exposure.
- **Env vars persisted to bbolt.** First start saves config; future restarts need no env vars.

## Lost vs. gained vs. headless port

| Lost | Gained |
|------|--------|
| Persistent agent sessions ("Claude" running in tmux) | Bounded memory: zero idle processes |
| Live TUI capture (`/watch` showed the terminal) | `/watch` reads a clean log file |
| Manager mode (delegation between instances) | Simpler architecture; delegation can be re-added as an HTTP endpoint |
| Outbound TTS (`send_voice` tool) | Plan documents how to re-add via a sentinel marker |
| `/model` and `/effort` as interactive Claude commands | Per-repo `.trd/agent.json` is greppable and version-able |
| Real-time streaming of assistant tokens | Final reply only (follow-up: stream via Telegram `editMessageText`) |

## Roadmap

Roughly prioritized:

- Streaming Telegram edits during a run (so long replies feel live).
- Delegation between instances via `POST /api/delegate`.
- Re-add outbound TTS — sentinel marker in assistant text → dispatcher synthesizes and sends voice.
- Auto-download inbound photos (pre-fetch instead of attachment dance).
- CI / release automation (GitHub Actions → tagged releases with prebuilt binaries per host arch).
- Branch-aware topics (git worktrees).
- Remote instances via SSH.
