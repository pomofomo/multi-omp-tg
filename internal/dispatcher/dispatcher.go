// Package dispatcher is the heart of TRD in its headless-omp incarnation:
// it owns the Telegram long-poll, the small HTTP control plane, and a
// per-instance FIFO of one-shot `omp -p` invocations.
//
// Lifecycle of a Telegram message arriving in a topic bound to instance I:
//
//   1. routeToInstance      pulls I from bbolt, builds the prompt
//                           (text + voice transcript + attachment notes).
//   2. enqueueOrRun         either spawns omp immediately (no run in flight)
//                           or appends to per-instance pendingQueue.
//   3. driveAgentRun        agent.Start → range events → forward to Telegram.
//   4. finishRun            cleans up; drains the next queued prompt if any.
//
// There is no persistent agent. There is no WebSocket. There is no tmux.
// Everything the dispatcher remembers across messages lives in bbolt:
//   - Instance.SessionID — omp session UUID, used as `--resume` next turn.
//   - Allowlist, persistent settings — same as before.
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/pomofomo/multi-omp-tg/internal/agent"
	"github.com/pomofomo/multi-omp-tg/internal/agent/extension"
	"github.com/pomofomo/multi-omp-tg/internal/api"
	"github.com/pomofomo/multi-omp-tg/internal/config"
	"github.com/pomofomo/multi-omp-tg/internal/media"
	"github.com/pomofomo/multi-omp-tg/internal/storage"
	"github.com/pomofomo/multi-omp-tg/internal/telegram"
)

// Options for Run.
type Options struct {
	TelegramToken string
	Port          int
	Logger        *slog.Logger
	// AttachDir is where downloaded Telegram attachments are written.
	// Defaults to ~/.trd/attachments.
	AttachDir string
	// Debug toggles verbose logging on the dispatcher side. omp itself has
	// no --debug flag in print mode; the value is still surfaced to event
	// logging and recorded per-instance via /debug.
	Debug bool
	// OMPBinary overrides the omp executable path. Empty falls back to
	// $TRD_OMP_BIN then "omp" on PATH.
	OMPBinary string
	// RunTimeout caps how long a single agent run may take before the
	// dispatcher SIGINTs it. 0 → 15 minutes.
	RunTimeout time.Duration
	// runner is a test seam for swapping out the agent.Start function.
	// Production callers leave this nil; tests set it to a fake.
	runner runner
}

// runner is the minimal abstraction we need over agent.Start so tests can
// substitute an in-process fake. Exposed only to the test file via
// Options.runner.
type runner interface {
	Start(ctx context.Context, opts agent.RunOptions) (agentHandle, error)
}

// agentHandle is the slice of *agent.Run the dispatcher uses. The real
// *agent.Run satisfies it; tests provide their own implementation.
type agentHandle interface {
	Events() <-chan agent.Event
	Cancel(grace time.Duration)
}

// defaultRunner wraps agent.Start.
type defaultRunner struct{}

func (defaultRunner) Start(ctx context.Context, opts agent.RunOptions) (agentHandle, error) {
	r, err := agent.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Dispatcher glues the subsystems together.
type Dispatcher struct {
	opts   Options
	logger *slog.Logger
	tg     *telegram.Client
	store  *storage.Store
	api    *api.Server
	media  *media.Engine
	runner runner

	// sendMessage and setReaction are function-field test seams over the
	// telegram client. New() defaults them to d.tg.SendMessage and
	// d.tg.SetReaction; tests substitute fakes to verify routing without
	// hitting the wire.
	sendMessage func(ctx context.Context, p telegram.SendMessageParams) (telegram.Message, error)
	setReaction func(ctx context.Context, chatID int64, msgID int, emoji string) error

	// editMessage updates an existing Telegram message in place. The
	// streamingReply state machine uses it to live-edit a placeholder
	// as the agent emits deltas (see stream.go). Default wires to
	// d.tg.EditMessageText.
	editMessage func(ctx context.Context, p telegram.EditMessageTextParams) error

	// sendVoice uploads an OGG/Opus file as a Telegram voice note.
	// Defaults to d.tg.SendVoice; tests substitute a fake that records
	// the upload without hitting the wire.
	sendVoice func(ctx context.Context, chatID int64, threadID, replyTo int, path string, caption string) (telegram.Message, error)

	// synthesize converts text → OGG path. Defaults to
	// d.media.Synthesize; tests substitute a fake that returns a pre-made
	// path so they don't depend on a loaded TTS model.
	synthesize func(ctx context.Context, text, outDir string) (string, error)

	// canSynthesize reports whether TTS is available. Defaults to
	// d.media.CanSynthesize; the seam lets tests force the gate.
	canSynthesize func() bool

	// extPath is the absolute path of the embedded omp extension that
	// registers TRD's Telegram-aware tools (tg_react). Populated by New
	// via extension.Ensure; empty disables the extension.
	extPath string

	// In-flight agent runs and per-instance pending queues. The dispatcher
	// serialises invocations per instance: a topic always sees its agent's
	// reply to message N before message N+1 is sent.
	runMu        sync.Mutex
	runs         map[string]*agentRun
	pendingQueue map[string][]queuedPrompt
	// runWG tracks in-flight driveAgentRun goroutines so Shutdown can
	// wait for them to drain cleanly before the process exits.
	runWG sync.WaitGroup

	// Restart machinery. See § "Restart" in dispatcher.go's package
	// comment and DEBUG.md "Proposal A — graceful in-process self-restart".
	//
	// pendingRestart is set when a controller calls RequestRestart;
	// enqueueOrRun consults it and persists incoming prompts to bbolt
	// instead of spawning new omp runs. finishRun checks it after every
	// run completes and, when len(runs)==0, calls stopForRestart to
	// unwind Run() so cmdStart can syscall.Exec us in place.
	//
	// lastUpdateID mirrors the Telegram long-poll cursor maintained by
	// pollLoop so the value can be persisted across the re-exec gap.
	// restartOnce guards the single triggerRestartStop call.
	pendingRestart atomic.Bool
	lastUpdateID   atomic.Int64
	restartOnce    sync.Once
	stopForRestart context.CancelFunc
}

// agentRun is the live handle for an `omp -p` invocation. Created by
// driveAgentRun, accessed under runMu.
type agentRun struct {
	instanceID string
	handle     agentHandle
	cancel     context.CancelFunc
	chatID     int64
	thread     int
	msgID      int // telegram message id this run is responding to
	started    time.Time
}

// queuedPrompt holds the data needed to invoke omp for a message that
// arrived while another run was in flight on the same instance.
type queuedPrompt struct {
	chatID int64
	thread int
	msgID  int
	user   string
	text   string
}

// InstanceInfo is the JSON shape returned by GET /api/instances. It adds
// runtime state (whether a run is currently in flight) to the stored row.
type InstanceInfo struct {
	storage.Instance
	Running bool `json:"running"`
}

// New builds a Dispatcher, opening the DB and constructing the API server.
func New(opts Options) (*Dispatcher, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Port == 0 {
		opts.Port = 7777
	}
	if opts.TelegramToken == "" {
		return nil, errors.New("telegram token is required")
	}
	if opts.RunTimeout == 0 {
		opts.RunTimeout = 15 * time.Minute
	}

	if err := config.EnsureRoot(); err != nil {
		return nil, fmt.Errorf("create ~/.trd: %w", err)
	}
	dbPath, err := config.StateDBPath()
	if err != nil {
		return nil, err
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	if opts.AttachDir == "" {
		root, _ := config.Root()
		opts.AttachDir = filepath.Join(root, "attachments")
	}
	if err := os.MkdirAll(opts.AttachDir, 0o700); err != nil {
		store.Close()
		return nil, err
	}

	mediaCfg := media.ConfigFromEnv()
	engine, err := media.NewEngine(mediaCfg)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("init media: %w", err)
	}

	r := opts.runner
	if r == nil {
		r = defaultRunner{}
	}

	d := &Dispatcher{
		opts:         opts,
		logger:       opts.Logger,
		tg:           telegram.New(opts.TelegramToken),
		store:        store,
		media:        engine,
		runner:       r,
		runs:         map[string]*agentRun{},
		pendingQueue: map[string][]queuedPrompt{},
	}
	d.sendMessage = d.tg.SendMessage
	d.setReaction = d.tg.SetReaction
	d.editMessage = d.tg.EditMessageText
	d.sendVoice = d.tg.SendVoice
	d.synthesize = d.media.Synthesize
	d.canSynthesize = d.media.CanSynthesize
	if root, err := config.Root(); err == nil {
		extPath, err := extension.Ensure(root)
		if err != nil {
			opts.Logger.Warn("extension.Ensure failed; tg_react tool will be unavailable", "err", err)
		} else {
			d.extPath = extPath
		}
	}
	d.api = api.New(fmt.Sprintf("127.0.0.1:%d", opts.Port), opts.Logger, d)
	return d, nil
}

// Close flushes state and releases resources.
func (d *Dispatcher) Close() error {
	d.media.Close()
	return d.store.Close()
}

// Shutdown gracefully drains every in-flight omp run before the process
// exits. It SIGINTs each child's process group, blocks for up to grace
// per child (in parallel) for a clean exit, SIGKILLs stragglers, then
// waits for the driveAgentRun goroutines themselves to return so their
// final Telegram replies make it out before the dispatcher closes its
// stdout pipe.
//
// Idempotent and safe to call concurrently with Run returning. Pending
// queued prompts are dropped (they would otherwise spawn new omp runs
// during shutdown).
func (d *Dispatcher) Shutdown(grace time.Duration) {
	d.runMu.Lock()
	handles := make([]agentHandle, 0, len(d.runs))
	for _, run := range d.runs {
		if run.handle != nil {
			handles = append(handles, run.handle)
		}
	}
	dropped := 0
	for id, q := range d.pendingQueue {
		dropped += len(q)
		delete(d.pendingQueue, id)
	}
	d.runMu.Unlock()

	if len(handles) == 0 && dropped == 0 {
		return
	}
	d.logger.Info("shutdown: draining in-flight runs",
		"active", len(handles), "dropped_queued", dropped, "grace", grace)

	// SIGINT each child in parallel so total wait is bounded by grace,
	// not grace*N. Cancel internally waits for the process to exit (then
	// SIGKILLs on timeout), so by the time wg.Wait returns omp is gone.
	var wg sync.WaitGroup
	for _, h := range handles {
		wg.Add(1)
		go func(h agentHandle) {
			defer wg.Done()
			h.Cancel(grace)
		}(h)
	}
	wg.Wait()

	// driveAgentRun is still draining its event channel and may be
	// mid-SendMessage. Wait for those to finish so the last reply lands.
	done := make(chan struct{})
	go func() { d.runWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace + 5*time.Second):
		d.logger.Warn("shutdown: driveAgentRun goroutines did not finish within grace")
	}
}

