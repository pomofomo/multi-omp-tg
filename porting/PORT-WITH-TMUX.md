# Porting TRD to OMP Harness

This document identifies every Claude Code-specific coupling in the TRD codebase and describes what a port to the OMP harness would need to replace.

## Architecture context

TRD is three components:

```
Telegram ──> dispatcher (Go) ──WS──> channel plugin (TS) ──MCP──> Claude Code
                │                       │
           bbolt + tmux             .mcp.json + tools
```

## Summary: what survives, what changes

| Layer | File(s) | Port status |
|-------|---------|-------------|
| Telegram client | `internal/telegram/` | **Reusable as-is** |
| bbolt storage | `internal/storage/` | **Reusable as-is** |
| tmux manager | `internal/tmuxmgr/` | **Reusable as-is** |
| WS server + Frame protocol | `internal/ws/` | **Reusable as-is** |
| Voice pipeline (whisper/TTS) | `internal/media/`, `internal/audio/` | **Reusable as-is** |
| CLI entry point | `cmd/trd/` | **Reusable as-is** |
| Config paths | `internal/config/` | **Reusable as-is** |
| Dispatcher core | `internal/dispatcher/dispatcher.go` | **~80% reusable** — launch, auto-confirm, rate-limit watchdog, `/model`, `/effort` need porting |
| Channel plugin | `channel/index.ts` | **Full rewrite** — MCP protocol is Claude-specific |
| `.mcp.json` generation | `writeMCPConfig` in dispatcher.go | **Replace** with OMP equivalent |

---

## 1. Channel plugin (`channel/index.ts`) — Full rewrite

This is a Claude Code MCP server. Every line is Claude-specific:

### What it does
- Connects via stdio transport using `@modelcontextprotocol/sdk`
- Declares an `experimental: { "claude/channel": {} }` capability
- Emits `notifications/claude/channel` notifications for inbound messages
- Exposes tools: `reply`, `react`, `edit_message`, `download_attachment`, `send_voice`
- Includes behavioral instructions (acknowledgment patterns, voice reply rules)

### What an OMP port needs
Replace with an OMP agent adapter that:

1. **Reads `.trd/config.json`** — instance ID, secret, dispatcher port (same format, no changes needed)
2. **Opens a WebSocket** to `ws://127.0.0.1:{port}/channel?secret={secret}` — same wire protocol, no changes
3. **Handles inbound frames** (`type: "message"`, `type: "download_result"`, `type: "tts_result"`, `type: "delegate_result"`, `type: "config"`) — same JSON structure, no changes
4. **Sends outbound frames** (`type: "reply"`, `type: "react"`, `type: "edit"`, `type: "download"`, `type: "tts"`, `type: "delegate"`) — same JSON structure, no changes
5. **Translates to OMP's tool/notification protocol** instead of MCP

### Frame types to implement (same JSON schema as `ws.Frame`)

| Direction | Frame type | What the adapter must do |
|-----------|-----------|--------------------------|
| inbound | `message` | Deliver to OMP agent as a user message with metadata |
| inbound | `download_result` | Resolve pending download promise, return path to agent |
| inbound | `tts_result` | Resolve pending TTS promise |
| inbound | `delegate_result` | Resolve pending delegate promise |
| inbound | `config` | Set `isManager` flag (enables extra tools) |
| outbound | `reply` | Agent calls → send frame to dispatcher |
| outbound | `react` | Agent calls → send frame to dispatcher |
| outbound | `edit` | Agent calls → send frame to dispatcher |
| outbound | `download` | Agent calls → send frame, await `download_result` |
| outbound | `tts` | Agent calls → send frame, await `tts_result` |
| outbound | `hello` | Send on connect with `instance_id` |
| outbound | `delegate` | Manager agent calls → send frame, await `delegate_result` |

---

## 2. Dispatcher launch (`internal/dispatcher/dispatcher.go`) — Partial rewrite

### Lines that need porting

**`launchTmuxWithOpts`** (~line 1080-1110 in dispatcher.go):

