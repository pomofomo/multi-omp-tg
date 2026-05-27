# Telegram ↔ omp bridge: architectural options

Two viable shapes for a Go bridge that proxies Telegram messages into omp. Both
use the same engine (`AgentSession` + `SessionManager`); they differ in **who
owns process lifetime** and **what wire format the bridge speaks**.

## Option A — `omp -p` per message (subprocess-per-turn)

### Wire shape

```
                ┌─────────────┐                             ┌──────────────┐
Telegram ───▶   │  Go bridge  │ ──spawn (one per message)── │ omp -p (Bun) │
                │ (long-lived)│         JSONL on disk       └──────┬───────┘
                └──────▲──────┘                                    │
                       │                                           ▼
                       └──────── stdout (text or NDJSON) ──────────┘
```

Per message, the bridge runs roughly:

```
omp -p --resume <sessionId> --mode json --cwd <workdir> "<msg>"
```

- `--resume <id>` loads the JSONL session log (`~/.omp/sessions/...`) and
  appends to it. If `<id>` is unknown, omp creates a new session and prints
  a header line you can capture and persist alongside the Telegram chat ID.
- `--mode json` streams every `AgentSessionEvent` as a JSON object per line
  (assistant tokens, tool starts/results, plan updates, errors). `--mode text`
  or `-p` alone emits only the final assistant message — easier to parse but
  loses tool telemetry.
- The process exits after `prompt()` settles (`session.dispose()` runs at the
  tail of `runPrintMode` in `packages/coding-agent/src/modes/print-mode.ts`).

### Entry point

`packages/coding-agent/src/main.ts` → branch around line 1014, calling
`runPrintMode(session, { mode, messages, initialMessage, initialImages })`.

`SessionManager.open(arg, sessionDir?)` resolves the resume target
(`packages/coding-agent/src/main.ts` ~ line 400). Argument shapes accepted:

| `--resume` value           | Resolution                                       |
| -------------------------- | ------------------------------------------------ |
| absolute/relative path     | open that JSONL file directly                    |
| ends with `.jsonl`         | open that JSONL file directly                    |
| bare id (no `/`, no `\\`)  | look up under `~/.omp/sessions/<cwd-hash>/<id>`  |
| `-c` / `--continue`        | most recent session in the current `cwd`         |

### State ownership

- **Authoritative state lives on disk** (`~/.omp/sessions/.../*.jsonl`).
- **No in-memory state survives across messages.** Every turn cold-loads the
  JSONL, replays it through `SessionManager`, prompts, persists, exits.
- MCP servers, LSP servers, file watchers, and extensions are spun up per turn
  and torn down on exit.

### Bridge code shape (Go)

```go
cmd := exec.CommandContext(ctx, "omp", "-p",
    "--resume", sessionID,
    "--mode", "json",
    "--cwd", workdir,
    message)
cmd.Stderr = logSink
out, _ := cmd.StdoutPipe()
_ = cmd.Start()

scanner := bufio.NewScanner(out)
scanner.Buffer(make([]byte, 1<<20), 16<<20) // tool outputs can be large
for scanner.Scan() {
    var ev map[string]any
    _ = json.Unmarshal(scanner.Bytes(), &ev)
    // forward text deltas to Telegram, ignore the rest, or render tool calls
}
_ = cmd.Wait()
```

The bridge needs **one** Go abstraction: `omp.Run(ctx, sessionID, msg) -> events`.

---

## Option B — `omp acp` long-lived per chat

### Wire shape

```
                ┌──────────────────┐  initialize / newSession / prompt   ┌──────────────────┐
Telegram ───▶   │   Go bridge      │ ──────── JSON-RPC (NDJSON) ────────▶│  omp acp (Bun)   │
                │  (ACP client     │                                     │  long-lived,     │
                │   per chat)      │◀──── sessionUpdate notifications ───│  one per chat    │
                └──────────────────┘                                     └──────────────────┘
```

### Entry point

`packages/coding-agent/src/commands/acp.ts` forces `mode: "acp"` and delegates
to the same launcher as the TUI. The ACP machinery lives in
`packages/coding-agent/src/modes/acp/`:

| File                    | Role                                                       |
| ----------------------- | ---------------------------------------------------------- |
| `acp-mode.ts`           | Wires stdio through `ndJsonStream` into `AgentSideConnection` |
| `acp-agent.ts`          | Implements the `Agent` interface; ~2k LOC                  |
| `acp-event-mapper.ts`   | `AgentSessionEvent` → ACP `SessionUpdate`                  |
| `acp-client-bridge.ts`  | Routes tool I/O *out* to the client (gated by capabilities)|

RPCs the bridge sends:

| Request                  | When                                                 |
| ------------------------ | ---------------------------------------------------- |
| `initialize`             | once, immediately after spawn                        |
| `authenticate`           | once if a `methodId` is offered and needed           |
| `newSession` / `loadSession` / `resumeSession` | once per chat session start         |
| `prompt`                 | per Telegram message                                 |
| `cancel`                 | when the user sends `/cancel` or a new message lands while one is in flight |
| `setSessionMode` / `setSessionConfigOption` | optional: switch plan mode, model, thinking |
| `closeSession`           | on shutdown                                          |

Notifications the bridge consumes (`sessionUpdate`):

- `agent_message_chunk` — assistant text deltas → Telegram message edits
- `tool_call`, `tool_call_update` — tool lifecycle (for UX/telemetry)
- `plan` — todo updates from `todo_write`
- `current_mode_update`, `available_commands_update`

