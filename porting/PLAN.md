# TRD → omp headless port: refactoring plan

This plan is the execution blueprint for replacing the **tmux + persistent
Claude + MCP channel plugin + WebSocket bridge** stack with **`omp -p`
subprocess-per-message**, per `porting/REWRITE.md` (target architecture) and
`porting/BRIDGE.md` Option A (wire protocol).

> `porting/tmux.md` is **not** the target. It explored a halfway design that
> kept a long-lived agent and WS channel. Where REWRITE.md and tmux.md
> disagree, **REWRITE.md wins**. tmux.md remains useful only as a reference
> catalog of every tmux call site.

---

## 0. Target architecture (one screen)

```
                       ┌──────────────────────────────┐
Telegram long-poll ──▶ │ Dispatcher (single Go binary)│
                       │                              │
                       │  per inbound user message:   │
                       │    spawn omp -p              │
                       │      --resume <session-id>   │
                       │      --mode json             │
                       │      --cwd <repo>            │
                       │      "<message text>"        │
                       │    read stdout NDJSON        │
                       │    forward text chunks to TG │
                       │    cmd.Wait(), free slot     │
                       └──────────────────────────────┘
                                  │
                       ┌──────────┴──────────┐
                       │ bbolt: instances    │
                       │   + session_id      │
                       │   + allowlist       │
                       │   + settings        │
                       └─────────────────────┘
```

**Nothing persistent per instance.** No tmux. No WebSocket server. No channel
plugin. No MCP. No screen scraping. No rate-limit watchdog (omp owns that).
No autoConfirm. No health loop.

The dispatcher keeps **one new concept**: a per-instance **session id** (the
omp session identifier returned in the first JSON header line). It is stored
in bbolt next to the repo path, and supplied as `--resume <id>` on every
subsequent invocation in that topic.

---

## 1. Wire contract with `omp -p`

Per Telegram message in topic `T` bound to instance `I`:

```
omp -p \
    --mode json \
    [--resume <I.SessionID>]   # omitted on first invocation
    [--model <cfg.Model>]      # if persisted (fuzzy match: "opus", "haiku", …)
    [--thinking <cfg.Thinking>]# if persisted (minimal, low, medium, high, xhigh)
    "<user message text>"
```

The process is run with `cmd.Dir = I.RepoPath` (omp has no `--cwd` flag —
the cwd is inherited from the parent).

- **stdin**: closed immediately after spawn (omp -p reads the prompt from
  argv, not stdin).
- **stdout**: NDJSON, one event per line. The dispatcher only **needs** to
  surface assistant text to Telegram and capture the session id from the
  first `type:"session"` event; everything else (tool calls, plan updates)
  is logged but not forwarded for the initial port.
- **stderr**: appended verbatim to `~/.trd/logs/<instance-id>.log`. Surfaced
  by `/watch` and `trd watch`.
- **exit code**: 0 on success (including soft errors like API overloaded —
  omp reports those via `errorMessage` on the assistant message). Non-zero →
  reply "agent exited <code>; see /watch".
- **cancellation**: `ctx.Done()` (user-initiated `/cancel`, dispatcher
  shutdown) → `cmd.Process.Signal(os.Interrupt)`, then 5s grace, then
  `cmd.Process.Kill()`.

Session-id capture: scan the JSON stream for the first event carrying a
session identifier. The exact field name is not fixed in `BRIDGE.md` — the
dispatcher MUST treat the first JSON object whose shape contains a
`session_id` (or equivalent identifier returned by omp's `--mode json`
header) as the source of truth. Reflect-and-store on first run; verify in
implementation by running `omp -p --mode json --cwd /tmp/x "hi"` once and
recording the exact key.

**Acceptance check before declaring "wire layer done"**: a `trd test-prompt`
flag (or unit-level integration test) that drives one prompt end-to-end and
prints the captured session id, then re-runs with `--resume` and verifies
omp loads the same conversation file.

---

## 2. File-by-file delta

Legend: **DELETE** = remove file entirely. **REWRITE** = the file remains
but >50% of its content changes. **TRIM** = small surgical edits.

