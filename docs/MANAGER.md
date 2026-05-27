# Manager Session Design (HISTORICAL)

> **Status: not implemented in the headless-omp port.** This file
> describes a pre-port design where a `/manager`-promoted instance could
> delegate to other instances via dispatcher-mediated WS frames. The
> WebSocket bridge no longer exists; delegation will be re-added as
> `POST /api/delegate` if needed. The `Manager` field on `Instance`
> remains in the bbolt schema for forward compatibility but is unused.


A manager session is a Claude instance promoted with `/manager` that can orchestrate work across other TRD-managed repos. Instead of manually switching between topics, you talk to one "manager" topic and it delegates tasks to other instances, collects results, and reports progress.

## Decisions

| Question | Decision |
|----------|----------|
| Who can delegate? | Only `/manager`-promoted instances |
| Repo-less managers? | No — every manager must have a repo attached |
| Can managers /start new instances? | No — future work |
| Permission scoping (manager → subset)? | No — future work. Any manager can delegate to any instance |

## Architecture: WS bridge through dispatcher

Delegation flows through the dispatcher's existing WebSocket connections, not through Telegram. Telegram is used for user visibility only.

```
Manager Claude
  → calls delegate(instance, message) tool
  → channel plugin sends WS frame {type: "delegate", target: "backend", text: "..."}
  → dispatcher receives frame
  → dispatcher forwards as a message frame to target instance's WS connection
  → dispatcher posts task to target's Telegram topic (user visibility)
  → target Claude processes the message, replies
  → dispatcher captures the reply
  → dispatcher sends {type: "delegate_result", req_id: "...", text: "..."} back to manager
  → dispatcher posts reply to target's Telegram topic (user visibility)
  → manager Claude receives the result, synthesizes, reports to user
```

**Why WS, not Telegram round-trip:** Faster, richer data (no Telegram formatting limits), and the dispatcher already has live WS connections to all instances. Telegram posts are a side effect for user visibility, not the transport.

## Promoting a manager

```
/manager        — toggle manager mode for this topic's instance
```

When promoted:
- Dispatcher sets a `Manager` flag on the Instance in bbolt
- On next channel plugin connection, the dispatcher tells the plugin to expose extra tools
- The channel plugin adds `delegate`, `broadcast`, `poll_instance`, `list_instances` tools

When demoted (toggle off):
- Flag cleared, extra tools removed on next reconnect

## Tools

### `delegate`

Send a task to another instance and wait for the response.

```json
{
  "name": "delegate",
  "arguments": {
    "instance": "backend",
    "message": "Add rate limiting to the /api/users endpoint"
  }
}
```

- `instance`: repo name or instance ID prefix (same matching as CLI)
- `message`: the task to send
- Returns: the target instance's reply text
- Timeout: 5 minutes (configurable). Returns error on timeout or if target is offline.

Under the hood:
1. Channel plugin sends `delegate` frame to dispatcher
2. Dispatcher resolves instance name → instance_id
3. Dispatcher injects a message frame into the target's WS channel
4. Dispatcher also sends the task to the target's Telegram topic (prefixed "📋 [manager-name]:")
5. Dispatcher subscribes to the target's next outbound reply frame
6. When target replies, dispatcher captures the reply text
7. Dispatcher sends `delegate_result` frame back to manager
8. Dispatcher also posts the reply to the target's Telegram topic (normal)

### `broadcast`

Send the same message to multiple instances in parallel.

```json
{
  "name": "broadcast",
  "arguments": {
    "instances": ["backend", "frontend", "infra"],
    "message": "What's your current git branch and latest commit?"
  }
}
```

Returns: JSON map of instance name → response text.

Under the hood: parallel delegates with a shared timeout.

### `poll_instance`

Read recent activity from another instance without sending a message.

```json
{
  "name": "poll_instance",
  "arguments": {
    "instance": "backend",
    "last_n": 5
  }
}
```

Returns: last N message frames that passed through the dispatcher for that instance (both inbound and outbound). The dispatcher keeps a small ring buffer per instance.

### `list_instances`

List all running instances and their state.

```json
{
  "name": "list_instances",
  "arguments": {}
}
```

Returns: all instances with name, state, tmux alive, channel connected, repo URL.

## WS frame types

New frame types added to `ws.Frame`:

| Frame | Direction | Fields |
|-------|-----------|--------|
| `delegate` | plugin → server | `target` (instance name), `text`, `req_id` |
| `delegate_result` | server → plugin | `req_id`, `text`, `error` |
| `delegate_status` | server → plugin | `req_id`, `status` (e.g. "sent", "processing", "timeout") |

## Progress visibility

Everything is visible to the user in Telegram:

1. **Target topic** shows the delegated task (prefixed with manager name) and the target Claude's reply
2. **Manager topic** shows the manager's synthesis after collecting results
3. No hidden work — if you open any topic, you see what happened

## Implementation plan

### Phase 1: Core delegation (MVP)
- [ ] Add `Manager` bool to Instance struct in bbolt
- [ ] Add `/manager` Telegram command to toggle
- [ ] Add `delegate` and `delegate_result` frame types to `ws.Frame`
- [ ] Dispatcher: handle `delegate` frame — route to target, wait for reply, return result
- [ ] Channel plugin: add `delegate` and `list_instances` tools (conditional on manager flag)
- [ ] Telegram: post delegated tasks with 📋 prefix for visibility
- [ ] Per-instance message ring buffer in dispatcher (for poll_instance)

### Phase 2: Broadcast + poll
- [ ] Add `broadcast` tool (parallel delegates)
- [ ] Add `poll_instance` tool (read from ring buffer)
- [ ] Add `delegate_status` frame for progress updates

### Phase 3: Streaming progress
- [ ] Manager subscribes to target instance's reply stream
- [ ] Progress updates forwarded to manager topic in real-time

## Future work (post-MVP)

- **Manager can /start new instances** — "clone this repo and do X" in one step
- **Permission scoping** — manager can only delegate to a configured subset of instances
- **Task queuing** — if target is busy, queue the delegate and notify when it's picked up
- **Multi-step workflows** — manager defines a DAG of tasks across instances
- **Manager-to-manager delegation** — hierarchical orchestration
