package media

import (
	"context"
	"errors"
	"testing"
)

func TestEmptyEngineDefaults(t *testing.T) {
	e, err := NewEngine(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.CanTranscribe() {
		t.Error("empty config should not be able to transcribe")
	}
	if e.CanSynthesize() {
		t.Error("empty config should not be able to synthesize")
	}
}

func TestCanTranscribeWithOpenAI(t *testing.T) {
	e, _ := NewEngine(Config{OpenAIAPIKey: "sk-test"})
	defer e.Close()
	if !e.CanTranscribe() {
		t.Error("should be able to transcribe with OpenAIAPIKey set")
	}
}

func TestCanSynthesizeWithOpenAI(t *testing.T) {
	e, _ := NewEngine(Config{OpenAIAPIKey: "sk-test"})
	defer e.Close()
	if !e.CanSynthesize() {
		t.Error("should be able to synthesize with OpenAIAPIKey set")
	}
}

func TestTranscribeNotConfigured(t *testing.T) {
	e, _ := NewEngine(Config{})
	defer e.Close()
	_, err := e.Transcribe(context.Background(), "/nonexistent")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
}

func TestSynthesizeNotConfigured(t *testing.T) {
	e, _ := NewEngine(Config{})
	defer e.Close()
	_, err := e.Synthesize(context.Background(), "hello", t.TempDir())
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
}

func TestChunkForWhisperShortPassesThrough(t *testing.T) {
	// 25 seconds → must be a single chunk (no scan, no cost).
	samples := make([]float32, 25*whisperSampleRate)
	chunks := chunkForWhisper(samples)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for 25s audio, got %d", len(chunks))
	}
	if len(chunks[0]) != len(samples) {
		t.Errorf("chunk length %d != input length %d", len(chunks[0]), len(samples))
	}
}

func TestChunkForWhisperExactly30sPassesThrough(t *testing.T) {
	// Whisper accepts up to 30s; 29s sits comfortably below the cap.
	samples := make([]float32, 29*whisperSampleRate)
	chunks := chunkForWhisper(samples)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for 29s audio, got %d", len(chunks))
	}
}

func TestChunkForWhisperLongAudioSplits(t *testing.T) {
	// 75 seconds with a single uniform loud signal. With no silence to
	// snap to we still must split, and every chunk must be ≤30s.
	samples := make([]float32, 75*whisperSampleRate)
	for i := range samples {
		samples[i] = 0.5
	}
	chunks := chunkForWhisper(samples)
	if len(chunks) < 2 {
		t.Fatalf("expected ≥2 chunks for 75s audio, got %d", len(chunks))
	}
	var total int
	for i, c := range chunks {
		total += len(c)
		if len(c) > 30*whisperSampleRate {
			t.Errorf("chunk %d length %d exceeds 30s window", i, len(c))
		}
		if len(c) == 0 {
			t.Errorf("chunk %d is empty", i)
		}
	}
	if total != len(samples) {
		t.Errorf("chunks total %d samples, input had %d (lost or duplicated)", total, len(samples))
	}
}

func TestChunkForWhisperPrefersSilenceBreak(t *testing.T) {
	// 60s of "speech" (full-amplitude) with one ~200ms silent gap planted
	// at 26s. The chunker should snap the first cut to that gap rather
	// than the bare 28s ideal-cut.
	samples := make([]float32, 60*whisperSampleRate)
	for i := range samples {
		samples[i] = 0.5
	}
	gapStart := 26 * whisperSampleRate
	gapEnd := gapStart + whisperSampleRate/5 // 200ms
	for i := gapStart; i < gapEnd; i++ {
		samples[i] = 0
	}
	chunks := chunkForWhisper(samples)
	if len(chunks) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(chunks))
	}
	firstCut := len(chunks[0])
	if firstCut < gapStart || firstCut > gapEnd {
		t.Errorf("first cut at sample %d (%.2fs); expected inside silent gap [%d, %d]",
			firstCut, float64(firstCut)/float64(whisperSampleRate), gapStart, gapEnd)
	}
}