```go
claudeBin := firstNonEmpty(os.Getenv("TRD_CLAUDE_BIN"), "claude")
claudeArgs := os.Getenv("TRD_CLAUDE_ARGS")
if claudeArgs == "" {
    claudeArgs = "--dangerously-skip-permissions --dangerously-load-development-channels server:trd-channel"
    if d.opts.Debug {
        claudeArgs = "--debug " + claudeArgs
    }
}
if resume {
    claudeArgs += " --session-id " + inst.InstanceID
}
cmd := fmt.Sprintf("%s %s", claudeBin, claudeArgs)
```

**What to change:** Replace `claudeBin`/`claudeArgs` with OMP agent launch command.

Add env vars (or reuse `TRD_CLAUDE_BIN`/`TRD_CLAUDE_ARGS` but rename):
- `TRD_AGENT_BIN` — path to OMP agent binary
- `TRD_AGENT_ARGS` — args to pass (replaces `--dangerously-skip-permissions --dangerously-load-development-channels server:trd-channel`)
- Session persistence: OMP's equivalent of `--session-id` for resuming conversations

### Lines to remove or adapt

| Feature | Lines | Claude coupling | Port action |
|---------|-------|-----------------|-------------|
| Auto-confirm prompts | `autoConfirm()` ~20 lines | Hunts for "enter to confirm", "local development", "y/n" in Claude's TUI | Replace with OMP prompt detection or remove |
| Rate-limit watchdog | `checkRateLimit()` ~50 lines | Parses "rate-limit-options", "hit your limit", "Waiting for...limit" in Claude's TUI | Replace with OMP rate-limit detection or remove |
| `/model` command | `cmdModel()` ~30 lines | Types `/model` into tmux pane | Remove or replace with OMP model switching |
| `/effort` command | `cmdEffort()` ~30 lines | Types `/effort` into tmux pane | Remove or replace with OMP effort control |
| `--debug` flag passthrough | `launchTmuxWithOpts` 3 lines | Passes `--debug` to claude binary | Adapt to OMP debug flag |
| Dev-channels prompt | `writeMCPConfig()` ~40 lines | Writes `.mcp.json` with `trd-channel` MCP server entry | Write OMP equivalent config file instead |

### `.mcp.json` generation (`writeMCPConfig`)

Claude discovers channel plugins via `.mcp.json` in the repo root. OMP likely has a different discovery mechanism. Replace `writeMCPConfig` with whatever config file OMP agents read to find their adapter.

Current format written:
```json
{
  "mcpServers": {
    "trd-channel": {
      "command": "bun",
      "args": ["run", "path/to/channel/index.ts"]
    }
  }
}
```

Priority for channel entry path:
1. `$TRD_CHANNEL_ENTRY` set → `bun run <path>` (dev)
2. Default → `trd-channel` (npm bin on PATH)

---

## 3. Config files written into repos — Partial rewrite

### `.trd/config.json` — No changes needed

```json
{
  "instance_id": "uuid",
  "secret": "256-bit-hex",
  "dispatcher_port": 7777
}
```

This is the identity file read by the channel plugin (now the OMP adapter). Format is platform-agnostic.

### `.mcp.json` — Replace

Replace with OMP's equivalent of a "here's how to find the adapter" config file.

### `.gitignore` — No changes needed

`config.EnsureGitignore()` adds `.trd/`, `.mcp.json`, `.omc/`. Update `.mcp.json` to whatever OMP uses.

---

## 4. Platform-agnostic layers (no changes needed)

These packages have zero Claude coupling and work as-is:

| Package | Purpose | Lines |
|---------|---------|-------|
| `internal/telegram/` | Telegram Bot API client (getUpdates, sendMessage, sendPhoto, sendVoice, etc.) | ~300 |
| `internal/storage/` | bbolt wrapper (instances, topic/secret indexes, allowlist, settings) | ~250 |
| `internal/tmuxmgr/` | tmux session lifecycle (new, has, kill, capture-pane, send-keys) | ~70 |
| `internal/ws/` | WebSocket server, Frame protocol, HTTP API endpoints | ~280 |
| `internal/media/` | Whisper STT + VITS TTS via sherpa-onnx, OpenAI fallback | ~350 |
| `internal/audio/` | OGG/Opus decode/encode via libopus (no ffmpeg) | ~250 |
| `internal/config/` | Paths (~/.trd/...), RepoConfig read/write, gitignore | ~130 |
| `cmd/trd/` | CLI (start, status, stop, watch, shell, cd, allow, deny, allowed) | ~300 |