### Client capabilities — what NOT to advertise

`AcpAgent.initialize()` returns capabilities; the agent then asks the *client*
what it supports. `acp-client-bridge.ts` gates routing on these client-side
flags:

```ts
readTextFile: clientCapabilities?.fs?.readTextFile === true,
writeTextFile: clientCapabilities?.fs?.writeTextFile === true,
terminal:     clientCapabilities?.terminal === true,
```

A Telegram bridge has no buffer model, no editor save path, no embedded
terminal — so it MUST advertise **none** of these. omp then falls back to its
own direct filesystem and PTY paths, exactly as in `-p` mode. The bridge only
implements:

- `session/update` notification consumer (text rendering)
- `session/request_permission` responder (auto-allow, or DM the user a Y/N)
- Optionally `fs/read_text_file` / `fs/write_text_file` if you want the chat
  to be the source of truth for files — almost certainly not.

### State ownership

- **Authoritative state still lives on disk** (same JSONL store).
- **Hot state lives in the Bun process**: tokenizer caches, MCP child
  processes, LSP servers, file watchers, the in-memory `AgentSession`.
- One Bun process per active chat (or per pool slot). Crash = lose hot state;
  must `loadSession` the JSONL again.

### Bridge code shape (Go)

You implement the *client* side of ACP. There is no Go SDK upstream; you
either:

1. Use a generic JSON-RPC 2.0 library over NDJSON and hand-roll the message
   types from `@agentclientprotocol/sdk` types (~30–40 message shapes), or
2. Generate Go bindings from the ACP schema at
   <https://github.com/zed-industries/agent-client-protocol>.

Plus a per-chat process manager: spawn, health-check, restart on crash,
idle-timeout, drain on shutdown.

---

## Comparison

| Axis                    | A — `omp -p` per message                  | B — `omp acp` long-lived             |
| ----------------------- | ----------------------------------------- | ------------------------------------ |
| Bridge code (Go)        | `exec.Command` + line scanner             | JSON-RPC client + protocol types + process pool |
| Per-message latency     | + cold start (Bun + extensions + MCP + JSONL replay) | RPC round-trip only       |
| Idle memory             | 0 (no resident omp process)               | full omp RSS × active chats          |
| Memory ceiling          | bounded by concurrent in-flight turns     | grows with concurrent live chats     |
| Crash blast radius      | one turn                                  | the whole chat's hot state           |
| MCP / LSP lifecycle     | spawned + reaped per turn                 | one set per chat, lifetime of process|
| Cancellation            | `cmd.Process.Kill()`                      | `cancel` RPC, clean abort            |
| Streaming UX            | per-line NDJSON, simple                   | `sessionUpdate` notifications, rich  |
| Multi-turn coherence    | full (state on disk)                      | full (state in-process + disk)       |
| Failure surface         | OS process boundary; trivially supervised | long-lived protocol state machine    |
| External dependencies   | none beyond `omp` binary                  | NDJSON framing + protocol versioning |

## Recommendation

**Start with Option A.** For a Telegram bridge specifically, the trade-offs
all point the same way:

- **Telegram traffic is bursty and human-paced.** A chat is idle 99% of the
  time. Holding a Bun process per chat to save a few hundred ms of cold start
  on the 1% of seconds where a message lands is the wrong direction — you pay
  steady-state RSS for nothing.
- **The crash story is much better.** A model error, an MCP server going
  rogue, a runaway tool — in Option A it takes down one turn and the next
  message gets a fresh process. In Option B you're writing supervision logic
  to detect and recycle wedged Bun processes per chat, and clients
  re-attaching via `loadSession` after each restart.
- **The Go side is one `exec.Command` and a line scanner.** No protocol
  client to maintain, no message-shape drift to track when omp's ACP surface
  evolves, no per-chat lifecycle bookkeeping.
- **The bridge gains nothing from ACP's editor features.** ACP exists to let
  editors own the buffer, save path, and terminal. Telegram owns none of
  these, so you'd be running ACP with all client capabilities turned off —
  i.e. asking omp to talk to itself through a JSON-RPC layer.

**When to revisit:** if cold start becomes a perceptible problem (typically
1–3s on first turn, faster once Bun's module cache is warm on the host),
introduce a **small warm pool** of `omp acp` processes keyed by chat ID with
an idle timeout (e.g. evict after 10 min idle). That is a strict superset of
the per-message design — same JSONL on disk, same `loadSession` path — so you
can adopt it later without rewriting the bridge.

## Concrete next steps for Option A

1. Persist `(telegram_chat_id → omp_session_id, cwd)` in the bridge's store.
   On first message in a chat: omit `--resume`, capture the session id from
   the JSON header line emitted in `--mode json`.
2. Run with `--mode json` and at minimum render `message_update` /
   `message_end` text deltas. Optionally render `tool_execution_start` events
   as Telegram status pings ("running `bash`…").
3. Use `--cwd <per-chat workspace>` so each chat is sandboxed to its own
   directory. Combine with `--no-skills` / `--tools=...` if you want to
   constrain capabilities for untrusted chats.
4. Wire `context.Cancel()` to `cmd.Process.Signal(os.Interrupt)` so user
   `/cancel` propagates as SIGINT (omp handles it cleanly via the abort
   path).
5. Wrap `exec.Command` with a retry on exit code 1 only when the failure was
   a transient model error (parse the last JSON event); don't retry on tool
   permission denials or auth failures.
