# tmux → headless migration analysis (HISTORICAL)

> **Status: superseded.** This file enumerated tmux call sites and
> proposed migrating to an `agentmgr` package that still managed a
> persistent agent process per instance. The final port skips that
> intermediate step entirely: every Telegram message spawns a one-shot
> `omp -p` subprocess, and `internal/tmuxmgr` is deleted. Kept for
> reference only.


Every tmux call in TRD, what it provides, and whether `omp -p` subprocess management can replace it.

## All tmux usage (5 primitives, 8 call sites)

### Primitives (`internal/tmuxmgr/tmuxmgr.go`)

| Primitive | tmux command | What it does |
|-----------|-------------|--------------|
| `HasSession` | `tmux has-session -t <name>` | Liveness check |
| `NewSession` | `tmux new-session -d -s <name> -c <cwd> -e KEY=VAL cmd` | Launch detached process in a workdir with env vars |
| `KillSession` | `tmux kill-session -t <name>` | SIGTERM the process tree |
| `CapturePane` | `tmux capture-pane -p -t <name>` | Read the terminal screen buffer as text |
| `SendKeys` | `tmux send-keys -t <name> keys...` | Type keystrokes into the process's stdin/ptty |

### Call sites in dispatcher

| Method | Primitives used | Purpose |
|--------|----------------|---------|
| `launchTmuxWithOpts` | `HasSession`, `NewSession` | Start the agent process |
| `autoConfirm` | `HasSession`, `CapturePane`, `SendKeys` | Detect Claude's "enter to confirm" prompt and dismiss it |
| `checkRateLimit` | `CapturePane`, `SendKeys` | Detect Claude's rate-limit prompt, dismiss it, notify user |
| `cmdModel` | `HasSession`, `SendKeys`, `CapturePane` | Type `/model` into Claude's TUI and capture the response |
| `cmdEffort` | `HasSession`, `SendKeys`, `CapturePane` | Type `/effort` into Claude's TUI and capture the response |
| `cmdWatch` | `CapturePane` | Show user what Claude's terminal looks like |
| `cmdStatus` | `HasSession` | Report whether the tmux session is alive |
| `cmdStop` | `KillSession` | Stop the agent |
| `cmdRestart`/`cmdReset` | `KillSession` + `NewSession` | Kill and relaunch |
| `resumeInstances` | `HasSession` + `NewSession` | Recover dead sessions on dispatcher restart |
| `healthLoop` | `HasSession` + `NewSession` | Detect dead sessions, relaunch, reset fail counters |

### Call sites in CLI

| Command | Primitives used |
|---------|----------------|
| `cmdStatus` | `HasSession` |
| `cmdStop` | `KillSession` |
| `cmdWatch` | `CapturePane` |

---

## What tmux provides vs. headless subprocess

### 1. Terminal screen buffer (`CapturePane`)

**What it is:** tmux maintains an in-memory scrollback buffer of the pseudo-terminal it allocated for the process. `capture-pane` reads the *visible rendered text* — ANSI escapes stripped, wrapped at the terminal width.

**Why TRD depends on it:**

| Call site | What it scrapes from the pane |
|-----------|------------------------------|
| `autoConfirm` | String match: "enter to confirm", "local development", "y/n" |
| `checkRateLimit` | String match: "rate-limit-options", "hit your limit", "Waiting for...limit" |
| `cmdModel` | Claude's model selection UI rendered in the TUI |
| `cmdEffort` | Claude's effort selection UI rendered in the TUI |
| `cmdWatch` | The full visible pane for user debugging |

**Headless replacement:** In headless mode the agent has no TUI — it communicates through its protocol and stdout/stderr.

- `autoConfirm`: Agent startup is non-interactive. Pass `--non-interactive` or equivalent at launch. **Deleted.**
- `checkRateLimit`: Rate-limit status arrives as structured data from the agent (via the WS channel). **Replaced with `status` frame handler.**
- `cmdModel`/`cmdEffort`: Model and effort are written to a config file the agent watches, or sent as a WS control frame. **Replaced with config-file hot-reload.**
- `cmdWatch`: Agent stdout/stderr are written to a log file. `/watch` tails it. **Replaced with log-file reads.**

**Verdict:** Every `CapturePane` call exists solely because Claude is a TUI application. In headless mode, all five call sites are either **deleted** (autoConfirm) **replaced with structured alternatives** (cmdModel → config file, cmdEffort → config file, checkRateLimit → WS status frame), or **replaced with log-file reads** (cmdWatch).

### 2. Keystroke injection (`SendKeys`)

**What it is:** Writes bytes into the pseudo-terminal's master side, as if the user typed them.