---

## 5. Dispatcher core — what stays

These dispatcher methods are platform-agnostic:

- `pollLoop()` — Telegram long-poll, message routing
- `cmdStart()` — clone repo, generate secret, write config, persist to bbolt
- `cmdStop()`, `cmdRestart()`, `cmdReset()`, `cmdStatus()`, `cmdForget()`, `cmdWatch()`, `cmdHelp()`
- `cmdManager()` — toggle manager flag
- `cmdDebug()` — toggle debug flag (remains useful)
- `routeToInstance()` — forwards Telegram messages to WS channel
- `handleEditedMessage()` — forwards edited messages
- `sendFileSmartly()` — picks sendPhoto/sendVoice/sendAudio/sendDocument based on extension
- `transcribeAttachment()` — downloads Telegram file, runs Whisper
- `handleDelegate()` — manager → target instance delegation via WS
- `findInstance()` — resolve instance by name or ID prefix
- `checkHealth()` / `sweepAttachments()` — health loop and cleanup
- `isUserAllowed()` — allowlist check
- `resumeInstances()` — relaunch dead tmux sessions on dispatcher restart
- `normalizeRepoURL()` — URL normalization

---

## 6. Porting checklist

### Phase 1: Adapter (replace channel plugin)
- [ ] Write OMP adapter that reads `.trd/config.json`
- [ ] Implement WS connect with exponential backoff (same as current: 500ms → 10s)
- [ ] Handle all 5 inbound frame types
- [ ] Implement all 7 outbound frame types
- [ ] Map to OMP tool/notification protocol
- [ ] Port behavioral instructions (acknowledgment patterns) to OMP system prompt

### Phase 2: Launch (replace Claude-specific dispatcher code)
- [ ] Replace `claudeBin`/`claudeArgs` with OMP agent launch
- [ ] Replace `writeMCPConfig` with OMP config file generation
- [ ] Replace or remove `autoConfirm()`
- [ ] Replace or remove `checkRateLimit()` watchdog
- [ ] Remove `/model` and `/effort` commands (or map to OMP equivalents)

### Phase 3: Cleanup
- [ ] Rename `TRD_CLAUDE_BIN` → `TRD_AGENT_BIN` (with backward compat)
- [ ] Rename `TRD_CLAUDE_ARGS` → `TRD_AGENT_ARGS`
- [ ] Update `.gitignore` additions (`.mcp.json` → OMP config filename)
- [ ] Update help text in `cmdHelp()`
- [ ] Update `SetMyCommands()` bot command list

---

## 7. WS wire protocol reference

The WebSocket protocol between dispatcher and adapter is the stable contract. Do not change it.

### Server → Adapter

```json
{"type":"message","chat_id":-100123,"message_id":42,"thread_id":5,"user":"alice","text":"fix the bug","ts":1700000000}
{"type":"message","chat_id":-100123,"message_id":43,"thread_id":5,"user":"alice","text":"transcribed voice message","ts":1700000001,"attachment_file_id":"voice123","attachment_name":"voice.ogg"}
{"type":"download_result","req_id":"dl-123-abc","path":"/home/user/.trd/attachments/file.pdf"}
{"type":"download_result","req_id":"dl-456-def","error":"file too large"}
{"type":"tts_result","req_id":"tts-789-ghi","path":"/home/user/.trd/attachments/tts-123.ogg"}
{"type":"delegate_result","req_id":"del-012-jkl","text":"Done: added rate limiting to /api/users"}
{"type":"delegate_result","req_id":"del-345-mno","error":"delegate timed out (5m)"}
{"type":"config","manager":true}
```

### Adapter → Server

```json
{"type":"hello","instance_id":"uuid"}
{"type":"reply","chat_id":-100123,"text":"fixed","reply_to":42,"files":[]}
{"type":"react","chat_id":-100123,"message_id":42,"emoji":"👍"}
{"type":"edit","chat_id":-100123,"message_id":99,"text":"updated status"}
{"type":"download","file_id":"doc123","req_id":"dl-123-abc"}
{"type":"tts","text":"hello world","req_id":"tts-789-ghi"}
{"type":"delegate","target":"backend","text":"Add rate limiting","req_id":"del-012-jkl"}
```