| File | Action | Notes |
|------|--------|-------|
| `channel/index.ts` | **DELETE** | Plus `channel/package.json`, `channel/tsconfig.json`, `channel/bun.lock`. Whole `channel/` dir goes. |
| `internal/tmuxmgr/tmuxmgr.go` | **DELETE** | No persistent process to manage. |
| `internal/ws/ws_coder.go` | **DELETE** | No WS upgrade. |
| `internal/ws/ws_test.go` | **DELETE** | Tests are for WS server. |
| `internal/ws/ws.go` | **REWRITE** → rename to `internal/api/api.go` | Keep only HTTP REST surface used by CLI: `/api/instances`, `/api/allowed/*`, `/healthz`. Drop `Frame`, `Conn`, `Handler`, `serveConn`, `handleChannel`, the `wsWriter` plumbing, and the `coder/websocket` import. ~280 → ~120 LOC. |
| `internal/dispatcher/dispatcher.go` | **REWRITE** | ~1700 → ~700 LOC. See §3. |
| `internal/agent/agent.go` (new) | **CREATE** | Wraps `omp -p` invocation, NDJSON parser, cancellation. See §4. |
| `internal/storage/storage.go` | **TRIM** | Add `SessionID string` to `Instance`. Add `State State` simplification — keep `running`/`stopped`/`failed` for now but only `stopped`/`active` are exercised (active when SessionID set OR ready to launch; failed only on clone failure). |
| `internal/config/config.go` | **TRIM** | Delete the `Secret` and `DispatcherPort` fields on `RepoConfig`. Drop `WriteRepoConfig` writes entirely if nothing else consumes it (verify with grep). Update `EnsureGitignore` to stop adding `.mcp.json`; keep `.trd/`, `.omc/`. |
| `internal/dispatcher/dispatcher_test.go` | **REWRITE** | Drop WS-frame tests; add subprocess-mock-based routing tests. |
| `cmd/trd/main.go` | **TRIM** | Remove `tmuxmgr` calls in `cmdStatus`/`cmdStop`/`cmdWatch`. `cmdWatch` reads the log file. `cmdStop` cancels an in-flight run via API (or is removed; see §5). Status no longer shows `tmux=`/`channel=`. |
| `Makefile` | **TRIM** | Remove `self-modify` and `cd channel && bun install` from `setup`. Keep `restart` as a rebuild+systemd/tmux launcher of the dispatcher itself (the dispatcher still runs in a tmux for the operator; that is a *deployment* concern, not an instance concern). Decide explicitly per §10. |
| `package.json` | **TRIM** | Drop `"trd-channel"` bin, `@modelcontextprotocol/sdk`, and `channel/` from `files`. |
| `postinstall.js` / `bin/trd-shim.js` | unchanged | Binary picker. |
| `scripts/install.sh` | **TRIM** | Remove tmux prereq check and the channel `bun install` step. Add `omp` prereq check. |
| `go.mod` / `go.sum` | **TRIM** | Drop `github.com/coder/websocket`. |
| `internal/media/*` | unchanged | Whisper/TTS still used for inbound voice transcription and (optionally) outbound `send_voice`. See §6 — outbound TTS moves into a different surface or is dropped initially. |
| `internal/telegram/*` | unchanged | |
| `internal/audio/*` | unchanged | |
| `README.md` / `DEV.md` / `CLAUDE.md` / `ARCHITECTURE.md` | **REWRITE** (last phase) | After code lands. Out of scope of code phases. |

---

## 3. Dispatcher: new shape

### 3.1 New `Dispatcher` struct

```go
type Dispatcher struct {
    opts   Options
    logger *slog.Logger
    tg     *telegram.Client
    store  *storage.Store
    api    *api.Server         // was *ws.Server
    media  *media.Engine

    // One in-flight agent run per instance. New messages while a run is
    // active are queued FIFO per instance. Optionally bounded.
    runMu sync.Mutex
    runs  map[string]*agentRun  // instanceID -> active run handle
    queue map[string][]queued   // instanceID -> waiting prompts
}

type agentRun struct {
    cmd    *exec.Cmd
    cancel context.CancelFunc
    msgID  int        // telegram message id this run is responding to
    chatID int64
    thread int
}

type queued struct {
    chatID, msgID int
    thread        int
    text          string
    attach        attachment   // file id + name + transcript
}
```

**Removed fields** (vs. current):
- `mu`, `conns`, `liveConn` — no WS.
- `pendingMu`, `pending` — no async WS responses.
- `rateLimitedMu`, `rateLimited` — omp owns rate-limit handling.
- `delegateMu`, `pendingDelegates` — delegation reshaped (§7).