// --- Graceful self-restart ---

// RequestRestart marks the dispatcher as pending a graceful restart and
// kicks the drain-and-stop sequence. callerInstanceID is the claimed
// identity from the X-Trd-Instance header (api.handleAPIRestart) — it
// must match a stored instance flagged as the controller.
//
// Idempotent: a second call while a restart is already pending is a
// no-op (but still validates the caller).
//
// Implements api.Handler.
func (d *Dispatcher) RequestRestart(callerInstanceID string) error {
	if !d.isControllerInstance(callerInstanceID) {
		d.logger.Warn("restart: unauthorized caller",
			"caller", shortID(callerInstanceID))
		return api.ErrUnauthorized
	}
	if d.pendingRestart.Swap(true) {
		// Already pending — surface success so the caller (curl, agent
		// tool, Telegram command) gets a consistent response.
		return nil
	}

	d.runMu.Lock()
	// Flush every in-memory queued prompt to bbolt so the successor
	// process picks them up after the re-exec. This must happen here
	// (and not in finishRun) so prompts that arrived BEFORE the restart
	// request — already sitting in pendingQueue — are not dropped.
	flushed := 0
	for instID, q := range d.pendingQueue {
		for _, p := range q {
			if err := d.store.EnqueueDeferred(storage.DeferredPrompt{
				InstanceID: instID,
				ChatID:     p.chatID,
				ThreadID:   p.thread,
				MsgID:      p.msgID,
				User:       p.user,
				Text:       p.text,
			}); err != nil {
				d.logger.Warn("restart: defer queued prompt failed",
					"instance", shortID(instID), "msg_id", p.msgID, "err", err)
				continue
			}
			flushed++
		}
		delete(d.pendingQueue, instID)
	}
	active := len(d.runs)
	d.runMu.Unlock()

	d.logger.Info("restart: requested",
		"caller", shortID(callerInstanceID),
		"deferred_queued", flushed,
		"active_runs", active)
	if active == 0 {
		d.triggerRestartStop()
	}
	return nil
}

// PendingRestart reports whether a graceful restart has been requested
// and the dispatcher has begun draining. cmdStart consults this after
// Run returns to decide between syscall.Exec and a normal shutdown.
func (d *Dispatcher) PendingRestart() bool { return d.pendingRestart.Load() }

// FlushBeforeExec persists any state that must survive an in-place
// re-exec. Currently only the long-poll cursor — Instance rows are
// already written eagerly on every transition. Safe to call multiple
// times.
func (d *Dispatcher) FlushBeforeExec() error {
	if v := d.lastUpdateID.Load(); v > 0 {
		if err := d.store.SetLastUpdateID(int(v)); err != nil {
			d.logger.Warn("flush_before_exec: persist last_update_id failed", "err", err)
			return err
		}
	}
	return nil
}

// triggerRestartStop cancels the runCtx held by Run so pollLoop and the
// API server exit, allowing cmdStart to re-exec. Guarded by restartOnce
// because both RequestRestart (when idle) and finishRun (after the last
// run drains) call it.
func (d *Dispatcher) triggerRestartStop() {
	d.restartOnce.Do(func() {
		d.logger.Info("restart: drain complete, stopping for re-exec")
		if d.stopForRestart != nil {
			d.stopForRestart()
		}
	})
}

// isControllerInstance returns true when id is non-empty and resolves to
// a stored instance with Controller=true. Used to authorise restart
// requests both from /api/restart and from the /restart-self command.
func (d *Dispatcher) isControllerInstance(id string) bool {
	if id == "" {
		return false
	}
	inst, err := d.store.Get(id)
	if err != nil || inst == nil {
		return false
	}
	return inst.Controller
}

// redeliverDeferredPrompts drains the bbolt deferred_prompts bucket on
// startup and re-routes each item through enqueueOrRun. Prompts whose
// instance has vanished, been forgotten, or been /stopped between drains
// are logged and dropped — there is no user-visible recovery path for
// them.
//
// Called once at the top of Run, before pollLoop, so any prompt that
// landed during the predecessor's restart drain is processed before new
// Telegram updates can arrive.
func (d *Dispatcher) redeliverDeferredPrompts(_ context.Context) {
	items, err := d.store.DrainDeferred()
	if err != nil {
		d.logger.Warn("startup: drain deferred prompts failed", "err", err)
		return
	}
	if len(items) == 0 {
		return
	}
	d.logger.Info("startup: redriving deferred prompts", "count", len(items))
	for _, item := range items {
		inst, err := d.store.Get(item.InstanceID)
		if err != nil || inst == nil {
			d.logger.Warn("startup: deferred prompt for missing instance, dropping",
				"instance", shortID(item.InstanceID), "msg_id", item.MsgID, "err", err)
			continue
		}
		if inst.State != storage.StateRunning {
			d.logger.Info("startup: deferred prompt for non-running instance, dropping",
				"instance", shortID(item.InstanceID), "state", inst.State, "msg_id", item.MsgID)
			continue
		}
		d.enqueueOrRun(*inst, queuedPrompt{
			chatID: item.ChatID,
			thread: item.ThreadID,
			msgID:  item.MsgID,
			user:   item.User,
			text:   item.Text,
		})
	}
}

