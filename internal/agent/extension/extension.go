// Package extension ships an embedded TypeScript file that omp loads via
// `--extension <path>` so the agent can call back into the dispatcher.
//
// The single tool currently exposed is `tg_react`, which lets the agent
// add an emoji reaction to the Telegram message that triggered the run.
// The system prompt (see SystemPromptAppend) instructs the agent to use
// it immediately on every new message so the user gets a visible "got
// it" before omp starts thinking.
//
// Distribution: the .ts source is embedded into the Go binary and written
// to <trd-root>/ext/tg.ts on dispatcher startup (idempotent — only
// rewrites when the on-disk content drifts from the embedded blob).
package extension

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed tg.ts
var tgSource []byte

// SystemPromptAppend is the snippet the dispatcher passes to omp via
// --append-system-prompt on every spawn. Keep it short and unambiguous —
// it sits on top of every system prompt the agent already has.
//
// The embedded newline matters: omp's --append-system-prompt resolver
// will try to read the value as a file path first, and skips the read
// when the string contains a newline. Keeps a stat() out of the hot path.
const SystemPromptAppend = "You are running inside TRD (Telegram Repo Dispatcher). " +
	"Each user turn is a Telegram message in a forum topic bound to this repo.\n\n" +
	"## Interaction patterns\n\n" +
	"1. ACKNOWLEDGE: When you receive a new message, immediately react with " +
	"👍 on it (using the tg_react tool with emoji \"👍\") BEFORE you start " +
	"processing it. This confirms to the sender that the message was " +
	"received, especially important for voice messages which arrive after " +
	"transcription delay. A 👍 is also fired automatically at agent_start " +
	"as a backstop — a duplicate tg_react(\"👍\") call from you is " +
	"harmless, Telegram dedupes identical reactions.\n\n" +
	"2. REPLY WHEN DONE: Always send a reply message when you finish a " +
	"task, summarising what was done. The user is not watching your " +
	"screen — they only see Telegram messages.\n\n" +
	"3. CHANGE THE EMOJI ON COMPLETION: When the work is finished, you " +
	"MAY call tg_react again with a different emoji that better reflects " +
	"the outcome (e.g. 🎉 on a clear success, 😅 on a soft failure, ❌ on " +
	"a hard failure). Skip this if 👍 still fits.\n\n" +
	"4. ASK QUESTIONS: If the request is ambiguous or you need " +
	"clarification, stop and ask questions via reply before proceeding. " +
	"Do not guess at unclear requirements."

// Path returns the absolute path of the embedded extension under root.
// root is typically ~/.trd. The file is written eagerly when missing or
// stale.
func Path(root string) string {
	return filepath.Join(root, "ext", "tg.ts")
}

// Ensure writes the embedded extension to <root>/ext/tg.ts when the file
// is missing or its content drifts from the binary. Returns the absolute
// path callers should pass to omp via --extension.
//
// Idempotent: a no-op when the on-disk content already matches.
func Ensure(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("extension: root is required")
	}
	dst := Path(root)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("extension: mkdir %s: %w", dir, err)
	}

	existing, err := os.ReadFile(dst)
	if err == nil && bytes.Equal(existing, tgSource) {
		return dst, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("extension: read %s: %w", dst, err)
	}
	if err := os.WriteFile(dst, tgSource, 0o644); err != nil {
		return "", fmt.Errorf("extension: write %s: %w", dst, err)
	}
	return dst, nil
}

// Source exposes the embedded extension contents for tests.
func Source() []byte { return tgSource }
