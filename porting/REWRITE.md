# Rewrite: tmux-free subprocess architecture (HISTORICAL)

> **Status: completed.** This file was the high-level design doc for the
> port; the implementation now lives in `internal/dispatcher`,
> `internal/agent`, and `internal/api`. Kept for reference and for context
> when revisiting the design.


If we remove tmux and use `omp -p` for on-demand subprocess invocations (no persistent agent, no WebSocket bridge, no channel plugin), the architecture simplifies dramatically. This document maps the impact at a file level.

## Architecture before and after

### Current (tmux + persistent Claude)

```
Telegram ──> dispatcher ──WS──> channel plugin ──MCP──> Claude (persistent in tmux)
                                  (per-instance,      (reads stdin, writes stdout,
                                   long-lived)         TUI screen-scraped for state)
```

Dispatcher maintains: WebSocket server, per-instance WS connections, per-instance tmux sessions, health loop, rate-limit watchdog, auto-confirm poller.

### After (omp -p subprocess per message)

```
Telegram ──> dispatcher ──exec──> omp -p (spawned per message, exits after reply)
                                  stdin: message text
                                  stdout: reply text
                                  stderr: log
```

Dispatcher maintains: nothing persistent per instance. Each inbound message spawns a short-lived `omp -p`. The subprocess writes its reply to stdout before exiting. The dispatcher reads stdout and sends the result back to Telegram.

---

## Files that are deleted entirely

| File | Why |
|------|-----|
| `channel/index.ts` | No persistent agent to bridge to. The dispatcher talks directly to `omp -p`. |
| `channel/package.json` | Part of the channel plugin. |
| `channel/tsconfig.json` | Part of the channel plugin. |
| `channel/bun.lock` | Part of the channel plugin. |
| `internal/tmuxmgr/tmuxmgr.go` | No persistent sessions. Subprocesses are fire-and-forget via `os/exec`. |
| `internal/ws/ws.go` | No WebSocket server. The dispatcher no longer accepts inbound connections. |
| `internal/ws/ws_coder.go` | Coder/websocket integration. |
| `internal/ws/ws_test.go` | WS tests. |
| `.mcp.json` generation in `internal/config/` | No MCP config needed. |

---

## Files that need major rewrites

### `internal/dispatcher/dispatcher.go` — The hub, ~1,200 lines → ~500 lines

This file loses roughly 60% of its code.

**Deleted methods (no persistent agent to manage):**

| Method | Lines | Why deleted |
|--------|-------|-------------|
| `launchTmuxWithOpts` | ~40 | No tmux sessions to create |
| `launchTmux` / `launchTmuxFresh` | ~10 | Thin wrappers |
| `autoConfirm` | ~20 | No TUI prompts to dismiss |
| `checkRateLimit` | ~50 | No TUI to scrape for rate limits |
| `resumeInstances` | ~20 | No sessions to recover on restart |
| `healthLoop` | ~20 | No sessions to health-check |
| `sweepAttachments` | ~20 | Can stay, but simpler without session context |
| `cmdModel` | ~30 | Model selection is a CLI flag on `omp -p`, not a runtime toggle |
| `cmdEffort` | ~30 | Same — effort is a CLI flag |
| `handleDelegate` | ~50 | Delegation becomes: spawn `omp -p` in the target repo instead of routing via WS |
| `sendTo` | ~20 | No WS channel to push frames to |
| `findInstance` | ~30 | May stay for delegate lookups |

**Deleted fields on the Dispatcher struct:**

| Field | Why |
|-------|-----|
| `conns map[string]*liveConn` | No WS connections to track |
| `pending map[string]chan ws.Frame` | No async download/TTS responses (subprocess handles synchronously) |
| `rateLimited map[string]bool` | No TUI to scrape for rate limits |
| `pendingDelegates map[string]chan string` | Delegation uses subprocess stdout, not WS channels |

**Rewritten methods:**