// --- api.Handler implementation ---

// ListInstances returns all instances as JSON for the CLI API endpoint,
// enriched with whether a run is currently in flight.
func (d *Dispatcher) ListInstances() ([]byte, error) {
	all, err := d.store.All()
	if err != nil {
		return nil, err
	}
	d.runMu.Lock()
	defer d.runMu.Unlock()
	infos := make([]InstanceInfo, len(all))
	for i, inst := range all {
		_, running := d.runs[inst.InstanceID]
		infos[i] = InstanceInfo{Instance: inst, Running: running}
	}
	return json.Marshal(infos)
}

// ReactToMessage implements api.Handler. It is invoked from the localhost
// HTTP endpoint POST /api/tg/react when the in-process omp extension
// calls its tg_react tool. Synchronous (the agent waits for the call) but
// bounded by a short timeout: we don't want a misbehaving Telegram API
// to stall the agent's first turn.
func (d *Dispatcher) ReactToMessage(chatID int64, messageID int, emoji string) error {
	if chatID == 0 || messageID == 0 || emoji == "" {
		d.logger.Warn("tg_react: invalid args",
			"chat", chatID, "msg_id", messageID, "emoji", emoji)
		return errors.New("react: chat_id, message_id, and emoji are required")
	}
	d.logger.Info("tg_react: tool fired",
		"chat", chatID, "msg_id", messageID, "emoji", emoji)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.setReaction(ctx, chatID, messageID, emoji); err != nil {
		d.logger.Warn("tg_react: setReaction failed",
			"chat", chatID, "msg_id", messageID, "emoji", emoji, "err", err)
		return err
	}
	d.logger.Info("tg_react: applied",
		"chat", chatID, "msg_id", messageID, "emoji", emoji)
	return nil
}