**InstanceInfo**: drop `Connected` and `TmuxAlive`. Add `Running bool` (true
iff there is an active `runs[instanceID]`).

### 3.2 Lifecycle

```go
func (d *Dispatcher) Run(ctx) error {
    // 1. Start HTTP API (instances + allowlist).
    go d.api.ListenAndServe(ctx)
    // 2. Long-poll Telegram.
    return d.pollLoop(ctx)
}
```

No `resumeInstances`. No `healthLoop`. No `sweepAttachments` on a timer
(move to a one-shot at startup, or run inside the API server lazily).

### 3.3 Commands surviving in `handleMessage`

Keep: `/start`, `/stop`, `/restart`, `/reset`, `/status`, `/watch`,
`/debug`, `/forget`, `/help`, `/model`, `/effort`, `/cancel` (new).

Drop: `/manager` (see §7).

Per-command semantics in the subprocess world:

| Command | New behaviour |
|--------|---------------|
| `/start <url>` | Clone repo, persist `Instance{State: stopped, SessionID: ""}`. Do **not** spawn anything. First user message triggers the first omp run. |
| `/stop` | If a run is active, cancel it. Set `State = stopped`. No process to "kill" beyond the in-flight run. |
| `/restart` | No-op for process — there is no daemon. Just clear "stopped" flag back to "active". Reply "ready". |
| `/reset` | Clear `SessionID` in bbolt. Next message starts a new omp session. |
| `/status` | Report repo path, current session id (8 char), whether a run is in flight. |
| `/watch` | `tail -n 200` of `~/.trd/logs/<id>.log`, truncated to 4 KB. |
| `/forget` | Delete bbolt row. Cancel any active run first. |
| `/debug` | Toggle a setting persisted in bbolt; consulted on every spawn to attach extra logging (`omp` itself has no `--debug` flag; we tee stderr and bump our own slog level). |
| `/model [name]` | Write `Model` to per-repo config file `<repo>/.trd/agent.json`. Consumed at next spawn as `--model`. Empty arg → show current. |
| `/effort [level]` | Same as model, for `--thinking`. Effort levels accepted: `minimal`, `low`, `medium`, `high`, `xhigh`. |
| `/cancel` | New. SIGINT the in-flight run for this topic. |
| `/help` | Updated text. |

### 3.4 `routeToInstance` — the new hot path

```go
func (d *Dispatcher) routeToInstance(ctx, m, text) {
    inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
    if inst == nil { /* silently ignore */ return }
    if inst.State == storage.StateStopped {
        d.sendText(...; "instance stopped; use /restart"); return
    }
    // Resolve attachments + voice transcription (logic kept from current
    // routeToInstance lines 1139–1177).
    prompt := buildPrompt(text, attach)

    d.enqueueOrRun(ctx, inst, m, prompt)
}

func (d *Dispatcher) enqueueOrRun(ctx, inst, m, prompt) {
    d.runMu.Lock()
    if _, busy := d.runs[inst.InstanceID]; busy {
        d.queue[inst.InstanceID] = append(d.queue[inst.InstanceID], queued{...})
        d.runMu.Unlock()
        d.sendReact(m, "👀")   // optional: acknowledge queued
        return
    }
    runCtx, cancel := context.WithCancel(context.Background())
    run := &agentRun{cancel: cancel, msgID: m.MessageID, chatID: m.Chat.ID, thread: m.MessageThreadID}
    d.runs[inst.InstanceID] = run
    d.runMu.Unlock()

    go d.driveAgentRun(runCtx, inst, run, prompt)
}

func (d *Dispatcher) driveAgentRun(ctx, inst, run, prompt) {
    defer d.finishRun(inst.InstanceID)

    cfg, _ := readAgentConfig(inst.RepoPath)
    opts := agent.RunOptions{
        Cwd:       inst.RepoPath,
        SessionID: inst.SessionID,    // "" on first run
        Model:     cfg.Model,
        Thinking:  cfg.Thinking,
        Debug:     d.opts.Debug || inst.Debug,
        Prompt:    prompt,
        LogPath:   logPathFor(inst.InstanceID),
    }
    events, err := agent.Start(ctx, opts)   // returns <-chan agent.Event
    if err != nil {
        d.sendText(ctx, run.chatID, run.thread, "agent spawn failed: "+err.Error())
        return
    }
    run.cmd = events.Cmd()           // for /cancel SIGINT path
    streamer := newReplyStreamer(d, run)
    for ev := range events {
        switch ev.Kind {
        case agent.EvSessionID:
            if inst.SessionID == "" {
                inst.SessionID = ev.SessionID
                _ = d.store.Put(*inst)
            }
        case agent.EvAssistantText:
            streamer.Append(ev.Text)        // edits the Telegram message
        case agent.EvToolCall:
            d.logger.Debug("tool", "name", ev.ToolName, ...)
        case agent.EvError:
            streamer.Flush()
            d.sendText(ctx, run.chatID, run.thread, "agent error: "+ev.Text)
        }
    }
    streamer.Finalize()
}

func (d *Dispatcher) finishRun(instID string) {
    d.runMu.Lock()
    delete(d.runs, instID)
    next := d.queue[instID]
    if len(next) > 0 {
        d.queue[instID] = next[1:]
        // drive next prompt in a new goroutine; release lock first
    }
    d.runMu.Unlock()
    // ... drive next if present
}
```

