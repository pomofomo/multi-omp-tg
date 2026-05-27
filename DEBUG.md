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
| Reply has `(agent reported: …)` appended | A pre-final `EvError` from `omp`. Check `~/.trd/logs/<instance-id>.log`. (Post-final EvErrors are suppressed — see § 1.) |
| `EPIPE` in the per-instance log | `omp` lost its stdout pipe mid-write. Almost always because `trd` was killed (SIGINT in tmux, restart, crash) or the run completed and `omp`'s final `agent_end` frame raced the pipe close. Harmless when a clean reply already shipped. |
| `agent: bad json line:` warnings in `trd.log` | `omp` emitted partial NDJSON (same root cause as EPIPE). When tagged `post_final, suppressed from reply` it is informational only. |
| 👀 lands but no 👍 follows | Dispatcher routed the message but the LLM never called `tg_react("👍")`. Most likely cause: an old `omp` invocation that predates the extension wiring (check `ps` for `--extension` on the omp argv). |
| Neither 👀 nor 👍 | Dispatcher couldn't reach Telegram's `setMessageReaction`. `trd start --debug` and look for `"reaction failed"`. |
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

**Fix.** In `internal/dispatcher/dispatcher.go`'s `driveAgentRun`, an
`EvError` arriving *after* `sawFinal=true` is logged at WARN with a
`(post_final, suppressed from reply)` marker but does **not** flip
`hadError`. The run already succeeded; do not pollute the reply with a
post-success diagnostic.

Errors that arrive *before* the final message still flip `hadError`
normally — pre-final failures are real failures and the user needs to see
them. `TestErrorBeforeFinalStillSurfaces` guards that path; the new
`TestPostFinalErrorDoesNotPolluteReply` guards the suppression.

The closely related visibility-mark question (👀 vs 👍, who fires what)
is handled separately under § 2 below.

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

**Fix — two-mark visibility chain.** Acknowledgement is split across the
two layers that have the relevant information, so the sender can see
exactly where the request is in the pipeline:

| Mark | Set by | When | Meaning |
|------|--------|------|---------|
| 👀 | Dispatcher in `enqueueOrRun` (deterministic) | Within ~ms of routing the Telegram update, before `omp -p` is spawned | "system received it" |
| 👍 | LLM via the `tg_react` tool, per the `SystemPromptAppend` ACKNOWLEDGE pattern | First action of the turn, before any other tool calls or text | "model has seen it" |

The 👀 fires for every prompt — idle path and queue-behind-busy path
both. The 👍 fires from the agent itself via the in-process omp extension
(`internal/agent/extension/tg.ts`), which POSTs `/api/tg/react` on the
dispatcher's localhost control plane. The bot token never leaves the
dispatcher process.

If 👀 appears but 👍 never does, the dispatcher routed the message but
the agent failed to act on it — useful diagnostic. The LLM MAY also
upgrade the reaction later in the turn (🎉 / 😅 / ❌) to reflect outcome.

**Lesson.** When you remove a layer, walk every promise that layer made to
the user. Some are visible enough to migrate explicitly; the rest become
silent regressions. Where the visibility itself is the user's signal of
progress, split the mark across whichever layers can actually observe
each stage — one mark per stage beats one mark from one layer.

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

## Self-restart (implemented)

The three proposals below were merged into the live dispatcher. This
section is the operational cheat-sheet for what's wired today;
§ "Design rationale" preserves the original reasoning.

### How to restart trd in production

| You want… | Run |
|---|---|
| Bounce after a code change (operator-driven) | `make restart` — uses `systemctl --user restart trd` when the unit is active, otherwise falls back to the legacy tmux path. |
| Bounce from inside a Telegram turn (agent-driven, lossless) | `/restart-self` in the controller topic, or have the agent call its `trd_restart` tool. The dispatcher drains in-flight runs (the current reply still lands), persists queued prompts to bbolt, then `syscall.Exec`s in place — same PID, supervisor doesn't observe a crash. |
| Authorise a topic to issue restarts | `trd promote <repo-name>` on the host. Exactly one controller at a time; `trd demote` clears the flag. |
| Survive crash / OOM / host reboot | `make install-systemd` (one-time). `Restart=always` brings the dispatcher back in ~2s. |

The two paths compose: in-place exec keeps `MainPID` stable so systemd
stays satisfied; if exec itself fails (post-`Close()`, pre-`Exec()`) the
supervisor still respawns us.

### What state survives an in-place exec

| State | Mechanism |
|---|---|
| `runs` map | Drained before exec — never in flight across the boundary. |
| `pendingQueue` (in-memory FIFOs) | Flushed to bbolt bucket `deferred_prompts` at `RequestRestart`; successor's `redeliverDeferredPrompts` re-routes through `enqueueOrRun`. |
| Prompts that arrive *during* the drain | `enqueueOrRun` short-circuits to `deferred_prompts` while `pendingRestart` is set. |
| `Instance.SessionID` | Already in bbolt — successor reads, passes `--resume`. Conversation continuity is preserved across the gap. |
| Telegram long-poll offset | Persisted to `settings.last_update_id` after every non-empty batch; successor seeds `offset` from it. No redelivery, no skipped updates. |

### Authorisation in one line

`POST /api/restart` requires header `X-Trd-Instance: <id>` matching an
instance with `Controller=true`. The in-process omp extension sets the
header from `TRD_INSTANCE_ID` (injected on every spawn). Non-controller
→ 403. `/restart-self` in Telegram uses the same predicate on the
topic's bound instance.

### Acceptable degradations

- The originating turn's reply lands in the *outgoing* process; the
  "restart complete" confirmation arrives in a *fresh* turn from the
  successor. The user sees one extra round-trip, not a silent gap.
- A prompt that arrives during the drain shows 👀 immediately (sent by
  the outgoing process before exec) and 👍 only after the successor
  picks it up. The two-mark visibility chain stays honest.

### Design rationale (kept for future readers)

The original §"The restart-self problem" walk-through and the Proposals
A/B/C lived here while the design was being argued. Their gist:

- The naive `tmux send-keys C-c` path is fatal when an omp agent
  triggers it from inside a turn — the act of asking for a restart
  kills the process responsible for delivering the reply.
- `syscall.Exec` after a clean drain (Proposal A) gives lossless
  restart-from-within-a-turn.
- A systemd `--user` unit (Proposal B) is the safety net for everything
  else: crashes, OOMs, host reboots, exec-failed-mid-restart.
- A single `Controller bool` per instance (Proposal C) is the minimum
  auth gate that doesn't require sharing secrets or a new transport.

The full original write-up (with sequence diagrams, the "What this
still doesn't fix" caveats, the migration order) is in git history
pre-implementation if you ever need to re-derive a decision.

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