// SendVoiceMemo implements api.Handler. It synthesises text into an
// OGG/Opus file via the media engine, uploads it to Telegram via
// sendVoice, and removes the temp file. Invoked from POST /api/tg/voice
// when the omp `tg_voice` tool fires.
//
// Synchronous from the agent's perspective: TTS + upload typically take
// a few seconds, so we cap at 90 s — enough headroom for cold-start
// sherpa or a slow OpenAI round-trip, short enough that a wedged
// network call can't stall the turn indefinitely.
//
// replyToMsgID=0 omits the reply binding (voice memo lands at the
// bottom of the topic). text is the literal string to speak.
func (d *Dispatcher) SendVoiceMemo(chatID int64, threadID, replyToMsgID int, text string) error {
	text = strings.TrimSpace(text)
	if chatID == 0 || text == "" {
		return errors.New("voice: chat_id and text are required")
	}
	if !d.canSynthesize() {
		return errors.New("voice: TTS is not configured (set TRD_TTS_MODEL_DIR or TRD_OPENAI_API_KEY)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	oggPath, err := d.synthesize(ctx, text, d.opts.AttachDir)
	if err != nil {
		d.logger.Warn("voice synthesize failed",
			"chat", chatID, "thread", threadID, "len", len(text), "err", err)
		return fmt.Errorf("synthesize: %w", err)
	}
	// Best-effort cleanup. We hold the only reference (the path is
	// stamped with UnixNano), so deletion races with nothing.
	defer func() {
		if err := os.Remove(oggPath); err != nil && !os.IsNotExist(err) {
			d.logger.Debug("voice: temp ogg cleanup failed", "path", oggPath, "err", err)
		}
	}()

	if _, err := d.sendVoice(ctx, chatID, threadID, replyToMsgID, oggPath, ""); err != nil {
		d.logger.Warn("voice upload failed",
			"chat", chatID, "thread", threadID, "path", oggPath, "err", err)
		return fmt.Errorf("sendVoice: %w", err)
	}
	d.logger.Info("voice sent",
		"chat", chatID, "thread", threadID, "reply_to", replyToMsgID, "len", len(text))
	return nil
}

// AllowedUsers returns the stored allowlist.
func (d *Dispatcher) AllowedUsers() ([]string, error) { return d.store.ListAllowedUsers() }

// AddAllowedUser adds a username to the stored allowlist.
func (d *Dispatcher) AddAllowedUser(username string) error {
	return d.store.AddAllowedUser(username)
}

// RemoveAllowedUser removes a username from the stored allowlist.
func (d *Dispatcher) RemoveAllowedUser(username string) error {
	return d.store.RemoveAllowedUser(username)
}

// CancelRun aborts any in-flight agent run matching the given instance id
// or repo-name/id prefix. Returns nil when there is nothing to cancel —
// the CLI caller's expectation is "best-effort stop", not "must be running".
func (d *Dispatcher) CancelRun(query string) error {
	inst, err := d.findInstance(query)
	if err != nil {
		return err
	}
	d.runMu.Lock()
	run, ok := d.runs[inst.InstanceID]
	d.runMu.Unlock()
	if !ok {
		return nil
	}
	d.logger.Info("cancel run", "instance", shortID(inst.InstanceID))
	run.cancel()
	run.handle.Cancel(5 * time.Second)
	return nil
}

// PromoteController flags the resolved instance as the controller and
// clears the flag on every other row. Enforces single-controller
// invariant so RequestRestart's authorisation check has a unique
// answer. Returns the canonical instance id.
func (d *Dispatcher) PromoteController(query string) (string, error) {
	inst, err := d.findInstance(query)
	if err != nil {
		return "", err
	}
	all, err := d.store.All()
	if err != nil {
		return "", err
	}
	for _, other := range all {
		if other.InstanceID == inst.InstanceID {
			continue
		}
		if !other.Controller {
			continue
		}
		other.Controller = false
		if err := d.store.Put(other); err != nil {
			return "", fmt.Errorf("clear controller on %s: %w", shortID(other.InstanceID), err)
		}
		d.logger.Info("controller: cleared previous", "instance", shortID(other.InstanceID))
	}
	inst.Controller = true
	if err := d.store.Put(*inst); err != nil {
		return "", err
	}
	d.logger.Info("controller: promoted", "instance", shortID(inst.InstanceID), "repo", inst.RepoName)
	return inst.InstanceID, nil
}

// DemoteController clears the controller flag on the resolved instance.
// No-op when the flag is already clear. Returns the canonical id.
func (d *Dispatcher) DemoteController(query string) (string, error) {
	inst, err := d.findInstance(query)
	if err != nil {
		return "", err
	}
	if !inst.Controller {
		return inst.InstanceID, nil
	}
	inst.Controller = false
	if err := d.store.Put(*inst); err != nil {
		return "", err
	}
	d.logger.Info("controller: demoted", "instance", shortID(inst.InstanceID), "repo", inst.RepoName)
	return inst.InstanceID, nil
}

// isUserAllowed checks a Telegram username against the combined allowlist
// (stored users + TRD_ALLOWED_USERNAMES env var). An empty combined list
// means everyone is allowed (backwards compatible).
func (d *Dispatcher) isUserAllowed(username string) bool {
	if username == "" {
		// No username to check — allow (Telegram users without usernames
		// can't be allowlisted, so blocking them would be surprising).
		return true
	}
	username = strings.ToLower(username)

	// Check env var first.
	if env := os.Getenv("TRD_ALLOWED_USERNAMES"); env != "" {
		for _, u := range strings.Split(env, ",") {
			if strings.ToLower(strings.TrimSpace(u)) == username {
				return true
			}
		}
		if d.store.IsAllowedUser(username) {
			return true
		}
		return false
	}

	stored, _ := d.store.ListAllowedUsers()
	if len(stored) == 0 {
		return true
	}
	return d.store.IsAllowedUser(username)
}

// SaveSettings persists the current values of the given env var keys
// into the bbolt settings bucket, so future restarts can use them as
// fallbacks when the env vars aren't set.
func (d *Dispatcher) SaveSettings(keys []string) {
	for _, key := range keys {
		if val := os.Getenv(key); val != "" {
			if err := d.store.SetSetting(key, val); err != nil {
				d.logger.Warn("save setting", "key", key, "err", err)
			}
		}
	}
}

// --- Telegram long-poll and command handling ---

// Run starts the HTTP API and Telegram long-poll. Blocks until ctx is
// canceled OR a graceful restart has been requested and all in-flight
// runs have drained (see RequestRestart / stopForRestart).
func (d *Dispatcher) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	d.stopForRestart = cancel
	defer cancel()

	go func() {
		if err := d.api.ListenAndServe(runCtx); err != nil {
			d.logger.Error("api server", "err", err)
		}
	}()

	// Re-deliver any prompts that landed during a prior restart drain.
	d.redeliverDeferredPrompts(runCtx)

	return d.pollLoop(runCtx)
}

func (d *Dispatcher) pollLoop(ctx context.Context) error {
	me, err := d.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	d.logger.Info("telegram bot online", "username", me.Username)

	if err := d.tg.SetMyCommands(ctx, []telegram.BotCommand{
		{Command: "start", Description: "Clone a repo and bind it to this topic: /start <git-url>"},
		{Command: "reset", Description: "Forget the session id; next message starts fresh"},
		{Command: "status", Description: "Show instance, session, and run state"},
		{Command: "model", Description: "Show or change model (e.g. /model opus)"},
		{Command: "effort", Description: "Show or change thinking level (minimal, low, medium, high, xhigh)"},
		{Command: "debug", Description: "Toggle dispatcher debug logging"},
		{Command: "cancel", Description: "Interrupt the in-flight agent run for this topic"},
		{Command: "forget", Description: "Delete the topic-repo mapping"},
		{Command: "help", Description: "Show available commands"},
		{Command: "restart_dispatcher", Description: "Controller-only: drain in-flight runs and re-exec the dispatcher in place"},
	}); err != nil {
		d.logger.Warn("setMyCommands failed", "err", err)
	}

	// Seed the long-poll cursor from bbolt so we resume across an
	// in-place re-exec without redelivering or skipping updates.
	offset := d.store.GetLastUpdateID()
	if offset > 0 {
		d.logger.Info("telegram poll: resuming from persisted offset", "offset", offset)
	}
	d.lastUpdateID.Store(int64(offset))
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		updates, raws, err := d.tg.GetUpdatesRaw(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			d.logger.Warn("getUpdates failed", "err", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for i, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			d.logger.Info("tg raw update", "update_id", u.UpdateID, "raw", string(raws[i]))
			if u.Message != nil {
				d.handleMessage(ctx, u.Message)
			}
			if u.EditedMessage != nil {
				d.handleEditedMessage(ctx, u.EditedMessage)
			}
		}
		// Persist the cursor when it advances so the successor of an
		// in-place re-exec (or a crash) resumes correctly. Skip the
		// write when the batch was empty (offset unchanged).
		if len(updates) > 0 {
			d.lastUpdateID.Store(int64(offset))
			if err := d.store.SetLastUpdateID(offset); err != nil {
				d.logger.Warn("persist last_update_id failed", "err", err)
			}
		}
	}
}

func (d *Dispatcher) handleMessage(ctx context.Context, m *telegram.Message) {
	user := ""
	if m.From != nil {
		user = m.From.Username
		if user == "" {
			user = m.From.FirstName
		}
	}
	rawText := m.Text
	if rawText == "" {
		rawText = m.Caption
	}
	d.logger.Info("tg recv",
		"chat", m.Chat.ID, "chat_type", m.Chat.Type, "is_forum", m.Chat.IsForum,
		"thread", m.MessageThreadID, "msg_id", m.MessageID,
		"from", user, "text_len", len(rawText), "text", preview(rawText),
		"has_doc", m.Document != nil, "photos", len(m.Photo),
		"voice", m.Voice != nil, "audio", m.Audio != nil,
	)
	if m.Chat.Type != "supergroup" || !m.Chat.IsForum {
		d.logger.Info("tg recv rejected: not forum supergroup", "chat", m.Chat.ID, "chat_type", m.Chat.Type)
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "TRD requires a forum supergroup (topics enabled). This chat is "+m.Chat.Type+".")
		return
	}
	if !d.isUserAllowed(user) {
		d.logger.Info("tg recv rejected: user not in allowlist", "user", user)
		return
	}
	text := strings.TrimSpace(rawText)

	// Strip bot mentions like "@mybot" from slash commands.
	if strings.HasPrefix(text, "/") {
		if idx := strings.Index(text, " "); idx > 0 {
			cmd := text[:idx]
			rest := text[idx+1:]
			if at := strings.Index(cmd, "@"); at > 0 {
				cmd = cmd[:at]
			}
			text = cmd + " " + rest
		} else if at := strings.Index(text, "@"); at > 0 {
			text = text[:at]
		}
	}

	switch {
	case strings.HasPrefix(text, "/start "):
		arg := strings.TrimSpace(strings.TrimPrefix(text, "/start"))
		d.cmdStart(ctx, m, arg)
	case text == "/start":
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Usage: /start <git-url>")
	case text == "/restart_dispatcher":
		d.cmdRestartDispatcher(ctx, m)
	case text == "/status":
		d.cmdStatus(ctx, m)
	case text == "/forget":
		d.cmdForget(ctx, m)
	case text == "/reset":
		d.cmdReset(ctx, m)
	case text == "/debug":
		d.cmdDebug(ctx, m)
	case text == "/help":
		d.cmdHelp(ctx, m)
	case text == "/cancel":
		d.cmdCancel(ctx, m)
	case text == "/model" || strings.HasPrefix(text, "/model "):
		d.cmdModel(ctx, m, strings.TrimSpace(strings.TrimPrefix(text, "/model")))
	case text == "/effort" || strings.HasPrefix(text, "/effort "):
		d.cmdEffort(ctx, m, strings.TrimSpace(strings.TrimPrefix(text, "/effort")))
	default:
		d.routeToInstance(ctx, m, text)
	}
}

// --- commands ---