| Method | Change |
|--------|--------|
| `routeToInstance` | Spawns `omp -p` subprocess with the message text as stdin. Waits for stdout. Sends stdout back to Telegram. |
| `OnOutbound` | Goes away entirely — there is no outbound WS channel. All "outbound" is now just reading subprocess stdout. |
| `Register` / `Unregister` | Deleted — no WS connections. |
| `AuthSecret` | Deleted — no WS auth needed. |
| `handleDelegate` | Simplified: resolves target repo path, spawns `omp -p` in that directory, returns stdout to the caller. |
| `ListInstances` | Simplified — just reads bbolt, no runtime state (no Connected/TmuxAlive fields). |

**New or simplified logic:**

- `routeToInstance` becomes the core loop: spawn subprocess → read stdout → send Telegram reply. This is ~30 lines.
- Subprocess timeout: if `omp -p` doesn't exit within N seconds, kill it and reply with a timeout error.
- Subprocess working directory: the cloned repo path from bbolt.
- stdin: the message text (plus any attachment metadata the agent might need).
- stdout: the reply text (or structured output if the agent produces richer responses).

### `cmd/trd/main.go` — CLI, ~300 lines → ~200 lines

| Section | Change |
|---------|--------|
| `cmdStart` | No change — token, port, debug flag unchanged. But `Dispatcher.New` takes fewer options (no WS server to configure). |
| `cmdStatus` / `allInstances` | `ListInstances` no longer enriches with Connected/TmuxAlive. Just shows bbolt data. |
| `cmdWatch` | Reads the per-instance log file instead of capturing a tmux pane. |
| `cmdStop` | Kills a running subprocess by PID (if one is active) or is a no-op. Optionally removed — with no persistent agent, there's nothing to stop. |
| `cmdShell` / `cmdCd` | Unchanged. |
| `cmdAllow` / `cmdDeny` / `cmdAllowed` | Unchanged — uses HTTP API, which stays (see ws package below). |

### `internal/ws/ws.go` — HTTP API only, ~280 lines → ~100 lines

The WebSocket server is removed. The HTTP REST API for allowlist management and instance listing stays, since the CLI uses it.

**Kept:**
- `/api/instances` — instance list (simplified: no Connected/TmuxAlive fields)
- `/healthz` — liveness probe
- `GET/POST/DELETE /api/allowed/{username}` — allowlist management

**Deleted:**
- `/channel` — no WS upgrade endpoint
- `ws.Frame` — no frame protocol
- `ws.Conn` — no WS connections
- `ws.Handler` interface — simplified or removed
- `upgradeAndServeCoder` — no WebSocket

The `ws` package may be renamed to `api` or folded into `dispatcher`.

### `channel/index.ts` — Deleted

No persistent agent, no MCP bridge, no WebSocket. The subprocess model eliminates the need for an adapter process entirely.

---

## Files that need minor changes

### `internal/storage/storage.go` — Instance struct

`Manager` field may stay (for delegation routing). `State` field becomes simpler: no `running`/`stopped`/`failed` tmux states. May reduce to `active`/`inactive`.

### `internal/config/config.go`

- `writeMCPConfig` — **deleted.** No `.mcp.json` to generate.
- `EnsureGitignore` — updated to stop adding `.mcp.json`. Still adds `.trd/` and `.omc/`.

### `Makefile`

- Remove `self-modify` target (no channel plugin to point at).
- Remove `restart` target or simplify (no tmux session to send C-c to).
- `setup` target: no `cd channel && bun install` step.

### `package.json`

- Remove `"trd-channel"` bin entry.
- Remove `@modelcontextprotocol/sdk` dependency.
- Remove `channel/` from `"files"`.

### `go.mod`

- Remove `github.com/coder/websocket` dependency.

---

## Files that stay unchanged

