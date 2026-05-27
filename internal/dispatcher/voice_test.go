package dispatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pomofomo/multi-omp-tg/internal/telegram"
)

// voiceFixture wires SendVoiceMemo's three seams (canSynthesize,
// synthesize, sendVoice) plus AttachDir so the dispatcher can do its
// real synthesize→upload→cleanup choreography against fakes.
type voiceFixture struct {
	d            *Dispatcher
	attachDir    string
	synthesized  atomic.Int32 // synth calls
	uploaded     atomic.Int32 // sendVoice calls
	lastText     atomic.Value // string passed to synth
	lastUpload   atomic.Value // voiceUpload recorded on send
	syntherError error        // optional injected synth error
	uploadError  error        // optional injected upload error
}

type voiceUpload struct {
	chatID   int64
	threadID int
	replyTo  int
	path     string
}

// newVoiceFixture returns a Dispatcher with the voice seams wired to
// recorders. AttachDir is a temp dir; the fake synthesize creates a
// stub OGG inside it so the deferred Remove in SendVoiceMemo has a real
// file to delete.
func newVoiceFixture(t *testing.T) *voiceFixture {
	t.Helper()
	d, _ := newTestDispatcher(t, &fakeRunner{})

	attachDir := t.TempDir()
	d.opts.AttachDir = attachDir

	fx := &voiceFixture{d: d, attachDir: attachDir}

	d.canSynthesize = func() bool { return true }
	d.synthesize = func(_ context.Context, text, outDir string) (string, error) {
		fx.synthesized.Add(1)
		fx.lastText.Store(text)
		if fx.syntherError != nil {
			return "", fx.syntherError
		}
		// Write a stub OGG so cleanup observes a real file. Path uses
		// time-prefixed name to mimic the real synthesizer.
		p := filepath.Join(outDir, "tts-test.ogg")
		if err := os.WriteFile(p, []byte("fake ogg bytes"), 0o600); err != nil {
			t.Fatalf("stub synth write: %v", err)
		}
		return p, nil
	}
	d.sendVoice = func(_ context.Context, chatID int64, threadID, replyTo int, path string, _ string) (telegram.Message, error) {
		fx.uploaded.Add(1)
		fx.lastUpload.Store(voiceUpload{chatID: chatID, threadID: threadID, replyTo: replyTo, path: path})
		if fx.uploadError != nil {
			return telegram.Message{}, fx.uploadError
		}
		return telegram.Message{MessageID: 4242}, nil
	}
	return fx
}

func TestSendVoiceMemoSynthesizesAndUploads(t *testing.T) {
	fx := newVoiceFixture(t)

	err := fx.d.SendVoiceMemo(-1001, 42, 99, "hello, world")
	if err != nil {
		t.Fatalf("SendVoiceMemo: %v", err)
	}
	if fx.synthesized.Load() != 1 {
		t.Errorf("synthesize: want 1 call, got %d", fx.synthesized.Load())
	}
	if fx.uploaded.Load() != 1 {
		t.Errorf("sendVoice: want 1 call, got %d", fx.uploaded.Load())
	}
	if got := fx.lastText.Load().(string); got != "hello, world" {
		t.Errorf("synthesize text: got %q", got)
	}
	up := fx.lastUpload.Load().(voiceUpload)
	if up.chatID != -1001 || up.threadID != 42 || up.replyTo != 99 {
		t.Errorf("upload payload: %+v", up)
	}
	if !strings.HasPrefix(up.path, fx.attachDir) {
		t.Errorf("upload path not under AttachDir: %s", up.path)
	}
}

func TestSendVoiceMemoCleansUpTempFile(t *testing.T) {
	fx := newVoiceFixture(t)

	if err := fx.d.SendVoiceMemo(1, 0, 0, "speakable"); err != nil {
		t.Fatalf("SendVoiceMemo: %v", err)
	}
	up := fx.lastUpload.Load().(voiceUpload)
	if _, err := os.Stat(up.path); !os.IsNotExist(err) {
		t.Errorf("temp ogg not cleaned up at %s (err=%v)", up.path, err)
	}
}

func TestSendVoiceMemoRejectsEmptyInputs(t *testing.T) {
	fx := newVoiceFixture(t)

	cases := []struct {
		name   string
		chatID int64
		text   string
	}{
		{"no chat id", 0, "hello"},
		{"empty text", 1, ""},
		{"whitespace text", 1, "   \t\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fx.d.SendVoiceMemo(tc.chatID, 0, 0, tc.text)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if fx.synthesized.Load() != 0 || fx.uploaded.Load() != 0 {
				t.Errorf("synth/upload should not run on validation failure: synth=%d up=%d",
					fx.synthesized.Load(), fx.uploaded.Load())
			}
		})
	}
}

func TestSendVoiceMemoFailsWhenTTSUnavailable(t *testing.T) {
	fx := newVoiceFixture(t)
	fx.d.canSynthesize = func() bool { return false }

	err := fx.d.SendVoiceMemo(1, 0, 0, "anything")
	if err == nil {
		t.Fatal("expected error when TTS is disabled, got nil")
	}
	if !strings.Contains(err.Error(), "TTS is not configured") {
		t.Errorf("error message should mention TTS config: %v", err)
	}
	if fx.synthesized.Load() != 0 || fx.uploaded.Load() != 0 {
		t.Errorf("synth/upload should not run: synth=%d up=%d",
			fx.synthesized.Load(), fx.uploaded.Load())
	}
}

func TestSendVoiceMemoSurfacesSynthError(t *testing.T) {
	fx := newVoiceFixture(t)
	fx.syntherError = errors.New("sherpa exploded")

	err := fx.d.SendVoiceMemo(1, 0, 0, "boom")
	if err == nil || !strings.Contains(err.Error(), "sherpa exploded") {
		t.Fatalf("expected wrapped synth error, got %v", err)
	}
	if fx.uploaded.Load() != 0 {
		t.Errorf("upload should not run after synth failure")
	}
}

func TestSendVoiceMemoSurfacesUploadError(t *testing.T) {
	fx := newVoiceFixture(t)
	fx.uploadError = errors.New("telegram 429")

	err := fx.d.SendVoiceMemo(1, 0, 0, "still spoken")
	if err == nil || !strings.Contains(err.Error(), "telegram 429") {
		t.Fatalf("expected wrapped upload error, got %v", err)
	}
	// Even on upload failure the temp file must be cleaned up so
	// repeated failures don't fill the attachments dir.
	up := fx.lastUpload.Load().(voiceUpload)
	if _, err := os.Stat(up.path); !os.IsNotExist(err) {
		t.Errorf("temp ogg should be cleaned up on upload failure: %v", err)
	}
}