func (d *Dispatcher) cmdStart(ctx context.Context, m *telegram.Message, repoURL string) {
	if repoURL == "" {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Usage: /start <git-url>\nAccepted formats:\n  git@github.com:org/repo.git\n  https://github.com/org/repo\n  github.com/org/repo")
		return
	}
	normalized, err := normalizeRepoURL(repoURL)
	if err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Invalid repo URL: "+err.Error())
		return
	}
	repoURL = normalized
	existing, err := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "internal error: "+err.Error())
		return
	}
	if existing != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
			fmt.Sprintf("This topic is already bound to %s (state=%s). Use /forget first.", existing.RepoURL, existing.State))
		return
	}

	// Verify omp is reachable on PATH (or under the override) so we can
	// fail fast at /start time instead of on the first user message.
	bin := d.ompBinary()
	if _, err := exec.LookPath(bin); err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
			fmt.Sprintf("%q not found on PATH. Install omp first (https://github.com/oh-my-pi/oh-my-pi).", bin))
		return
	}

	instID := uuid.NewString()
	reposDir, _ := config.ReposDir()
	repoPath := filepath.Join(reposDir, instID)

	sent, _ := d.sendMessage(ctx, telegram.SendMessageParams{
		ChatID:          m.Chat.ID,
		MessageThreadID: m.MessageThreadID,
		Text:            "Cloning " + repoURL + "…",
	})

	cloneCtx, cloneCancel := context.WithTimeout(ctx, 5*time.Minute)
	cloneDone := make(chan struct{})
	var cloneOut []byte
	var cloneErr error
	go func() {
		cloneOut, cloneErr = exec.CommandContext(cloneCtx, "git", "clone", repoURL, repoPath).CombinedOutput()
		close(cloneDone)
	}()

	if sent.MessageID != 0 {
		start := time.Now()
		ticker := time.NewTicker(10 * time.Second)
	loop:
		for {
			select {
			case <-cloneDone:
				break loop
			case <-ticker.C:
				elapsed := time.Since(start).Truncate(time.Second)
				_ = d.tg.EditMessageText(ctx, telegram.EditMessageTextParams{
					ChatID:    m.Chat.ID,
					MessageID: sent.MessageID,
					Text:      fmt.Sprintf("Cloning %s… (%s elapsed)", repoURL, elapsed),
				})
			}
		}
		ticker.Stop()
	} else {
		<-cloneDone
	}
	cloneCancel()

	if cloneErr != nil {
		_ = os.RemoveAll(repoPath)
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "clone failed:\n"+truncate(string(cloneOut), 1500))
		return
	}

	_ = config.EnsureGitignore(repoPath)

	inst := storage.Instance{
		InstanceID: instID,
		ChatID:     m.Chat.ID,
		TopicID:    m.MessageThreadID,
		RepoURL:    repoURL,
		RepoPath:   repoPath,
		RepoName:   storage.RepoNameFromURL(repoURL),
		State:      storage.StateRunning,
	}
	if err := d.store.Put(inst); err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "failed to persist: "+err.Error())
		return
	}
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
		fmt.Sprintf("Ready. Instance %s bound. Send a message to start the agent.", instID[:8]))
}


func (d *Dispatcher) cmdReset(ctx context.Context, m *telegram.Message) {
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
		return
	}
	_ = d.CancelRun(inst.InstanceID)
	inst.SessionID = ""
	inst.State = storage.StateRunning
	inst.FailCount = 0
	_ = d.store.Put(*inst)
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Reset — the next message will start a fresh agent session.")
}

// cmdRestartDispatcher triggers a dispatcher-wide graceful restart from the
// bound topic. The topic's instance must be flagged as the controller
// (set via `trd promote`) — this is the same authorisation gate that
// /api/restart enforces.
//
// User-visible behaviour: the in-flight run that issued this command
// (if any) is allowed to finish naturally; queued prompts are
// persisted; then the process re-execs in place. A "restart complete"
// confirmation lands in a fresh run after the successor process boots.
func (d *Dispatcher) cmdRestartDispatcher(ctx context.Context, m *telegram.Message) {
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
		return
	}
	if !inst.Controller {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
			"this topic's instance is not the controller — refusing /restart_dispatcher.\n"+
				"Run `trd promote "+inst.RepoName+"` on the host to authorise.")
		return
	}
	if err := d.RequestRestart(inst.InstanceID); err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "restart failed: "+err.Error())
		return
	}
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
		"Restart accepted. Draining in-flight runs, then re-execing the dispatcher in place.")
}

func (d *Dispatcher) cmdHelp(ctx context.Context, m *telegram.Message) {
	help := `TRD — Telegram Repo Dispatcher (omp headless mode)

/start <git-url> — Clone a repo and bind it to this topic
/reset — Forget the session id; next message starts fresh
/status — Show instance, session, and run state
/model [name] — Show or change the model (e.g. /model opus)
/effort [level] — Show or change thinking level (minimal, low, medium, high, xhigh)
/cancel — Interrupt the in-flight agent run for this topic
/debug — Toggle dispatcher debug logging
/forget — Delete the topic-repo mapping
/help — Show this message
/restart_dispatcher — (controller-only) Drain in-flight runs and re-exec the dispatcher in place

Anything else you type spawns omp -p in the bound repo.`
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID, help)
}

var validModels = map[string]bool{
	// Loose whitelist for the most common fuzzy-match aliases. omp will
	// further resolve the value at spawn time.
	"opus": true, "sonnet": true, "haiku": true,
	"gpt-5": true, "gpt-5.1": true, "gpt-5.2": true,
	"claude-opus-4-7": true, "claude-sonnet-4-5": true,
}

var validThinking = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

func (d *Dispatcher) cmdModel(ctx context.Context, m *telegram.Message, arg string) {
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
		return
	}
	cfg, _ := config.ReadAgentConfig(inst.RepoPath)
	if arg == "" {
		current := cfg.Model
		if current == "" {
			current = "(omp default)"
		}
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
			fmt.Sprintf("Current model: %s\nUsage: /model <name>  (e.g. opus, sonnet, haiku, gpt-5.2)\n/model -  resets to omp's default.", current))
		return
	}
	if arg == "-" {
		if err := config.WriteAgentField(inst.RepoPath, "model", ""); err != nil {
			d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "failed: "+err.Error())
			return
		}
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Model cleared — omp will use its own default on the next message.")
		return
	}
	// Light validation: warn but still accept anything containing letters
	// so omp's fuzzy match (e.g. "anthropic/claude-opus-4-7") keeps working.
	if !validModels[strings.ToLower(arg)] && !looksLikeModelName(arg) {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "That doesn't look like a model name. Try opus, sonnet, haiku, gpt-5.2, …")
		return
	}
	if err := config.WriteAgentField(inst.RepoPath, "model", arg); err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "failed: "+err.Error())
		return
	}
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
		fmt.Sprintf("Model set to %q. Takes effect on the next message.", arg))
}

func (d *Dispatcher) cmdEffort(ctx context.Context, m *telegram.Message, arg string) {
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
		return
	}
	cfg, _ := config.ReadAgentConfig(inst.RepoPath)
	if arg == "" {
		current := cfg.Thinking
		if current == "" {
			current = "(omp default)"
		}
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
			fmt.Sprintf("Current thinking level: %s\nOptions: minimal, low, medium, high, xhigh.\n/effort -  resets to omp's default.", current))
		return
	}
	if arg == "-" {
		if err := config.WriteAgentField(inst.RepoPath, "thinking", ""); err != nil {
			d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "failed: "+err.Error())
			return
		}
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Thinking level cleared — omp will use its own default.")
		return
	}
	low := strings.ToLower(arg)
	if !validThinking[low] {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Unknown thinking level. Options: minimal, low, medium, high, xhigh.")
		return
	}
	if err := config.WriteAgentField(inst.RepoPath, "thinking", low); err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "failed: "+err.Error())
		return
	}
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
		fmt.Sprintf("Thinking level set to %s. Takes effect on the next message.", low))
}

func (d *Dispatcher) cmdDebug(ctx context.Context, m *telegram.Message) {
	d.opts.Debug = !d.opts.Debug
	state := "OFF"
	if d.opts.Debug {
		state = "ON"
	}
	d.logger.Info("debug mode toggled", "debug", d.opts.Debug)
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID,
		fmt.Sprintf("Debug mode: %s.", state))
}