**Reply streaming**: omp emits assistant text in chunks. To keep Telegram
sane, the streamer buffers chunks and either:
1. (Simple) writes the full reply once at the end, splitting on the 4000-char boundary (existing `splitMessage`).
2. (Better, follow-up) `SendMessage` immediately with the first chunk, then `EditMessageText` on every Nth chunk or every M ms.

**For the initial port, implement option 1.** Streaming edits are a follow-up
once correctness is in.

---

## 4. `internal/agent/agent.go` — the omp wrapper

The single Go abstraction the rest of the codebase talks to.

```go
package agent

type Event struct {
    Kind      Kind   // EvSessionID | EvAssistantText | EvToolCall | EvToolResult | EvError | EvDone
    SessionID string // only on EvSessionID
    Text      string // EvAssistantText, EvError (free-form)
    ToolName  string // EvToolCall
    Raw       json.RawMessage // original line for debug
}

type RunOptions struct {
    Cwd       string
    SessionID string   // "" for new session
    Model     string   // "" → omit flag
    Thinking  string  // "" → omit flag; values: minimal, low, medium, high, xhigh
    Debug     bool    // toggles extra logging on our side; omp has no --debug flag
    Prompt    string
    LogPath   string   // stderr tee target
    Binary    string   // optional override; default "omp"
}

type Run struct {
    cmd    *exec.Cmd
    events chan Event
}

func (r *Run) Events() <-chan Event { return r.events }
func (r *Run) Cmd() *exec.Cmd       { return r.cmd }

// Start spawns omp -p, returns immediately with a Run whose Events channel
// is drained by a background goroutine that parses NDJSON from stdout.
// The channel closes when omp exits (cmd.Wait() returns).
func Start(ctx context.Context, opts RunOptions) (*Run, error) {
    args := []string{"-p", "--mode", "json"}
    if opts.SessionID != "" { args = append(args, "--resume", opts.SessionID) }
    if opts.Model != ""     { args = append(args, "--model", opts.Model) }
    if opts.Thinking != "" { args = append(args, "--thinking", opts.Thinking) }

    args = append(args, opts.Prompt)

    bin := opts.Binary
    if bin == "" { bin = firstNonEmpty(os.Getenv("TRD_OMP_BIN"), "omp") }

    cmd := exec.CommandContext(ctx, bin, args...)
    cmd.Dir = opts.Cwd
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // isolate signals
    stdout, _ := cmd.StdoutPipe()
    logFile, _ := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
    cmd.Stderr = logFile
    if err := cmd.Start(); err != nil {
        logFile.Close()
        return nil, err
    }

    ch := make(chan Event, 32)
    go parseLoop(stdout, ch, logFile, cmd)
    return &Run{cmd: cmd, events: ch}, nil
}

func parseLoop(stdout io.ReadCloser, ch chan<- Event, logFile *os.File, cmd *exec.Cmd) {
    defer close(ch)
    defer logFile.Close()
    s := bufio.NewScanner(stdout)
    s.Buffer(make([]byte, 1<<20), 16<<20)
    for s.Scan() {
        var obj map[string]any
        if err := json.Unmarshal(s.Bytes(), &obj); err != nil {
            ch <- Event{Kind: EvError, Text: "bad json: " + s.Text()}
            continue
        }
        ev := classify(obj, s.Bytes())
        ch <- ev
    }
    _ = cmd.Wait()
}

// classify maps an omp NDJSON event to our Event type. The exact key names
// are determined empirically (see §1 acceptance check); see TODO at top of
// agent.go listing the cases that MUST be implemented before the port is
// declared done.
func classify(obj map[string]any, raw []byte) Event { /* … */ }
```

