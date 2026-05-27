# Tech Stack

The technologies and libraries used in TRD, explained so you can reuse the same stack for a different project.

## Language: Go 1.23

Go for the only binary. Chosen for single-binary distribution, fast startup, low memory, and excellent concurrency primitives. CGo is required for the audio/ML libraries.

```bash
go.dev/dl   # install
```

There is no TypeScript code in the project after the headless-omp port — the old MCP channel plugin is gone.

## Database: bbolt

[go.etcd.io/bbolt](https://pkg.go.dev/go.etcd.io/bbolt) — embedded key-value store. Pure Go, single-file database (`~/.trd/state.db`), transactional, zero external dependencies.

Used for: instance state (incl. captured omp session id), user allowlist, persistent settings.

**Why bbolt:** No server to run, survives crashes (ACID), tiny footprint, perfect for single-process apps. The etcd project's fork of BoltDB.

```go
import bolt "go.etcd.io/bbolt"

db, _ := bolt.Open("state.db", 0o600, nil)
db.Update(func(tx *bolt.Tx) error {
    b, _ := tx.CreateBucketIfNotExists([]byte("data"))
    return b.Put([]byte("key"), []byte("value"))
})
```

## HTTP router: net/http stdlib

No framework. Go 1.22+ `http.ServeMux` supports path parameters (`/api/allowed/{username}`, `/api/instances/{id}/cancel`), which eliminates the need for chi/gorilla/echo for simple APIs.

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /api/items", handleList)
mux.HandleFunc("POST /api/items/{id}/cancel", handleCancel)
```

## Subprocess: os/exec

`omp -p` is the agent. We invoke it once per Telegram message via `exec.CommandContext`. Stdout is consumed as NDJSON; stderr is teed to `~/.trd/logs/<instance-id>.log`. The process group is detached (`Setpgid: true`) so SIGINT cleanly reaches every grandchild.

```go
cmd := exec.CommandContext(ctx, "omp", args...)
cmd.Dir = inst.RepoPath
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
stdout, _ := cmd.StdoutPipe()
_ = cmd.Start()
// … scan NDJSON from stdout …
```

## UUID: google/uuid

[github.com/google/uuid](https://pkg.go.dev/github.com/google/uuid) — standard UUID v4 generation for instance ids.

```go
id := uuid.NewString() // "550e8400-e29b-41d4-a716-446655440000"
```

## Speech-to-text: sherpa-onnx (whisper)

[github.com/k2-fsa/sherpa-onnx-go](https://github.com/k2-fsa/sherpa-onnx-go) — C++ inference engine with Go bindings (CGo). Runs ONNX models for speech recognition and TTS. The shared libraries are bundled in the Go module — no system install needed.

Used for: embedded whisper transcription of voice messages, fed back to the agent as plain text in the prompt.

**Why this over calling whisper CLI:** No subprocess overhead, model loaded once and reused, ~14s for 2.5 min audio on CPU.

```go
import sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

config := sherpa.OfflineRecognizerConfig{...}
recognizer := sherpa.NewOfflineRecognizer(&config)
stream := sherpa.NewOfflineStream(recognizer)
stream.AcceptWaveform(16000, samples)
recognizer.Decode(stream)
text := stream.GetResult().Text
```

**Models:** Download from [sherpa-onnx releases](https://github.com/k2-fsa/sherpa-onnx/releases/tag/asr-models). We use `sherpa-onnx-whisper-base.en` (~165MB, int8 quantized).

## Text-to-speech: sherpa-onnx (VITS piper)

Same library as above. Models are still downloadable, but outbound TTS is **not** wired in the headless port — there is no agent-side tool to invoke it. The package remains in tree as a follow-up.

## Audio codec: hraban/opus

[github.com/hraban/opus](https://pkg.go.dev/github.com/hraban/opus) — Go bindings for libopus (CGo). Encode and decode Opus audio.

Used for: decoding incoming Telegram voice messages (OGG/Opus → PCM). Encoding is still in the package but unused post-port.

**Build dependency:** `apt install libopus-dev libopusfile-dev`

```go
import "github.com/hraban/opus"

dec, _ := opus.NewDecoder(48000, 1)
n, _ := dec.Decode(packet, pcmBuffer)
```

## Coding agent: omp

[oh-my-pi](https://github.com/oh-my-pi/oh-my-pi) — the coding agent itself. Invoked as `omp -p --mode json --resume <session-id> <prompt>` per message. Conversation state lives in `~/.omp/agent/sessions/`; we just remember the session id per instance.

**Why omp:** Multi-provider (Anthropic, OpenAI, Google, etc.), structured NDJSON output mode suitable for programmatic consumption, native session-resume, no extra protocol layer needed.

```bash
npm install -g @oh-my-pi/pi-coding-agent
```

## Process management: optional tmux

Not for agents — only for the **dispatcher itself**. The `make setup` / `make restart` targets run `trd start` inside an operator tmux session named `trd` so it survives an SSH disconnect. If you have systemd or another supervisor, you don't need tmux.

There is no per-instance tmux in the headless port. Agents are stateless subprocesses.

## Telegram: hand-rolled net/http client

No third-party Telegram library. A minimal `internal/telegram` package (~300 lines) wraps only the Bot API methods TRD actually uses: `getMe`, `getUpdates`, `sendMessage`, `editMessageText`, `setReaction`, `sendDocument`, `sendPhoto`, `sendVoice`, `sendAudio`, `getFile`.

**Why no library:** Keeps the dependency tree small, easy to understand, only implements what's needed. Long-polling is a simple loop.

## Secret management

No external secret store. Secrets are:

- **Bot token:** env var → persisted to bbolt settings bucket.
- **HTTP API:** localhost only (`127.0.0.1`); no auth.

For production deployments, set env vars via your secret manager of choice and let TRD persist them to bbolt on first start.

## Summary table

| Concern | Choice | Import/Install |
|---------|--------|----------------|
| Language | Go 1.23 | go.dev/dl |
| Database | bbolt | `go.etcd.io/bbolt` |
| HTTP router | stdlib net/http | built-in |
| UUID | google/uuid | `github.com/google/uuid` |
| Speech-to-text | sherpa-onnx (whisper) | `github.com/k2-fsa/sherpa-onnx-go` |
| Text-to-speech | sherpa-onnx (VITS piper) | same as above (unused post-port) |
| Audio codec | hraban/opus | `github.com/hraban/opus` + libopus-dev |
| Coding agent | omp | `npm i -g @oh-my-pi/pi-coding-agent` |
| Process supervisor (operator) | tmux (optional) | system package |
| Telegram API | hand-rolled net/http | internal package |
| Secrets | env vars + bbolt | no external deps |