**Why TRD depends on it:**

| Call site | What it types | Why |
|-----------|--------------|-----|
| `autoConfirm` | `Enter` | Dismiss Claude's interactive prompt |
| `checkRateLimit` | `Enter` | Dismiss Claude's rate-limit menu |
| `cmdModel` | `/model sonnet`, `Enter` | Change Claude's model via TUI |
| `cmdEffort` | `/effort high`, `Enter` | Change Claude's effort via TUI |

**Headless replacement:** None of this is needed. In headless mode:

- Confirmation prompts and rate-limit status come through the agent's protocol, not the terminal.
- Model/effort changes happen through the agent's API or config, not by typing into a TUI.

**Verdict:** All `SendKeys` calls are **deleted** in a headless port.

### 3. Process lifecycle (`NewSession`, `KillSession`, `HasSession`)

**What tmux does:**

- `new-session -d`: Forks a process in a new session (detached from the controlling terminal). The process survives even if the parent (dispatcher) dies. tmux reaps it.
- `kill-session`: Sends SIGHUP then SIGTERM to the process group. Cleans up the pty.
- `has-session`: Checks if the tmux session still exists (process alive or zombie).

**What `omp -p` subprocess gives us:**

- The Go dispatcher owns the `exec.Cmd`. It gets `cmd.Process.Pid`, `cmd.Wait()`, and `cmd.Process.Signal()`.
- Catching SIGTERM in the dispatcher lets us forward it to child processes.