**Empirically-determined contract**: before writing `classify`, run

```
omp -p --mode json --cwd /tmp/scratch "say hello"
```

and pin the field names. Document them in a comment block at the top of
`agent.go`. Any future omp protocol drift becomes a single-file change.

---

## 5. CLI (`cmd/trd/main.go`)

| Subcommand | Change |
|-----------|--------|
| `trd start` | Unchanged surface. Drops `tmuxmgr` references entirely. |
| `trd status` / `trd list` | Drops `tmux=`/`channel=` columns. Adds `running=` (active agent run, if any) and `session=<short>`. |
| `trd stop <name>` | Calls a new `POST /api/instances/{id}/cancel` endpoint (cancels in-flight run + sets State=stopped). |
| `trd watch <name>` | `read` of `~/.trd/logs/<id>.log`, last 200 lines to stdout. |
| `trd shell` / `trd cd` | Unchanged. |
| `trd allow` / `trd deny` / `trd allowed` | Unchanged (use HTTP API). |

Remove `instanceInfo.TmuxAlive`. Add `Running bool` and `SessionID string`.

---

## 6. Voice and attachments

**Inbound** (Telegram → agent): keep current behaviour. Voice → whisper →
transcript text appended to the prompt. Images/documents → download to
`AttachDir` and append a `[attachment: <name>]` line to the prompt; the
agent can read it from disk via the supplied path. **Pass attachment paths
as text inside the prompt**, since there is no MCP tool to call.

**Outbound** (agent → Telegram TTS): the old `send_voice` MCP tool no longer
exists. Two options:

- **Option A (recommended for v1)**: drop outbound TTS. The agent replies
  with text only; the dispatcher TTS path is removed from the dispatcher
  hot path (the `media` engine stays in tree for inbound transcription).
- **Option B (follow-up)**: introduce a sentinel marker the agent can emit
  in its assistant text (e.g. lines starting with `<voice>...</voice>`) and
  have the dispatcher detect, strip, synthesise, and send as voice. No new
  protocol needed.

Plan goes with **Option A** for the initial port.

---

## 7. Delegation / manager mode

The current `/manager` toggle is meaningful only because the persistent
agent had a delegate MCP tool that sent WS frames to the dispatcher. In a
subprocess world the agent has no live channel to the dispatcher.

**Initial port: drop `/manager` and the delegate frame path entirely.**
`Instance.Manager` stays in the struct for forward-compat but is unused.

**Follow-up design (not in this PR)**: expose a `POST /api/delegate` HTTP
endpoint on the dispatcher. The omp invocation gets `TRD_DISPATCHER_URL`
in env; an `omp` plugin or simple shell wrapper can `curl` the endpoint
to delegate. Out of scope here.

---

## 8. Storage migration

Add `SessionID string` and (optionally) `Debug bool` to `storage.Instance`.
bbolt stores raw JSON; an older row without these fields decodes with zero
values, which is the correct behaviour (no session yet, debug off).
**No migration code required.**

Drop `Secret` consumption (the field can stay on disk; nothing reads it).
Same for `FailCount` — keep, unused.

---

## 9. Per-repo agent config file

`<repoPath>/.trd/agent.json`:

```json
{ "model": "sonnet", "effort": "high", "updated_at": "2026-05-27T12:00:00Z" }
```

- Written by `/model` and `/effort` commands.
- Read by `driveAgentRun` on every spawn (no caching, no watching needed —
  reads are cheap and bounded by message rate).
- Missing file or missing field → omit the corresponding `omp -p` flag.

Helpers live in `internal/config/agent_config.go` (new):

```go
type AgentConfig struct { Model, Thinking, UpdatedAt string }
func ReadAgentConfig(repoPath string) AgentConfig
func WriteAgentField(repoPath, field, value string) error
```

---