func (d *Dispatcher) cmdStatus(ctx context.Context, m *telegram.Message) {
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
		return
	}
	d.runMu.Lock()
	run, running := d.runs[inst.InstanceID]
	d.runMu.Unlock()
	cfg, _ := config.ReadAgentConfig(inst.RepoPath)

	session := inst.SessionID
	if session == "" {
		session = "(none — next message starts fresh)"
	} else if len(session) > 8 {
		session = session[:8] + "…"
	}
	runLine := "no"
	if running {
		runLine = fmt.Sprintf("yes (started %s ago, msg_id=%d)", time.Since(run.started).Truncate(time.Second), run.msgID)
	}
	model := cfg.Model
	if model == "" {
		model = "(omp default)"
	}
	thinking := cfg.Thinking
	if thinking == "" {
		thinking = "(omp default)"
	}

	msg := fmt.Sprintf(
		"instance: %s\nrepo: %s\npath: %s\nstate: %s\nsession: %s\nrun in flight: %s\nmodel: %s\nthinking: %s\nfail_count: %d",
		inst.InstanceID[:8], inst.RepoURL, inst.RepoPath, inst.State, session, runLine, model, thinking, inst.FailCount,
	)
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID, msg)
}

func (d *Dispatcher) cmdForget(ctx context.Context, m *telegram.Message) {
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
		return
	}
	_ = d.CancelRun(inst.InstanceID)
	if err := d.store.Delete(inst.InstanceID); err != nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "forget failed: "+err.Error())
		return
	}
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "forgotten. repo files at "+inst.RepoPath+" kept on disk.")
}

func (d *Dispatcher) cmdCancel(ctx context.Context, m *telegram.Message) {
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no instance bound to this topic")
		return
	}
	d.runMu.Lock()
	_, running := d.runs[inst.InstanceID]
	d.runMu.Unlock()
	if !running {
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "no run in flight.")
		return
	}
	_ = d.CancelRun(inst.InstanceID)
	d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "Cancel requested.")
}

// routeToInstance is the hot path for ordinary (non-command) messages.
// It looks up the bound instance, builds the prompt (text + voice
// transcript + attachment notes), and queues or immediately spawns omp.
func (d *Dispatcher) routeToInstance(ctx context.Context, m *telegram.Message, text string) {
	inst, err := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if err != nil {
		d.logger.Warn("route: ByTopic lookup failed",
			"chat", m.Chat.ID, "thread", m.MessageThreadID, "err", err)
		return
	}
	if inst == nil {
		d.logger.Debug("route: no instance bound to topic — ignoring",
			"chat", m.Chat.ID, "thread", m.MessageThreadID)
		return
	}
	if inst.State != storage.StateRunning {
		d.logger.Info("route: instance not running",
			"instance", shortID(inst.InstanceID), "state", inst.State)
		d.sendText(ctx, m.Chat.ID, m.MessageThreadID, "instance state is "+string(inst.State)+"; use /restart")
		return
	}
	user := ""
	if m.From != nil {
		user = m.From.Username
		if user == "" {
			user = m.From.FirstName
		}
	}

	prompt := d.buildPrompt(ctx, m, text)
	if prompt == "" {
		d.logger.Info("route: empty prompt after attachments — skipping",
			"instance", shortID(inst.InstanceID))
		return
	}

	d.logger.Info("tg->agent forward",
		"instance", shortID(inst.InstanceID),
		"chat", m.Chat.ID, "thread", m.MessageThreadID, "msg_id", m.MessageID,
		"from", user, "text_len", len(prompt), "text", preview(prompt),
	)

	d.enqueueOrRun(*inst, queuedPrompt{
		chatID: m.Chat.ID,
		thread: m.MessageThreadID,
		msgID:  m.MessageID,
		user:   user,
		text:   prompt,
	})
}