| File | Why |
|------|-----|
| `internal/telegram/telegram.go` | Telegram API client — no Claude/tmux coupling |
| `internal/telegram/telegram_test.go` | Tests for the above |
| `internal/storage/storage.go` | bbolt wrapper — platform-agnostic |
| `internal/storage/storage_test.go` | Tests for the above |
| `internal/media/media.go` | Whisper/TTS — platform-agnostic |
| `internal/media/media_test.go` | Tests for the above |
| `internal/audio/audio.go` | OGG/Opus codec — platform-agnostic |
| `scripts/install.sh` | Prereq check — remove tmux, keep the rest |
| `scripts/build-binaries.sh` | Cross-compile — no change |
| `postinstall.js` | Binary platform picker — no change |
| `bin/trd-shim.js` | JS shim — no change |

---

## Delegation model (manager mode)

Without persistent WS connections, delegation changes from "inject a message frame into the target's WS channel and wait for a reply frame" to "spawn `omp -p` in the target repo's directory with the task as stdin."

```
Manager Claude calls delegate(instance="backend", message="Add rate limiting")
  → Manager's channel plugin sends WS frame {type: "delegate", ...}
  → Dispatcher spawns: omp -p serve --cwd /path/to/backend
  → stdin: "Add rate limiting"
  → Subprocess runs, writes result to stdout, exits
  → Dispatcher reads stdout, returns it to manager
```

Wait — in the subprocess model, the manager itself is also a subprocess. So the flow becomes:

```
User sends message to manager topic
  → Dispatcher spawns omp -p in manager's repo
  → Manager subprocess calls delegate(instance, message) tool
  → HOW? The subprocess has no WS connection to the dispatcher.
```

This is the unresolved question for manager mode in a subprocess-only architecture. Options:

1. **Manager subprocess calls dispatcher's HTTP API** — `POST /api/delegate` with target instance and message. Dispatcher spawns the target subprocess, returns the result in the HTTP response.
2. **No manager mode** — if every invocation is stateless and on-demand, there's less need for orchestration. The user just sends messages to the right topic directly.
3. **Subprocess chains** — the manager subprocess can `exec` other subprocesses itself, since it has filesystem access to all repos.

---

## Subprocess contract

The `omp -p` subprocess receives:
- **cwd:** the cloned repo directory
- **stdin:** the user's message text (UTF-8)
- **env:** `TRD_CONFIG` pointing at `.trd/config.json`, `TRD_INSTANCE_ID`
- **args:** `-p serve` (plus `--model`, `--effort`, `--session-id` as needed)

The subprocess writes its reply to stdout and exits with code 0. On error, it writes an error message to stderr and exits non-zero.

No persistent state is kept by the dispatcher. Conversation history lives in the repo's working tree (files modified, git log) and/or the agent's own session store.

---

## What we lose, what we gain

| Lose | Gain |
|------|------|
| Persistent agent sessions (Claude survives across messages) | Zero persistent processes — no memory leaks, no watchdog needed |
| Real-time agent output streaming | Simpler architecture: ~800 lines deleted, ~400 lines simplified |
| Manager orchestration via WS | No WebSocket server, no secret auth, no channel plugin |
| `/watch` capturing a live TUI | No tmux, no screen-scraping, no TUI dependencies |
| Runtime model/effort toggling | Each invocation can have different flags |
| Rate-limit auto-detection and dismissal | Rate limits are the agent's problem, not the dispatcher's |

---

## Migration order

1. Write the subprocess spawn logic in `routeToInstance` (replaces WS frame push).
2. Delete `internal/tmuxmgr/`, `internal/ws/ws_coder.go`, `channel/`.
3. Strip WS server from `internal/ws/ws.go` — keep only HTTP API endpoints.
4. Delete all tmux-scraping methods from `dispatcher.go` (autoConfirm, checkRateLimit, cmdModel, cmdEffort, launchTmuxWithOpts, healthLoop, resumeInstances).
5. Simplify `cmd/trd/main.go` (remove tmux status fields, simplify stop/watch).
6. Update `config.EnsureGitignore` and delete `writeMCPConfig`.
7. Remove `github.com/coder/websocket` from `go.mod`.
8. Update `Makefile`, `package.json`, `scripts/install.sh`.