## 10. Makefile / install / packaging

- `setup`: drop `cd channel && bun install`. Keep tmux usage **only** for
  the dispatcher process itself (operator convenience; nothing to do with
  agent management). Make this explicit in a comment.
- `restart`: keep as "rebuild + bounce the dispatcher tmux session". Pure
  dev ergonomics; not part of the runtime.
- `self-modify`: delete. There is no channel plugin path to point at.
- `scripts/install.sh`: remove tmux from prereqs **for the agent path**
  (still optional for operator tmux). Add `omp` as a prereq.
- `package.json`: drop `trd-channel` bin, `channel/` from `files`, the
  `@modelcontextprotocol/sdk` dep.
- `go.mod`: `go mod tidy` after deleting the WS code; expect
  `github.com/coder/websocket` to vanish.

---

## 11. Verification strategy

The port is **not** done until each item below is reproducible.

| Check | How |
|-------|-----|
| Wire layer | `go test ./internal/agent` with a fake `omp` binary (shell script in `testdata/`) that emits a canned NDJSON stream; assert classify mapping. |
| First-message flow | Integration test: spawn dispatcher with `TRD_OMP_BIN=./testdata/fake-omp`, simulate Telegram update via in-process Telegram client stub, assert correct stdout was forwarded as Telegram `sendMessage`. |
| Resume flow | Same as above, run a second prompt, assert spawned argv contains `--resume <captured-id>`. |
| `/reset` | After `/reset`, the next spawn must NOT contain `--resume`. |
| `/cancel` | Spawn a fake `omp` that sleeps 60s, send `/cancel`, assert the process group received SIGINT and `runs` map cleared within 1s. |
| `/model` / `/effort` | Setting writes `.trd/agent.json`; next spawn argv contains the flag. |
| Queueing | Two messages arrive while one is in flight; second waits, then runs. Assert no second `omp` spawn until first exits. |
| HTTP API | `GET /api/instances` no longer returns `tmux_alive` / `connected`; returns `running` and `session_id`. |
| CLI | `trd status`, `trd watch`, `trd stop` work against the API for a live dispatcher and via direct bbolt when the dispatcher is down. |
| Cleanup | `internal/tmuxmgr`, `internal/ws`, `channel/`, `coder/websocket` all gone. `grep -R tmux internal/` returns only deployment docs. |
| Build | `go build ./...` and `go test ./...` clean. `go mod tidy` produces no diff. |

---

## 12. Execution phases (agent-friendly)

Each phase is **independently shippable** (the tree still builds, tests
still pass) **except phase 3**, which deliberately straddles a breaking
change and must land atomically. Phases 0, 1, 2, 4, 5, 6 are parallelisable
in pairs noted below.

### Phase 0 — Probe omp wire

**Scope**: one engineer, ~30 min, no code change.
1. Run `omp -p --mode json --cwd /tmp/x "hi"` against the installed omp.
2. Capture the first 50 lines of NDJSON to `porting/omp-sample.ndjson`.
3. Document the session-id field name (and any other relevant keys) at
   the top of `porting/PLAN.md` Appendix A (created on completion).
4. **Hard blocker for phases 3 and 4.** Without empirical event names,
   `agent.classify` is guesswork.

### Phase 1 — Storage + config groundwork (parallel with Phase 2)

**Files**: `internal/storage/storage.go`, `internal/storage/storage_test.go`,
`internal/config/config.go`, `internal/config/agent_config.go` (new),
`internal/config/config_test.go`.

**Changes**:
- Add `SessionID`, `Debug` to `Instance`. Add tests covering encode/decode
  round trip with zero values.
- Add `agent_config.go` with `AgentConfig`, `ReadAgentConfig`,
  `WriteAgentField`. Add tests.
- `EnsureGitignore`: remove `.mcp.json`, keep `.trd/`, `.omc/`.
- Drop `WriteRepoConfig` if grep confirms no consumer remains after Phase 3.

**Done when**: `go test ./internal/storage ./internal/config` green.

### Phase 2 — `internal/agent` package (parallel with Phase 1)

**Files**: `internal/agent/agent.go` (new), `internal/agent/agent_test.go`
(new), `testdata/fake-omp/main.go` (new, tiny Go program emitting canned
NDJSON; built into a binary by the test).

