# Debugging & Operations

Field notes for keeping `trd` healthy in production, and proposals for making
the deploy loop survive `omp` editing `trd` itself.

This is a working doc — when you fix something non-trivial, add what you
learned at the bottom of [§ Case studies](#case-studies) so the next person
doesn't have to re-derive it.

## TL;DR

| You see… | Look at… |
|---|---|
| Bot is silent | `tmux attach -t trd` — is the dispatcher even running? |
| Reply has `(agent reported: …)` appended | An `EvError` arrived from `omp`. Check `~/.trd/logs/<instance-id>.log`. |
| `EPIPE` in the per-instance log | `omp` lost its stdout pipe mid-write. Almost always because `trd` was killed (SIGINT in tmux, restart, crash). |
| `agent: bad json line:` warnings in `trd.log` | `omp` emitted partial NDJSON (same root cause as EPIPE). Now suppressed when a final reply was already received. |
| No 👍 reaction after sending a message | The dispatcher couldn't reach Telegram's `setMessageReaction` endpoint. `trd start --debug` and look for `"reaction failed"`. |
| Run hangs forever | `RunTimeout` is 15 min. After that the context kills `omp`. To kill sooner: `/cancel` in the topic, or `POST /api/instances/{id}/cancel`. |
| Session won't resume | A killed run can leave a `.tmp` session file in `~/.omp/agent/sessions/`. `/reset` in the topic clears `Instance.SessionID`; next message starts fresh. |

## Log sources

Three places, in order of usefulness for most bugs:

1. **`~/.trd/trd.log`** — dispatcher's structured log. Every Telegram update,
   every routing decision, every agent error event. This is the first thing
   you read.
2. **`~/.trd/logs/<instance-id>.log`** — the `omp` subprocess's stderr for
   that instance. Crashes, EPIPE, Node uncaught exceptions land here.
3. **`~/.omp/agent/sessions/<cwd-mangled>/<ts>_<session-id>.jsonl`** — the
   full conversation as `omp` recorded it. Useful when you need to know what
   the model actually saw vs. what we *think* we sent.

Quick aliases:

```bash
tail -F ~/.trd/trd.log                            # live dispatcher
tail -F ~/.trd/logs/*.log                         # all instance stderrs
ls -lt ~/.omp/agent/sessions/                     # newest sessions first
trd watch <instance-name>                         # cli tail of (2)
```

Toggle verbose: `/debug` in a topic, or start with `TRD_DEBUG=1`.

## Case studies

### 1. EPIPE on `agent_end` polluting clean replies (2026-05-27)

**Symptom.** Replies from `omp` arrived correctly but came with a trailing
`(agent reported: agent: bad json line: {"type":"agent_end",…)` annotation.
Per-instance log showed two stack traces:

```
[Uncaught Exception] Error: EPIPE: broken pipe, write
    at write (unknown)
    at writeFast (internal:fs/streams:345:38)
    at <anonymous> (…/pi-coding-agent/src/modes/print-mode.ts:58:19)
```

**Root cause.** `omp`'s last NDJSON line is `agent_end`, which carries the
entire conversation `messages` array — easily multiple megabytes on a long
run. Node's stdout flush at process exit races with `trd` shutting down its
read-end of the pipe (SIGINT in tmux, parent exit, etc.). The result on the
`trd` side: `bufio.Scanner` returns a truncated, unterminated JSON token.
`classify` fails the top-level `json.Unmarshal` and emits `EvError`. The
dispatcher had already received `EvAssistantFinal` and built a clean reply
from `message_end`, but the post-final error flipped `hadError`, and the
trailing branch in `driveAgentRun` appended `(agent reported: …)`.

**Fix.** Two-part, both in `internal/dispatcher/dispatcher.go`:

1. `driveAgentRun`: `EvError` arriving *after* `sawFinal=true` is logged
   (`post_final=true`) but does **not** flip `hadError`. The run already
   succeeded; do not pollute the reply with a post-success diagnostic.
2. `routeToInstance`: send `👍` via `sendReaction` *before* `buildPrompt`,
   so the user sees acknowledgement even when transcription/download is
   slow. Queued messages still get `👀` (overrides `👍`) from `enqueueOrRun`.

**Lessons.**

- **Subprocess stderr ≠ subprocess error.** A non-empty `<instance>.log`
  doesn't mean the run failed. EPIPE on the way out is the most common
  false-positive.
- **Late events from the agent stream are suspect.** Anything after the
  canonical `message_end` is process-teardown noise — handle it
  defensively at the dispatcher, not at the parser.
- **Don't reach for "send the error to the user" reflexively.** If we
  already have a final reply, the user wants the reply. Internal-only logs
  are still available via `/watch`.

### 2. Headless port lost the inbound-message ack (2026-05-27)

**Symptom.** User reported "I don't see any thumbs up reply on these
messages" — receipts that used to land within a second of sending now never
appeared.

**Root cause.** In the pre-port architecture (commit `bd7e38a`), a long-lived
Claude session ran behind an MCP channel plugin that exposed a `react` tool,
and a system-prompt instruction told the agent to call it on every inbound
message. The headless port removed the MCP plugin (deliberately — see
`ARCHITECTURE.md` "Lost vs gained") but did not move the acknowledgement
into the dispatcher.

**Fix.** Acknowledge in the dispatcher (`routeToInstance`). The dispatcher
already owns every outbound Telegram call in the headless model; the agent
doesn't need to know about Telegram at all.

**Lesson.** When you remove a layer, walk every promise that layer made to
the user. Some are visible enough to migrate explicitly; the rest become
silent regressions.

## The restart-self problem

### Today's `make restart`

```
tmux send-keys -t trd C-c       # SIGINT to whatever runs in the tmux window
sleep 1
tmux send-keys -t trd 'trd start' Enter
```

This works when *you* run it from your shell. It is **fatal** when an `omp`
agent — running as a child of the dispatcher you are about to kill — runs
it from a Telegram-driven turn:

```
human  → tg msg "rebuild and restart"
trd    → spawns `omp -p` (child)
omp    → Bash tool: `make restart`
make   → tmux send-keys -t trd C-c
kernel → SIGINT to trd (foreground in that tmux window)
trd    → begins shutdown, drains in-flight runs (5s grace)
omp    →   (still alive: it's in its own pgroup via Setpgid)
omp    → writes to stdout to report progress
kernel → EPIPE (trd already closed the read end)
omp    → uncaught exception, process dies
trd    → exits
tmux   → fresh `trd start`
NEW trd: no record of which message was in flight, no reply gets sent,
         human waits forever.
```

The reply the agent was about to produce is lost because the very act of
asking for a restart kills the process responsible for delivering the
reply.

### Proposal A — graceful in-process self-restart (recommended)

Add a self-restart path that **the dispatcher** drives, so the calling
`omp` is allowed to finish its turn before we re-exec.

**Wire.**

```
POST /api/restart                      → 202 Accepted, "restarting after current runs drain"
                                         403 if caller isn't authorized (see Proposal C)
```

Or a Telegram command in the controller topic:

```
/restart-self                          → equivalent to POSTing the above
```

**Semantics inside the dispatcher.**

1. Set a `pendingRestart` flag under `runMu`. Reject new prompts that try to
   spawn agents from this point on (queue them in bbolt instead, so the
   successor picks them up — see "State that must survive" below).
2. Let every in-flight run drain naturally. `driveAgentRun` finishes,
   sends its Telegram reply, calls `finishRun`. The omp that triggered
   the restart is one of those runs — it gets to deliver its message.
3. When `len(d.runs) == 0`, the dispatcher:
   - Closes the Telegram long-poll cleanly so we don't lose any updates.
   - `Sync()` and `Close()` bbolt.
   - Closes the HTTP listener.
   - Calls `syscall.Exec(os.Args[0], os.Args, os.Environ())`.

   `syscall.Exec` replaces the current process image *in place*. PID is
   preserved; tmux/systemd don't observe a crash; no respawn loop needed.

4. The successor starts, opens bbolt, sees its own queue of "deferred"
   prompts (from step 1) and dispatches them.

**State that must survive an in-place exec.**

| Today | After |
|---|---|
| `runs` map (in memory) | Drained before exec; nothing to persist. |
| `pendingQueue` map (in memory) | Must be persisted to a new `deferred_prompts` bbolt bucket so the successor picks them up. |
| `Instance.SessionID` | Already in bbolt. Successor reads, passes `--resume`. Mid-flight conversation continuity is preserved. |
| Long-poll `offset` | Currently in memory. Persist last-acked `update_id` to settings so we don't redeliver. |

Last-acked `update_id` is the only new piece of state Telegram requires.
Without it, the successor either replays the last batch (annoying for the
user) or skips updates that arrived during the exec gap (worse).

**Acceptable degradations.**

- The originating turn's reply gets sent, but the user's confirmation
  ("restart complete") arrives in a *new* run, kicked off by the
  successor against the same topic. That's fine.
- If something explodes between `Close()` and `Exec()`, the supervisor
  (Proposal B) restarts us anyway — at worst we re-execute the same
  startup path.

### Proposal B — external supervisor (systemd `--user` unit)

`tmux send-keys` is fine as a development affordance and useless as a
production supervisor — it doesn't restart on crash, doesn't log rotate,
doesn't sandbox.

A user-mode systemd unit takes over both jobs. Once installed, `make
restart` becomes `systemctl --user restart trd`, and any unexpected exit
brings the dispatcher right back.

**`~/.config/systemd/user/trd.service`** (sketch):

```ini
[Unit]
Description=Telegram Repo Dispatcher
After=network-online.target

[Service]
Type=exec
ExecStart=%h/.local/bin/trd start
Restart=always
RestartSec=2
# Keep stdio so `journalctl --user -u trd -f` works.
StandardOutput=journal
StandardError=journal
# Don't kill omp children when the unit is stopped — they're in their own
# pgroups and exit cleanly when their pipes close.
KillMode=mixed
TimeoutStopSec=10

[Install]
WantedBy=default.target
```

Enable per-user lingering so the unit survives logout:

```bash
loginctl enable-linger "$USER"
systemctl --user daemon-reload
systemctl --user enable --now trd
```

**Interaction with Proposal A.** If `trd` re-execs in place (Proposal A),
systemd does not observe the restart — same PID, `MainPID` stays valid.
If something fails and `trd` *exits*, `Restart=always` brings it back in
~2s. Either path keeps the dispatcher live; you choose between
"transparent" (re-exec) and "supervised" (exit+respawn) per situation.

**`make restart` becomes:**

```makefile
restart: install
	@if systemctl --user is-active --quiet trd; then \
	    systemctl --user restart trd; \
	else \
	    echo "trd unit not active — falling back to tmux"; \
	    tmux send-keys -t trd C-c 2>/dev/null || true; sleep 1; \
	    tmux send-keys -t trd '$(TRD_BIN) start' Enter; \
	fi
```

(Both supervisors supported during the transition.)

**What this still doesn't fix.** A `make restart` triggered by `omp`
inside a turn *still* kills the parent. systemd just brings it back
faster, but the in-flight reply is still lost. Proposal A is the actual
fix for that path; Proposal B is the safety net for everything else
(panics, crashes, OOMs, host reboots).

### Proposal C — controller-instance flag

`POST /api/restart` and `/restart-self` are dangerous — any agent that can
shell out to `curl` could trigger them, and the HTTP API binds to
`127.0.0.1` which means any process on the host has access.

Authorize them by flagging one instance as the controller:

```go
type Instance struct {
    // … existing fields …
    Controller bool `json:"controller,omitempty"`
}
```

- New CLI: `trd promote <name>` and `trd demote <name>` flip the flag.
  Optionally: enforce at most one controller across the store.
- `/restart-self` in Telegram: rejected unless the topic is bound to the
  controller instance.
- `POST /api/restart`: requires `X-Trd-Controller: <instance-id>`
  matching the controller instance. The header is the agent's way of
  proving it knows which instance it is — `omp` can read it from a
  per-repo file (e.g. `.trd/controller-id`) that the dispatcher writes on
  promotion.

This is the minimum auth that doesn't require sharing secrets or
deploying a new transport. It treats "is this the trd-self-development
topic" as a single boolean in the store, which is exactly how the user
already thinks about it.

### Migration order (when someone implements this)

1. Add the bbolt buckets (`deferred_prompts`, `last_update_id`) and the
   long-poll offset persistence. This is risk-free and useful on its own.
2. Add the `Controller` flag and the `promote`/`demote` CLI. Still inert.
3. Add `POST /api/restart` that drains runs and calls `syscall.Exec`,
   gated on the controller flag.
4. Add `/restart-self` Telegram command, gated on the same flag.
5. Ship the systemd unit and update `make restart` to prefer it.
6. Mark `make restart`-via-tmux as deprecated for production use.

Steps 1–4 are independent of 5–6; both halves are useful in isolation.

## Operational checklist (current state)

When something looks wrong, in order:

1. `tmux capture-pane -t trd -p -S -200` — is the dispatcher running and
   what was it doing recently?
2. `tail -100 ~/.trd/trd.log` — structured view of the same window.
3. `ls -lt ~/.trd/logs/` — which instance's stderr was last touched?
4. `tail -200 ~/.trd/logs/<id>.log` — what did `omp` last say to stderr?
5. `ls -lt ~/.omp/agent/sessions/<repo>/` — did `omp` write a session file
   for the last turn? If not, it died before producing one.
6. `curl -s localhost:7777/api/instances | jq .` — runtime view: which
   instances exist, which are `running: true`.

If `trd` itself is unresponsive but the process is alive, send it `SIGQUIT`
(`kill -QUIT $(pgrep -x trd)`) to dump goroutines to stderr —
fastest way to see a deadlocked `runMu` or a stuck Telegram poll.