// buildPrompt merges the user's text, any voice transcript, and any
// attachment metadata into a single prompt string the agent receives.
// Attachments are surfaced as a trailing "[attachment: <name> (<path>)]"
// line so the agent can read them directly from disk via its tools.
func (d *Dispatcher) buildPrompt(ctx context.Context, m *telegram.Message, text string) string {
	var parts []string
	if text != "" {
		parts = append(parts, text)
	}

	switch {
	case m.Voice != nil:
		if d.media.CanTranscribe() {
			if t := d.transcribeAttachment(ctx, m.Voice.FileID); t != "" {
				parts = append(parts, t)
			}
		}
	case m.Audio != nil:
		if d.media.CanTranscribe() {
			if t := d.transcribeAttachment(ctx, m.Audio.FileID); t != "" {
				parts = append(parts, t)
			}
		}
	case m.Document != nil:
		if path, err := d.tg.DownloadFile(ctx, m.Document.FileID, d.opts.AttachDir); err == nil {
			parts = append(parts, fmt.Sprintf("[attachment: %s (%s)]", m.Document.FileName, path))
		}
	case len(m.Photo) > 0:
		ph := m.Photo[len(m.Photo)-1]
		if path, err := d.tg.DownloadFile(ctx, ph.FileID, d.opts.AttachDir); err == nil {
			parts = append(parts, fmt.Sprintf("[attachment: photo (%s)]", path))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// enqueueOrRun spawns the agent immediately if no run is in flight for
// this instance, otherwise FIFO-queues the prompt. Holds runMu only long
// enough to check or mutate the maps; driveAgentRun does its own locking
// when it later finishes.
func (d *Dispatcher) enqueueOrRun(inst storage.Instance, p queuedPrompt) {
	// Stage 1 of the two-mark visibility chain: the dispatcher acknowledges
	// reception immediately, before any omp work. Stage 2 (👍) is set by the
	// agent itself via tg_react once the LLM has actually seen the message.
	// Together they let the user distinguish "system got it" from
	// "model started thinking".
	go d.sendReaction(inst.ChatID, p.msgID, "👀")

	// Restart drain in progress: persist the prompt to bbolt so the
	// successor process picks it up post-exec. The 👀 above already
	// shipped (system received it); the 👍 will land in the successor
	// once that turn's omp boots.
	if d.pendingRestart.Load() {
		if err := d.store.EnqueueDeferred(storage.DeferredPrompt{
			InstanceID: inst.InstanceID,
			ChatID:     p.chatID,
			ThreadID:   p.thread,
			MsgID:      p.msgID,
			User:       p.user,
			Text:       p.text,
		}); err != nil {
			d.logger.Warn("restart: defer incoming prompt failed",
				"instance", shortID(inst.InstanceID), "msg_id", p.msgID, "err", err)
		} else {
			d.logger.Info("restart: prompt deferred for successor",
				"instance", shortID(inst.InstanceID), "msg_id", p.msgID)
		}
		return
	}

	d.runMu.Lock()
	if _, busy := d.runs[inst.InstanceID]; busy {
		d.pendingQueue[inst.InstanceID] = append(d.pendingQueue[inst.InstanceID], p)
		d.runMu.Unlock()
		return
	}
	runCtx, cancel := context.WithTimeout(context.Background(), d.opts.RunTimeout)
	run := &agentRun{
		instanceID: inst.InstanceID,
		cancel:     cancel,
		chatID:     p.chatID,
		thread:     p.thread,
		msgID:      p.msgID,
		started:    time.Now(),
	}
	d.runs[inst.InstanceID] = run
	d.runMu.Unlock()

	d.runWG.Add(1)
	go func() {
		defer d.runWG.Done()
		d.driveAgentRun(runCtx, inst, run, p)
	}()
}

// driveAgentRun is the per-message worker goroutine. It spawns omp, ranges
// over the event stream, accumulates assistant text, captures the session
// id, and (on completion) sends the final reply to Telegram. After the
// channel closes it calls finishRun, which may immediately dispatch the
// next queued prompt for this instance.
func (d *Dispatcher) driveAgentRun(ctx context.Context, inst storage.Instance, run *agentRun, p queuedPrompt) {
	defer d.finishRun(inst.InstanceID)

	agentCfg, _ := config.ReadAgentConfig(inst.RepoPath)
	logPath, _ := config.InstanceLogPath(inst.InstanceID)

	var extensions []string
	var sysPromptAppend string
	var extraEnv []string
	if d.extPath != "" {
		extensions = []string{d.extPath}
		sysPromptAppend = extension.SystemPromptAppend
		extraEnv = []string{
			fmt.Sprintf("TRD_CHAT_ID=%d", p.chatID),
			fmt.Sprintf("TRD_THREAD_ID=%d", p.thread),
			fmt.Sprintf("TRD_MESSAGE_ID=%d", p.msgID),
			fmt.Sprintf("TRD_DISPATCHER_URL=http://127.0.0.1:%d", d.opts.Port),
			fmt.Sprintf("TRD_INSTANCE_ID=%s", inst.InstanceID),
			fmt.Sprintf("TRD_TTS_AVAILABLE=%t", d.canSynthesize()),
		}
	}
	d.logger.Info("agent spawn",
		"instance", shortID(inst.InstanceID),
		"msg_id", p.msgID,
		"extension_loaded", d.extPath != "",
		"extension_path", d.extPath,
		"sys_prompt_chars", len(sysPromptAppend),
		"env_vars", len(extraEnv),
		"tts_available", d.canSynthesize())

	h, err := d.runner.Start(ctx, agent.RunOptions{
		Cwd:                inst.RepoPath,
		SessionID:          inst.SessionID,
		Model:              agentCfg.Model,
		Thinking:           agentCfg.Thinking,
		Prompt:             p.text,
		LogPath:            logPath,
		Binary:             d.opts.OMPBinary,
		Extensions:         extensions,
		AppendSystemPrompt: sysPromptAppend,
		ExtraEnv:           extraEnv,
	})
	if err != nil {
		d.logger.Error("agent.Start failed",
			"instance", shortID(inst.InstanceID), "err", err)
		d.sendText(ctx, p.chatID, p.thread, "agent spawn failed: "+err.Error())
		return
	}

	d.runMu.Lock()
	run.handle = h
	d.runMu.Unlock()

	runStartedAt := time.Now()
	d.logger.Info("agent run started",
		"instance", shortID(inst.InstanceID),
		"msg_id", p.msgID,
		"resume", inst.SessionID != "")

	var (
		stream      *streamingReply // nil until first delta — lazy so a
		                            // deltaless turn keeps the historic
		                            // one-shot send path
		finalBuf    strings.Builder // accumulates the text content of every
		                            // EvAssistantFinal in this turn. omp
		                            // emits one per assistant segment, so a
		                            // turn with tool calls fires multiple
		                            // (text → tool → text → tool → text…).
		                            // Overwriting would leave us with just
		                            // the last segment — see the bug from
		                            // 2026-05-28 where Finalize replaced
		                            // the streamed reply with "just the
		                            // last chunk".
		segments    int
		sawFinal    bool
		hadError    bool
		errText     string
	)

	for ev := range h.Events() {
		switch ev.Kind {
		case agent.EvSessionID:
			if inst.SessionID == "" && ev.SessionID != "" {
				inst.SessionID = ev.SessionID
				if err := d.store.Put(inst); err != nil {
					d.logger.Warn("persist session id failed",
						"instance", shortID(inst.InstanceID), "err", err)
				} else {
					d.logger.Info("captured session id",
						"instance", shortID(inst.InstanceID),
						"session", shortID(ev.SessionID))
				}
			}
		case agent.EvAssistantDelta:
			if stream == nil {
				stream = newStreamingReply(
					d.sendMessage, d.editMessage,
					p.chatID, p.thread, p.msgID,
					d.logger, shortID(inst.InstanceID),
				)
			}
			stream.Append(ev.Text)
		case agent.EvAssistantFinal:
			finalBuf.WriteString(ev.Text)
			segments++
			sawFinal = true
			d.logger.Info("agent: final segment",
				"instance", shortID(inst.InstanceID),
				"segment", segments, "len", len(ev.Text))
		case agent.EvToolCall:
			d.logger.Info("agent: tool call",
				"instance", shortID(inst.InstanceID), "tool", ev.Text)
		case agent.EvError:
			if sawFinal {
				// omp already produced its canonical message_end; the only
				// EvError we see now is its stdout teardown noise (EPIPE,
				// truncated agent_end frame). Log it for forensics but do
				// NOT flip hadError — the reply is already correct and we
				// must not append "(agent reported: …)" to a successful
				// turn. See DEBUG.md § "EPIPE on agent_end polluting clean
				// replies".
				d.logger.Warn("agent error event (post_final, suppressed from reply)",
					"instance", shortID(inst.InstanceID), "text", ev.Text)
				continue
			}
			hadError = true
			errText = ev.Text
			d.logger.Warn("agent error event",
				"instance", shortID(inst.InstanceID), "text", ev.Text)
		case agent.EvDone:
			// noop — channel will close next.
		}
	}

	// Compose the user-visible reply.
	//
	// Streaming path: stream.Streamed() is what the user has been
	// watching grow on Telegram. It's already the right canonical text;
	// finalBuf (the concatenation of every message_end segment) should
	// equal it, but we don't trust that equality — if omp emitted a
	// retry mid-stream or our deltas missed a few bytes from a partial
	// scanner read, the streamed text is what the user saw, so honour
	// that. Falling back to finalBuf only when streaming never started.
	//
	// Non-streaming path (no deltas ever arrived): use finalBuf as the
	// only source of canonical text.
	var reply string
	switch {
	case stream != nil:
		reply = stream.Streamed()
	default:
		reply = finalBuf.String()
	}
	if hadError && reply == "" {
		reply = "agent error: " + errText
	} else if hadError {
		reply = reply + "\n\n(agent reported: " + errText + ")"
	}
	if reply == "" {
		if !sawFinal {
			reply = "(agent produced no reply — see /watch for details)"
		} else {
			// sawFinal=true with empty text — historical no-op return
			// (used when an upstream agent intentionally suppresses
			// the reply). Close the stream so no late edit fires.
			if stream != nil {
				stream.Close()
			}
			d.logger.Info("agent run finished (empty reply, no-op)",
				"instance", shortID(inst.InstanceID),
				"segments", segments)
			return
		}
	}

	bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Log on every exit path (Finalize OR splitMessage send), pulling
	// the values captured up to this point. defer here so the message
	// shows up after every reply branch including early returns added
	// later.
	defer func() {
		d.logger.Info("agent run finished",
			"instance", shortID(inst.InstanceID),
			"msg_id", p.msgID,
			"duration", time.Since(runStartedAt).Round(time.Millisecond),
			"reply_chars", len(reply),
			"segments", segments,
			"streamed", stream != nil,
			"had_error", hadError)
	}()

	if stream != nil {
		// Streaming path: finalize the in-flight message with the
		// canonical text (possibly rolling over into more messages if
		// the final text is longer than what we streamed).
		if err := stream.Finalize(bgCtx, reply); err != nil {
			d.logger.Warn("stream finalize failed",
				"instance", shortID(inst.InstanceID), "err", err)
		}
		return
	}

	// Non-streaming path: no deltas ever arrived. Send the reply as a
	// fresh message (or multiple, when it exceeds the chunk size).
	for i, chunk := range splitMessage(reply, 4000) {
		replyTo := 0
		if i == 0 {
			replyTo = p.msgID
		}
		_, err := d.sendMessage(bgCtx, telegram.SendMessageParams{
			ChatID:           p.chatID,
			MessageThreadID:  p.thread,
			Text:             chunk,
			ReplyToMessageID: replyTo,
		})
		if err != nil {
			d.logger.Warn("send reply chunk failed",
				"instance", shortID(inst.InstanceID), "chunk", i, "err", err)
		}
	}
}

// finishRun removes the in-flight handle, then if more prompts are queued
// for this instance, dispatches the next one immediately. Reloads the
// Instance from bbolt so any SessionID written during the just-finished
// run is visible.
func (d *Dispatcher) finishRun(instanceID string) {
	d.runMu.Lock()
	if run, ok := d.runs[instanceID]; ok {
		if run.cancel != nil {
			run.cancel()
		}
	}
	delete(d.runs, instanceID)
	// Restart drain: when this was the last active run and a restart is
	// pending, cancel Run so cmdStart can re-exec. RequestRestart has
	// already flushed pendingQueue to bbolt, so we don't redispatch from
	// the local map below.
	if d.pendingRestart.Load() && len(d.runs) == 0 {
		d.runMu.Unlock()
		d.triggerRestartStop()
		return
	}
	queue := d.pendingQueue[instanceID]
	if len(queue) == 0 {
		d.runMu.Unlock()
		return
	}
	next := queue[0]
	d.pendingQueue[instanceID] = queue[1:]
	d.runMu.Unlock()

	inst, err := d.store.Get(instanceID)
	if err != nil || inst == nil {
		d.logger.Warn("finishRun: instance vanished during queue drain",
			"instance", shortID(instanceID), "err", err)
		return
	}
	if inst.State != storage.StateRunning {
		d.logger.Info("finishRun: instance stopped; dropping queued prompts",
			"instance", shortID(instanceID), "queued", len(queue))
		return
	}
	d.enqueueOrRun(*inst, next)
}

// handleEditedMessage forwards an edited Telegram message to the bound
// instance so the user's corrections aren't silently lost.
func (d *Dispatcher) handleEditedMessage(ctx context.Context, m *telegram.Message) {
	if m.Chat.Type != "supergroup" || !m.Chat.IsForum {
		return
	}
	user := ""
	if m.From != nil {
		user = m.From.Username
		if user == "" {
			user = m.From.FirstName
		}
	}
	if !d.isUserAllowed(user) {
		return
	}
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	if text == "" {
		return
	}
	inst, _ := d.store.ByTopic(m.Chat.ID, m.MessageThreadID)
	if inst == nil || inst.State != storage.StateRunning {
		return
	}
	d.logger.Info("tg edited->agent",
		"instance", shortID(inst.InstanceID), "msg_id", m.MessageID, "from", user)
	d.enqueueOrRun(*inst, queuedPrompt{
		chatID: m.Chat.ID,
		thread: m.MessageThreadID,
		msgID:  m.MessageID,
		user:   user,
		text:   fmt.Sprintf("(edited) %s", text),
	})
	_ = ctx
}

// sendReaction is best-effort — failures get logged but never propagated.
func (d *Dispatcher) sendReaction(chatID int64, msgID int, emoji string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.setReaction(ctx, chatID, msgID, emoji); err != nil {
		d.logger.Debug("reaction failed", "chat", chatID, "msg_id", msgID, "err", err)
	}
}

// sendText is best-effort: failures get logged but never propagated. Used
// for every dispatcher-to-Telegram interaction.
func (d *Dispatcher) sendText(ctx context.Context, chatID int64, threadID int, text string) {
	_, err := d.sendMessage(ctx, telegram.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            text,
	})
	if err != nil {
		d.logger.Warn("sendText failed", "err", err)
	}
}

// transcribeAttachment downloads a Telegram file and runs Whisper on it.
// Returns the transcript, or empty string on any failure (logged but not
// fatal). Cleans up sidecar files the whisper CLI may create.
func (d *Dispatcher) transcribeAttachment(ctx context.Context, fileID string) string {
	path, err := d.tg.DownloadFile(ctx, fileID, d.opts.AttachDir)
	if err != nil {
		d.logger.Warn("whisper: download failed", "file_id", fileID, "err", err)
		return ""
	}
	transcript, err := d.media.Transcribe(ctx, path)
	if err != nil {
		d.logger.Warn("whisper: transcription failed", "path", path, "err", err)
		return ""
	}
	base := strings.TrimSuffix(path, filepath.Ext(path))
	for _, ext := range []string{".txt", ".vtt", ".srt", ".json", ".tsv"} {
		sidecar := base + ext
		if err := os.Remove(sidecar); err == nil {
			d.logger.Debug("whisper: cleaned up sidecar", "path", sidecar)
		}
	}
	d.logger.Info("whisper: transcribed", "path", path, "len", len(transcript))
	return transcript
}

// findInstance resolves an instance by repo name or ID prefix. Same logic
// as before; used by CancelRun.
func (d *Dispatcher) findInstance(query string) (*storage.Instance, error) {
	all, err := d.store.All()
	if err != nil {
		return nil, err
	}
	var byName []storage.Instance
	for _, inst := range all {
		name := inst.RepoName
		if name == "" {
			name = storage.RepoNameFromURL(inst.RepoURL)
		}
		if strings.EqualFold(name, query) || strings.HasPrefix(strings.ToLower(name), strings.ToLower(query)) {
			byName = append(byName, inst)
		}
	}
	if len(byName) == 1 {
		return &byName[0], nil
	}
	if len(byName) > 1 {
		return nil, fmt.Errorf("%d instances match %q", len(byName), query)
	}
	for _, inst := range all {
		if strings.HasPrefix(inst.InstanceID, query) {
			return &inst, nil
		}
	}
	return nil, fmt.Errorf("no instance matching %q", query)
}

// ompBinary resolves the omp executable per the precedence rule defined
// in agent.Start (Options.OMPBinary > $TRD_OMP_BIN > "omp"). Centralised
// here so cmdStart can fail fast with a clear error.
func (d *Dispatcher) ompBinary() string {
	if d.opts.OMPBinary != "" {
		return d.opts.OMPBinary
	}
	return firstNonEmpty(os.Getenv("TRD_OMP_BIN"), "omp")
}

// --- pure helpers ---

// splitMessage breaks a long text into chunks of at most maxLen characters,
// splitting at newline boundaries when possible. Identical to the
// pre-port helper; carried over because Telegram's 4096 char limit is
// independent of the agent backend.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		cut := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n"); idx > maxLen/2 {
			cut = idx + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}

// normalizeRepoURL accepts three URL formats and converts to SSH:
//
//	git@host:org/repo.git   → pass through
//	https://host/org/repo   → git@host:org/repo.git
//	host/org/repo           → git@host:org/repo.git
//
// Rejects flag-like input (starts with -) and URLs that don't look like
// a valid repo path.
func normalizeRepoURL(raw string) (string, error) {
	if strings.HasPrefix(raw, "-") {
		return "", fmt.Errorf("URL must not start with a dash")
	}

	if strings.HasPrefix(raw, "git@") {
		if !strings.Contains(raw[4:], ":") {
			return "", fmt.Errorf("SSH URL missing colon: %q", raw)
		}
		if !strings.HasSuffix(raw, ".git") {
			raw += ".git"
		}
		return raw, nil
	}

	u := raw
	if after, ok := strings.CutPrefix(u, "https://"); ok {
		u = after
	} else if after, ok := strings.CutPrefix(u, "http://"); ok {
		u = after
	}

	parts := strings.SplitN(u, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("expected format: host/org/repo, got %q", raw)
	}
	host := parts[0]
	path := parts[1] + "/" + parts[2]
	path = strings.TrimSuffix(path, ".git")

	return fmt.Sprintf("git@%s:%s.git", host, path), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// shortID returns the first 8 chars of an instance id for compact log
// output. Identifiers shorter than that are returned unchanged.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// preview returns a single-line, length-capped sample of s suitable for
// logs. Collapses newlines so structured log records stay on one line.
func preview(s string) string {
	const max = 200
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// looksLikeModelName allows free-form names that contain a letter so
// fuzzy-match aliases like "anthropic/claude-opus-4-7" still work without
// hard-coding every provider/model.
func looksLikeModelName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// tailFile returns the last maxLines lines of path, capped at maxBytes
// bytes. Reads the whole file (these logs are bounded by run frequency,
// not size, and run.LogPath rotates on per-instance deletion). Returns
// os.ErrNotExist when the path is absent.
func tailFile(path string, maxLines, maxBytes int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
	}
	return out, nil
}