**Changes**:
- Implement `RunOptions`, `Event`, `Run`, `Start`, `parseLoop`, `classify`
  exactly per §4 using the field names from Phase 0.
- Unit test: feed a recorded stream to a stub Reader, assert event sequence.
- Unit test: end-to-end with `fake-omp` binary, assert `cmd.Wait()` returns
  cleanly and channel closes.
- Cancellation test: long-sleeping fake-omp, context cancel, assert SIGINT
  delivered to the process group.

**Done when**: `go test ./internal/agent` green. Package compiles standalone.

### Phase 3 — Dispatcher cutover (atomic, no parallel)

**Files**: `internal/dispatcher/dispatcher.go`,
`internal/dispatcher/dispatcher_test.go`, `internal/ws/*` (delete or rename
to `internal/api/`), `internal/tmuxmgr/*` (delete), `channel/*` (delete).

This is the **breaking** phase. The tree won't build mid-edit; bundle it.

**Order of operations inside the phase**:
1. `git rm -r internal/tmuxmgr channel` and the WS files listed in §2.
2. Create `internal/api/api.go` from the kept handlers of `internal/ws/ws.go`.
3. Rewrite `dispatcher.go`:
   - Strip all `tmuxmgr.*`, `ws.Frame`, `d.conns`, `d.pending`,
     `d.rateLimited`, `d.pendingDelegates`, `OnOutbound`, `Register`,
     `Unregister`, `AuthSecret`, `sendTo`, `launchTmux*`, `autoConfirm`,
     `resumeInstances`, `healthLoop`, `checkHealth`, `checkRateLimit`,
     `handleDelegate`, `writeMCPConfig`, `extractPaneSection`, `cmdManager`.
   - Add `runs`, `queue` maps and the `agentRun` struct.
   - Rewrite `routeToInstance` → `enqueueOrRun` → `driveAgentRun` as in §3.4.
   - Rewrite `cmdStart` to skip launch and just persist `Instance{State:
     stopped, SessionID: ""}`. (No clone behaviour change.)
   - Rewrite `cmdStop`, `cmdRestart`, `cmdReset`, `cmdWatch`, `cmdModel`,
     `cmdEffort` per §3.3.
   - Add `cmdCancel`.
4. Rewrite `dispatcher_test.go` against the new surface. Tests that
   poked the WS server are deleted; new tests stub `agent.Start` via an
   interface seam (`type runner interface { Start(...) (*agent.Run, error) }`)
   injected through `Options`.

**Done when**: `go build ./...` and `go test ./...` green.

### Phase 4 — CLI + HTTP API alignment (parallel with Phase 5)

**Files**: `cmd/trd/main.go`, `internal/api/api.go`.

- Add `POST /api/instances/{id}/cancel` endpoint backed by
  `d.cancelRun(instID)`.
- Update `instanceInfo` decoding to match the new `InstanceInfo`.
- Update `cmdStatus` formatting (`session=`, `running=`).
- Replace `cmdWatch` body with log-file read.
- Replace `cmdStop` body with the cancel API call.
- Update `usage` text in `main.go`.

**Done when**: `trd status`, `trd watch`, `trd stop` work end-to-end against
the new dispatcher.

### Phase 5 — Packaging cleanup (parallel with Phase 4)

**Files**: `Makefile`, `package.json`, `scripts/install.sh`,
`postinstall.js`, `go.mod`, `go.sum`.

- Remove `self-modify` target, `cd channel && bun install`, `@modelcontextprotocol/sdk`,
  `trd-channel` bin, `channel/` from `files`.
- Add `omp` prereq to `scripts/install.sh`.
- `go mod tidy` to drop `github.com/coder/websocket`.

**Done when**: `make build` produces a binary; `bun install` (if anyone
runs it) is a no-op for channel; `go.mod` clean.

### Phase 6 — Docs (final)

**Files**: `README.md`, `DEV.md`, `CLAUDE.md`, `ARCHITECTURE.md`,
`porting/REWRITE.md` (mark as historical), `porting/tmux.md` (mark as
historical/superseded), `porting/BRIDGE.md` (keep as engineering note).

- Update architecture diagram, command table, voice section.
- Drop tmux references except in operator-deployment notes.
- Document `omp` prereq, `TRD_OMP_BIN` env var, `.trd/agent.json`.

---

## 13. Out-of-scope (explicit)