**The key difference: detached survival.** With tmux, if the dispatcher crashes, agent processes keep running. Channel plugins reconnect when the dispatcher comes back. With `exec.Cmd`, if the dispatcher dies, the kernel sends SIGHUP to children (unless they're in their own process group).

This is fixable with `syscall.Setsid` / `cmd.SysProcAttr.Setpgid = true` on the child process — the child becomes a session leader, detached from the dispatcher's process group. The dispatcher then manages PIDs explicitly rather than tmux session names.

**Headless replacement:**

| tmux primitive | Headless equivalent |
|---------------|-------------------|
| `NewSession(name, cwd, cmd, env)` | `exec.Command(cmd[0], cmd[1:]...)` with `Dir: cwd`, `Env: env`, `SysProcAttr: {Setpgid: true}`. Start async, store PID keyed by instance ID. |
| `KillSession(name)` | `syscall.Kill(-pgid, syscall.SIGTERM)` on the process group. |
| `HasSession(name)` | `process.Signal(syscall.Signal(0))` — signal 0 is the null signal, returns error if the process is dead. Or check a PID file. |

**Verdict:** Process lifecycle is **trivially replaceable**. The only detail to get right is detaching the child so it survives dispatcher restarts.

### 4. Named session registry

tmux gives us a global namespace: `trd-<instance-id>`. We ask tmux "is this name alive?" and tmux answers.

**Headless replacement:** A PID file at `~/.trd/pids/<instance-id>.pid`. On dispatcher start, `resumeInstances` reads PID files, checks liveness with signal 0, and either reconnects or restarts.

---

## Summary matrix

| tmux feature | Can headless replace it? | Mechanism |
|-------------|------------------------|-----------|
| Process launch | Yes, trivially | `exec.Cmd` + `Setpgid` |
| Process kill | Yes, trivially | `signal(-pgid, SIGTERM)` |
| Liveness check | Yes, trivially | Signal 0 or PID file |
| Pane capture | **Replaced** | Screen-scraping replaced with config files (model/effort), WS status frames (rate-limit), and log files (watch) |
| Keystroke injection | **Deleted** | No TUI to type into — all interactions use structured protocols |
| Session naming | Yes, trivially | PID file or in-memory map |

## What we lose (and whether it matters)

### Lost: `/watch` command (tmux pane capture)

Users can currently type `/watch` in a topic to see Claude's terminal. In headless mode, there is no terminal — just stdin/stdout.

**Mitigation:** Log agent stdout/stderr to a file (`~/.trd/logs/<instance-id>.log`). `/watch` reads the last N lines of that log. This is arguably *better* — it's searchable, persistent, and doesn't depend on terminal dimensions.

### Replaced: `/model` and `/effort` commands

These currently type slash commands into Claude's TUI and screen-scrape the response. In headless mode, model and effort control must use a structured interface. Three strategies, in order of preference:

#### Strategy A: Config file hot-reload (best)

Write model/effort selection to a config file the agent watches. The agent reloads on change and applies the new setting on the next request.

```go
// cmdModel writes the model choice to the agent's config file.
func (d *Dispatcher) cmdModel(ctx context.Context, m *telegram.Message, arg string) {
    inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
    if inst == nil {
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
        return
    }
    cfgPath := filepath.Join(inst.RepoPath, ".trd", "agent.json")

    if arg == "" {
        // Read current setting.
        cfg, _ := readAgentConfig(cfgPath)
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
            fmt.Sprintf("Current model: %s\nOptions: /model sonnet, /model opus, /model haiku",
                cfg.Model))
        return
    }
    valid := map[string]bool{"sonnet": true, "opus": true, "haiku": true}
    if !valid[strings.ToLower(arg)] {
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
            "Unknown model. Options: sonnet, opus, haiku")
        return
    }
    // Write the change — agent hot-reloads on next poll.
    if err := updateAgentConfig(cfgPath, "model", strings.ToLower(arg)); err != nil {
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "failed to update config: "+err.Error())
        return
    }
    d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
        fmt.Sprintf("Model changed to %s. Takes effect on next request.", arg))
}

// cmdEffort is structurally identical but writes the "effort" field.
```

Agent config file at `<repo>/.trd/agent.json`:
```json
{
  "model": "sonnet",
  "effort": "high",
  "updated_at": "2026-05-26T12:00:00Z"
}
```

The OMP adapter (replacement for channel plugin) watches this file with `fs.watch` or polls `mtime` every 5s, and applies changes to the agent's next invocation.

**Pros:** No process restart needed. Survives dispatcher restarts. Changes are visible in the repo.
**Cons:** Requires the agent to support config-file reload. Polling adds ~5s latency to model switches.

#### Strategy B: Restart with env vars (simpler)

Pass model/effort as env vars on launch. Changing them requires a `/restart`.

```go
func (d *Dispatcher) launchAgent(inst storage.Instance) error {
    cfg, _ := readAgentConfig(filepath.Join(inst.RepoPath, ".trd", "agent.json"))
    env := []string{
        "TRD_CONFIG=" + cfgPath,
        "TRD_AGENT_MODEL=" + cfg.Model,
        "TRD_AGENT_EFFORT=" + cfg.Effort,
    }
    return agentmgr.Launch(inst, agentBin, agentArgs, env)
}
```

`/model sonnet` writes to `agent.json`, then the user gets a message: "Model set to sonnet. Use /restart to apply."

**Pros:** No live-reload mechanism needed. Simple.
**Cons:** User must `/restart` to apply. Disrupts in-progress work.

#### Strategy C: WS control frame (most responsive)

Extend the WS frame protocol with `config_update` frames. The dispatcher sends a frame to the adapter, which applies the change immediately.

```json
// Dispatcher → Adapter
{"type": "config_update", "key": "model", "value": "sonnet"}
```

Adapter handles it:
```go
// In dispatcher cmdModel:
d.sendTo(inst.InstanceID, ws.Frame{
    Type: "config_update",
    Path: "model",   // or a dedicated Key/Value pair on Frame
    Text: "sonnet",
})
```

The OMP adapter receives the frame and reconfigures the agent live.

**Pros:** Instant. No polling. No restart.
**Cons:** Requires a new frame type + adapter logic. State lost if agent restarts (mitigate by also writing to `agent.json`).

#### Recommendation

Implement **Strategy A (config file)** for the port. It's the simplest that works without restart, matches the existing pattern of `.trd/config.json`, and requires no WS protocol changes. If latency becomes an issue, add Strategy C as an optimization later.

---

### Replaced: Rate-limit detection and watchdog

Claude's rate limits are detected by screen-scraping its TUI for text like "rate-limit-options" and "hit your limit". In headless mode, rate limits manifest through the agent's API or protocol.

#### Pattern 1: Agent emits structured status (best)

The agent exposes its rate-limit status through a structured channel. The OMP adapter translates this into a WS frame the dispatcher already understands — or a new one.

Add a new WS frame type:

```json
// Adapter → Dispatcher
{"type": "status", "kind": "rate_limit", "limited": true, "resets_at": "2026-05-26T12:05:00Z"}
{"type": "status", "kind": "rate_limit", "limited": false}
```

Dispatcher handler (replaces `checkRateLimit`):
```go
// In OnOutbound, add case "status":
case "status":
    if frame.Kind == "rate_limit" {
        d.handleRateLimitStatus(instanceID, frame)
    }
```

```go
func (d *Dispatcher) handleRateLimitStatus(instanceID string, frame ws.Frame) {
    inst, _ := d.store.Get(instanceID)
    if inst == nil {
        return
    }
    d.rateLimitedMu.Lock()
    wasLimited := d.rateLimited[instanceID]
    if frame.Limited {
        d.rateLimited[instanceID] = true
    } else {
        d.rateLimited[instanceID] = false
    }
    d.rateLimitedMu.Unlock()

    if frame.Limited && !wasLimited {
        d.sendText(context.Background(), inst.ChatID, inst.TopicID,
            "⏳ Agent hit a rate limit. Resets at "+frame.ResetsAt+". Auto-waiting.")
    }
    if !frame.Limited && wasLimited {
        d.sendText(context.Background(), inst.ChatID, inst.TopicID,
            "✅ Rate limit cleared — agent is back.")
    }
}
```

**This replaces the 50-line `checkRateLimit` function entirely** — no tmux pane capture, no string matching, no `SendKeys("Enter")` to dismiss menus. The status comes through the WS channel as structured data.

#### Pattern 2: HTTP header inspection (fallback)

If the agent makes HTTP calls to an API that returns rate-limit headers, the adapter intercepts `429` responses or `X-RateLimit-Remaining: 0` headers and emits the same `status` frame.

```go
// In the OMP adapter, wrapping the agent's HTTP client:
resp, err := agentHTTPClient.Do(req)
if resp.StatusCode == 429 || resp.Header.Get("X-RateLimit-Remaining") == "0" {
    wsSend({type: "status", kind: "rate_limit", limited: true,
        resets_at: resp.Header.Get("X-RateLimit-Reset")})
}
```

#### Pattern 3: Process exit code (coarsest)

If the agent process exits with a specific code on rate-limit, `agentmgr` detects it in the background reaping goroutine and the dispatcher's health loop re-launches.

```go
go func() {
    err := cmd.Wait()
    if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 88 {
        // Agent-specific "rate limited, waiting" exit code.
        d.rateLimitedMu.Lock()
        d.rateLimited[instanceID] = true
        d.rateLimitedMu.Unlock()
        // health loop will retry with backoff.
    }
    cleanup(instanceID)
}()
```

#### Recommendation

Implement **Pattern 1 (structured status frame)**. It's the correct headless equivalent: structured data over the existing WS channel replacing unstructured TUI scraping.

---

### Deleted: Auto-confirm prompts

The `autoConfirm` function (~20 lines) polls the tmux pane for 30 seconds looking for Claude's "enter to confirm" / "y/n" prompt and sends Enter to dismiss it.

In headless mode, agent startup is non-interactive — there is no TUI prompt to dismiss. If the agent binary requires a confirmation flag, pass it at launch:

```go
// In launchAgent — equivalent of Claude's --dangerously-skip-permissions:
args := []string{"serve", "--accept-all-permissions", "--non-interactive"}
```

**No dispatcher code needed.** This is a launch-flag concern, not a runtime watchdog concern.

### Gained: no tmux dependency

## Implementation sketch

### agentmgr.go — replaces tmuxmgr

```go
package agentmgr

type Agent struct {
    InstanceID string
    cmd        *exec.Cmd
    pgid       int
    logFile    *os.File
}

func Launch(inst storage.Instance, bin string, args []string) (*Agent, error) {
    logPath := filepath.Join(logsDir, inst.InstanceID+".log")
    logFile, _ := os.Create(logPath)

    cmd := exec.Command(bin, args...)
    cmd.Dir = inst.RepoPath
    cmd.Env = append(os.Environ(),
        "TRD_CONFIG="+cfgPath,
        "TRD_INSTANCE_ID="+inst.InstanceID,
    )
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
    cmd.Stdout = logFile
    cmd.Stderr = logFile

    if err := cmd.Start(); err != nil {
        return nil, err
    }
    pgid, _ := syscall.Getpgid(cmd.Process.Pid)

    writePIDFile(inst.InstanceID, cmd.Process.Pid, pgid)

    // Background reaping with rate-limit exit-code detection.
    go func() {
        err := cmd.Wait()
        if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 88 {
            dispatcher.NotifyRateLimited(inst.InstanceID, true, "")
        }
        cleanup(inst.InstanceID)
    }()

    return &Agent{InstanceID: inst.InstanceID, cmd: cmd, pgid: pgid, logFile: logFile}, nil
}

func (a *Agent) Kill() error { return syscall.Kill(-a.pgid, syscall.SIGTERM) }

func (a *Agent) Alive() bool {
    return a.cmd.Process.Signal(syscall.Signal(0)) == nil
}
```

### Rate-limit handler — replaces checkRateLimit (50 lines)

```go
// Added to ws.Frame:
type Frame struct {
    // ... existing fields ...
    Kind    string `json:"kind,omitempty"`     // e.g. "rate_limit"
    Limited bool   `json:"limited,omitempty"`  // true = hit limit
    ResetsAt string `json:"resets_at,omitempty"` // ISO 8601
}

// In dispatcher.OnOutbound, added case for "status" frames:
case "status":
    if frame.Kind == "rate_limit" {
        d.handleRateLimitStatus(instanceID, frame)
    }

func (d *Dispatcher) handleRateLimitStatus(instanceID string, frame ws.Frame) {
    inst, _ := d.store.Get(instanceID)
    if inst == nil { return }

    d.rateLimitedMu.Lock()
    wasLimited := d.rateLimited[instanceID]
    if frame.Limited {
        d.rateLimited[instanceID] = true
    } else {
        d.rateLimited[instanceID] = false
    }
    d.rateLimitedMu.Unlock()

    if frame.Limited && !wasLimited {
        d.sendText(context.Background(), inst.ChatID, inst.TopicID,
            "⏳ Agent hit a rate limit. Resets at "+frame.ResetsAt+". Auto-waiting.")
    }
    if !frame.Limited && wasLimited {
        d.sendText(context.Background(), inst.ChatID, inst.TopicID,
            "✅ Rate limit cleared — agent is back.")
    }
}
```

### Agent config management — replaces cmdModel/cmdEffort (60 lines combined)

```go
// AgentConfig is written to <repo>/.trd/agent.json and watched by the adapter.
type AgentConfig struct {
    Model     string `json:"model"`      // "sonnet", "opus", "haiku"
    Effort    string `json:"effort"`     // "low", "medium", "high", "xhigh", "max", "auto"
    UpdatedAt string `json:"updated_at"` // ISO 8601
}

func readAgentConfig(repoPath string) (AgentConfig, error) { ... }
func writeAgentConfig(repoPath string, cfg AgentConfig) error { ... }
func updateAgentField(repoPath, field, value string) error { ... }

func (d *Dispatcher) cmdModel(ctx context.Context, m *telegram.Message, arg string) {
    inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
    if inst == nil {
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
        return
    }
    cfgPath := filepath.Join(inst.RepoPath, ".trd", "agent.json")
    if arg == "" {
        cfg, _ := readAgentConfig(cfgPath)
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
            fmt.Sprintf("Current model: %s\nOptions: /model sonnet, /model opus, /model haiku", cfg.Model))
        return
    }
    valid := map[string]bool{"sonnet": true, "opus": true, "haiku": true}
    if !valid[strings.ToLower(arg)] {
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Unknown model. Options: sonnet, opus, haiku")
        return
    }
    if err := updateAgentField(cfgPath, "model", strings.ToLower(arg)); err != nil {
        d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "failed to update config: "+err.Error())
        return
    }
    d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
        fmt.Sprintf("Model changed to %s. Takes effect on next request.", arg))
}

// cmdEffort is structurally identical, writing the "effort" field.
// Valid values: low, medium, high, xhigh, max, auto.
```

### Agent launch — replaces launchTmuxWithOpts

```go
func (d *Dispatcher) launchAgent(inst storage.Instance, resume bool) error {
    cfg, _ := readAgentConfig(filepath.Join(inst.RepoPath, ".trd", "agent.json"))

    bin := firstNonEmpty(os.Getenv("TRD_AGENT_BIN"), os.Getenv("TRD_CLAUDE_BIN"), "omp")
    args := []string{"-p", "serve"}
    if d.opts.Debug {
        args = append(args, "--debug")
    }
    if resume {
        // OMP equivalent of --session-id for resuming conversations.
        args = append(args, "--session-id", inst.InstanceID)
    }

    return agentmgr.Launch(inst, bin, args)
}
```

## Recommendation

**Replace tmux with direct subprocess management.** The only value tmux provides is TUI scraping, and in a headless OMP port, there is no TUI to scrape. Everything else — process lifecycle, liveness, naming — is simpler to do directly.

The migration is mechanical:

1. Write `internal/agentmgr/agentmgr.go` with the Launch/Kill/Alive primitives above.
2. Replace all `tmuxmgr.*` calls with `agentmgr.*` equivalents.
3. **Rewrite** `cmdModel` and `cmdEffort` to read/write `<repo>/.trd/agent.json` (config-file hot-reload).
4. **Delete** `autoConfirm` — agent startup is non-interactive in headless mode.
5. **Replace** `checkRateLimit` with a `status` frame handler in `OnOutbound` — the adapter pushes rate-limit state as structured JSON, no screen-scraping.
6. **Rewrite** `cmdWatch` to tail the agent's log file.
7. Add `Kind`, `Limited`, and `ResetsAt` fields to `ws.Frame` for the `status` frame type.
8. Remove tmux from the prerequisite check in `scripts/install.sh`.