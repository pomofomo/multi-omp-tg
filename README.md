# telegram-repo-dispatcher (`trd`)

Talk to multiple [omp](https://github.com/oh-my-pi/oh-my-pi) coding agent instances from one Telegram supergroup. **Each topic = one repo.** Create a topic, paste a git URL, and you're coding.

**This project is meant to be forked and modified.** Clone it, hack on it, and make it your own. PRs and issues are very welcome.

## How it works

A single Go binary connects to your Telegram bot. For each forum topic bound to a repo:

1. The first user message clones the repo (on `/start`).
2. Every subsequent message spawns `omp -p` in that repo, with `--resume` pointed at the omp session id captured from the previous run.
3. omp's reply streams back to the topic as live-edited Telegram messages (debounced ~1.5s, automatic 4000-char roll-over), and the agent can also emit voice-memo replies via a TTS tool.
4. Voice messages from the user are transcribed (whisper) and prefixed to the prompt.

There is **no persistent agent process**, no tmux per instance, no WebSocket bridge, no MCP channel plugin. Each user message is a one-shot subprocess; conversation continuity lives in omp's own session storage at `~/.omp/agent/sessions/`.

Voice processing (whisper STT) and Opus audio decoding are embedded directly in the binary — no ffmpeg, no Python, no external CLI tools.

## Quick start

```bash
# 1. Clone the repo
git clone https://github.com/pomofomo/multi-omp-tg.git
cd multi-omp-tg

# 2. Check prerequisites (Go, libopus-dev, libopusfile-dev, omp, …).
#    Tells you exactly what's missing for your distro.
make install-deps

# 3. (Optional) Download voice models (~230MB)
make install-models

# 4. Create a Telegram bot:
#    - Talk to @BotFather, /newbot, grab the token
#    - /setprivacy → DISABLE  (so the bot can read messages in groups)
#    - Create a supergroup with Topics enabled
#    - Add the bot as ADMIN  (required to read forum message_thread_id)
#
#    A non-admin bot will look like it "isn't seeing" your messages.

# 5. Start TRD
make setup TELEGRAM_BOT_TOKEN=123456:ABCDEF...
```

`make setup` builds `bin/trd`, copies it to `~/.local/bin/trd`, and starts the dispatcher inside an operator tmux session named `trd` (only so it survives an SSH disconnect — agents are not in tmux). If `~/.local/bin` isn't on `$PATH`, `make install-deps` warns you — add it to your shell rc.

After setup, your token is in bbolt; future starts need no env vars:

```bash
make start              # start (reads saved config)
make restart            # rebuild + restart after code changes
```

## Usage

In your Telegram supergroup, create a topic and send:

```
/start git@github.com:you/your-repo.git
```

The repo is cloned and bound to the topic. Then talk to it. Send voice messages. Each message spawns `omp -p` in the cloned repo, resumes the previous omp session, and forwards the reply.

### Telegram commands

| Command | Effect |
|---------|--------|
| `/start <git-url>` | Clone repo, bind to this topic |
| `/restart_dispatcher` | Drain in-flight runs and re-exec the dispatcher in place. Controller-only — set with `trd promote <repo-name>`. |
| `/reset` | Forget the omp session id; next message starts fresh |
| `/status` | Show instance, session, run state, model, thinking level |
| `/model [name]` | Show or change the model (e.g. `/model opus`) |
| `/effort [level]` | Show or change thinking level (`minimal`, `low`, `medium`, `high`, `xhigh`) |
| `/cancel` | Interrupt the in-flight agent run for this topic |
| `/debug` | Toggle dispatcher debug logging |
| `/forget` | Delete the topic-repo mapping |
| `/help` | Show available commands |

### CLI commands

```bash
trd list               # all instances with state, session id, running flag
trd watch <name>       # tail agent log for an instance
trd shell <name>       # open shell in repo
trd cd <name>          # print repo path
trd stop <name>        # cancel any in-flight agent run
trd allow <user>       # add to allowlist
trd promote <name>     # flag this instance as the controller (authorises /restart_dispatcher & /api/restart)
trd demote <name>      # clear the controller flag
trd deny <user>        # remove from allowlist
trd allowed            # show allowlist
```

## Model / effort per repo

`/model` and `/effort` write to `<repo>/.trd/agent.json`:

```json
{ "model": "opus", "thinking": "high", "updated_at": "2026-05-27T08:30:00Z" }
```

The next `omp -p` invocation in that repo passes `--model` and `--thinking` from this file. Use `/model -` or `/effort -` to clear back to omp's default.

## Voice

**Inbound** — send a voice note in a topic and TRD transcribes it with embedded whisper, prefixing the text to the prompt that omp receives.

**Outbound** — the agent can speak back. The omp extension registers a `tg_voice(text)` tool: when called, the dispatcher synthesises the text via its embedded TTS (sherpa-onnx VITS, or OpenAI TTS if `TRD_OPENAI_API_KEY` is set) and ships it as an OGG/Opus voice memo threaded to the originating message. The bot decides when to use voice; the description steers it toward short conversational replies (acks, quick status updates) and away from anything with code, links, or markdown.

```bash
make install-models     # downloads whisper + TTS models to ~/.trd/models/
```

Models are auto-detected at `~/.trd/models/`. No ffmpeg needed.

## User allowlist

By default, anyone in the supergroup can use TRD. To restrict:

```bash
trd allow alice         # only @alice can interact
trd allow bob
trd allowed             # see the list
trd deny bob            # remove
```

Empty allowlist = everyone allowed.

## Day-to-day

```bash
tmux attach -t trd      # see dispatcher logs (Ctrl+B, D to detach; tmux path only)
make restart            # rebuild + restart after code changes
trd status              # check all instances
trd watch <name>        # tail an instance's agent log
```

### Running under systemd (recommended for headless hosts)

By default `make setup` runs the dispatcher inside an operator tmux session. For unattended hosts, install a `systemd --user` unit instead:

```bash
make install-systemd    # one-time: writes ~/.config/systemd/user/trd.service
sudo loginctl enable-linger "$USER"   # optional but required on headless hosts
```

After that, `make restart` switches automatically to `systemctl --user restart trd`, and crashes / OOMs / host reboots respawn the dispatcher in ~2s. Live logs: `journalctl --user -u trd -f`.

`install-systemd` also writes a drop-in at `~/.config/systemd/user/trd.service.d/env.conf` that captures your **current shell's `$PATH`** and pins **`TRD_OMP_BIN`** to the absolute path of `omp` as resolved at install time. systemd `--user` boots with an almost-empty environment, so without this drop-in `omp` (typically installed via nvm) is not found and agent spawns fail with `exec: "omp": executable file not found in $PATH`.

**Re-run `make install-systemd` whenever you change node versions (nvm) or move `omp`**, otherwise the pinned path goes stale.

Conversation history survives dispatcher restarts: it lives in `~/.omp/agent/sessions/<cwd-mangled>/<session>.jsonl`, and bbolt remembers the session id. If a run is killed mid-stream the session file may stay in `.tmp` form and become unresumable — in that case `/reset` and continue.

## Developing TRD itself

TRD can manage its own repo. Fork it, `/start` your fork in a topic, and edit it through Telegram. After your changes:

```bash
make restart            # rebuild and bounce the dispatcher
```

See [DEV.md](./DEV.md) for the developer guide.

## Documentation

| Doc | What's in it |
|-----|-------------|
| [ARCHITECTURE.md](./ARCHITECTURE.md) | System design, message flows, key decisions |
| [DEBUG.md](./DEBUG.md) | Operational debugging notes, case studies, self-restart docs |
| [DEV.md](./DEV.md) | Contributing, code layout, debugging, env vars |
| [CLAUDE.md](./CLAUDE.md) | Key context for an agent editing this repo |
| [TECH_STACK.md](./TECH_STACK.md) | Languages, libraries, system dependencies |
| [porting/](./porting/) | Historical port notes (pre-headless architecture) |

## License

MIT — see [LICENSE](./LICENSE).