- Streaming Telegram edits during a run (Phase 3 ships a single final reply).
- Delegation / manager mode (deferred to a follow-up HTTP endpoint).
- Outbound TTS (`send_voice`).
- Multi-tenant per-chat resource limits.
- Cross-host runs (the agent always spawns on the dispatcher host).
- Migrating in-flight production state. The bbolt schema is backward-compatible.

---

## 14. Risk register

| Risk | Mitigation |
|------|-----------|
| omp `--mode json` event schema differs from our assumption | Phase 0 captures real output; `classify` is a single function. |
| Long-running tools (multi-minute) block per-instance queue | Acceptable for v1. Per-topic concurrency is the user's mental model. Document. |
| Telegram 4096-char reply limit on long outputs | Existing `splitMessage` handles this. |
| `omp` not on `$PATH` | Detect on dispatcher start; refuse to launch with a clear error. Add `TRD_OMP_BIN` env override. |
| Race between user `/cancel` and natural omp exit | `driveAgentRun` defers `finishRun` which is the single source of truth; cancel is idempotent. |
| Lost session id capture (omp crashes before emitting it) | First-run sessions stay `SessionID: ""`. Next user message starts a *new* session. Document and accept. |
| Concurrent inbound messages to a topic | `enqueueOrRun` serialises per-instance. |

---

## Appendix A — omp NDJSON sample

Recorded with `omp v15.3.0` running:
`cd /tmp/omp-probe && omp -p --mode json --no-skills --no-extensions "say hi in 5 words"`

A full 14-line capture lives in `porting/omp-sample.ndjson`. Below is the
distilled event taxonomy the `agent.classify` function targets.

### Per-line event types observed

| `type` | When emitted | Fields used by the bridge |
|--------|--------------|---------------------------|
| `session` | First line of every run | `id` (session UUID), `cwd`, `timestamp`, optional `title` |
| `agent_start` | Once, after `session` | — |
| `turn_start` | Once per turn | — |
| `message_start` | Per message in the turn (user + assistant) | `message.role`, `message.content[].text` |
| `message_update` | Streaming text chunks | `assistantMessageEvent.type` ∈ {`text_start`, `text_delta`, `text_end`}; `assistantMessageEvent.delta` (incremental); `assistantMessageEvent.content` (cumulative on `text_end`) |
| `message_end` | Per finalized message | `message.role`, `message.content[].text`, `message.stopReason`, `message.errorMessage` (on API errors) |
| `turn_end` | Once per turn | `message` (final assistant), `toolResults` |
| `agent_end` | Final line of the run | `messages[]` (whole conversation) |
| `auto_retry_start` | Soft API failure → omp retrying | `attempt`, `maxAttempts`, `delayMs`, `errorMessage`. Surface as a status. |
| `model_change`, `thinking_level_change` | Mid-session config events | not consumed by initial port |

### Bridge mapping (`agent.classify`)

| Source NDJSON | Emitted `agent.Event` |
|---------------|------------------------|
| `type:"session"` | `EvSessionID` with `SessionID = id` |
| `type:"message_update"` with `assistantMessageEvent.type == "text_delta"` | `EvAssistantDelta` with `Text = delta` |
| `type:"message_end"` with `message.role == "assistant"` and `message.stopReason == "error"` | `EvError` with `Text = message.errorMessage` |
| `type:"message_end"` with `message.role == "assistant"` and no error | `EvAssistantFinal` with `Text = concat(message.content[].text where type=="text")` |
| `type:"agent_end"` | `EvDone` |
| anything else | swallowed (logged at debug) |

### Resume semantics

`omp -p --resume <id>` matches the session id (full UUID or unambiguous
prefix) **within the cwd's session directory**
(`~/.omp/agent/sessions/<cwd-mangled>/`). Cross-cwd resume is unsupported.
The dispatcher MUST always invoke omp with `cmd.Dir = I.RepoPath`.

### Known sharp edge

omp writes session files atomically: while writing, the file is named
`.<timestamp>_<id>.jsonl.<rand>.tmp`. On a clean dispose the leading `.`
and trailing `.tmp` are removed. If omp is killed mid-stream (or exits
during an auto-retry pause), the `.tmp` file remains and **the session is
not resumable** by id. The dispatcher tolerates this: a failed resume just
means the next user message starts a new session. Captured by the risk
register.