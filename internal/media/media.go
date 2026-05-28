// Package media provides Whisper transcription and TTS synthesis, both
// embedded in the Go binary via sherpa-onnx. OpenAI API is a fallback.
// Both are optional and gracefully degrade (returning ErrNotConfigured).
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/pomofomo/multi-omp-tg/internal/audio"
)

var ErrNotConfigured = errors.New("not configured")

// Default model locations under ~/.trd/models/.
const (
	DefaultWhisperModelDir = ".trd/models/whisper"
	DefaultTTSModelDir     = ".trd/models/tts"
)

// Config holds media processing settings, all optional.
type Config struct {
	// WhisperModelDir is the path to a sherpa-onnx whisper model directory.
	// Must contain: *-encoder.int8.onnx, *-decoder.int8.onnx, *-tokens.txt
	// Defaults to ~/.trd/models/whisper/ if that directory contains model files.
	WhisperModelDir string

	// TTSModelDir is the path to a sherpa-onnx VITS piper model directory.
	// Must contain: *.onnx, tokens.txt, espeak-ng-data/
	// Defaults to ~/.trd/models/tts/ if that directory contains model files.
	TTSModelDir string

	// OpenAIAPIKey enables the OpenAI API for Whisper and/or TTS when
	// the embedded engines are not configured.
	OpenAIAPIKey string
}

// ConfigFromEnv reads media config from environment variables, falling
// back to default model directories under ~/.trd/models/.
func ConfigFromEnv() Config {
	home, _ := os.UserHomeDir()
	cfg := Config{
		WhisperModelDir: os.Getenv("TRD_WHISPER_MODEL_DIR"),
		TTSModelDir:     os.Getenv("TRD_TTS_MODEL_DIR"),
		OpenAIAPIKey:    os.Getenv("TRD_OPENAI_API_KEY"),
	}
	// Fall back to default directories if they contain model files.
	if cfg.WhisperModelDir == "" && home != "" {
		def := filepath.Join(home, DefaultWhisperModelDir)
		if hasModelFiles(def, ".onnx") {
			cfg.WhisperModelDir = def
		}
	}
	if cfg.TTSModelDir == "" && home != "" {
		def := filepath.Join(home, DefaultTTSModelDir)
		if hasModelFiles(def, ".onnx") {
			cfg.TTSModelDir = def
		}
	}
	return cfg
}

func hasModelFiles(dir, ext string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ext) && !strings.HasSuffix(e.Name(), ".onnx.json") {
			return true
		}
	}
	return false
}

// Engine wraps Config and holds persistent sherpa-onnx instances.
// Create with NewEngine; call Close when done.
type Engine struct {
	Config

	whisperMu   sync.Mutex
	whisper     *sherpa.OfflineRecognizer
	ttsMu       sync.Mutex
	tts         *sherpa.OfflineTts
}

// NewEngine creates a media engine. Initializes sherpa-onnx engines for
// any configured model directories.
func NewEngine(cfg Config) (*Engine, error) {
	e := &Engine{Config: cfg}
	if cfg.WhisperModelDir != "" {
		if err := e.initWhisper(); err != nil {
			return nil, fmt.Errorf("init whisper: %w", err)
		}
	}
	if cfg.TTSModelDir != "" {
		if err := e.initTTS(); err != nil {
			if e.whisper != nil {
				sherpa.DeleteOfflineRecognizer(e.whisper)
			}
			return nil, fmt.Errorf("init TTS: %w", err)
		}
	}
	return e, nil
}

func (e *Engine) initWhisper() error {
	dir := e.WhisperModelDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read whisper model dir %s: %w", dir, err)
	}

	// Find encoder, decoder, and tokens files.
	var encoder, decoder, tokens string
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, "-encoder.int8.onnx"):
			encoder = filepath.Join(dir, name)
		case strings.HasSuffix(name, "-decoder.int8.onnx"):
			decoder = filepath.Join(dir, name)
		case strings.HasSuffix(name, "-tokens.txt"):
			tokens = filepath.Join(dir, name)
		}
	}
	// Fall back to non-int8 if int8 not found.
	if encoder == "" || decoder == "" {
		for _, entry := range entries {
			name := entry.Name()
			if encoder == "" && strings.HasSuffix(name, "-encoder.onnx") {
				encoder = filepath.Join(dir, name)
			}
			if decoder == "" && strings.HasSuffix(name, "-decoder.onnx") {
				decoder = filepath.Join(dir, name)
			}
		}
	}
	if encoder == "" || decoder == "" || tokens == "" {
		return fmt.Errorf("whisper model dir %s missing encoder/decoder/tokens files", dir)
	}

	config := sherpa.OfflineRecognizerConfig{}
	config.FeatConfig.SampleRate = 16000
	config.FeatConfig.FeatureDim = 80
	config.ModelConfig.Whisper.Encoder = encoder
	config.ModelConfig.Whisper.Decoder = decoder
	config.ModelConfig.Whisper.Language = ""
	config.ModelConfig.Whisper.Task = "transcribe"
	config.ModelConfig.Whisper.TailPaddings = -1
	config.ModelConfig.Tokens = tokens
	config.ModelConfig.NumThreads = 2
	config.ModelConfig.Provider = "cpu"
	config.ModelConfig.Debug = 0
	config.DecodingMethod = "greedy_search"

	e.whisper = sherpa.NewOfflineRecognizer(&config)
	if e.whisper == nil {
		return fmt.Errorf("sherpa-onnx NewOfflineRecognizer returned nil")
	}
	return nil
}

func (e *Engine) initTTS() error {
	dir := e.TTSModelDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read TTS model dir %s: %w", dir, err)
	}

	var onnxFile string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".onnx") && !strings.HasSuffix(entry.Name(), ".onnx.json") {
			onnxFile = filepath.Join(dir, entry.Name())
			break
		}
	}
	if onnxFile == "" {
		return fmt.Errorf("no .onnx file found in %s", dir)
	}

	tokensFile := filepath.Join(dir, "tokens.txt")
	dataDir := filepath.Join(dir, "espeak-ng-data")

	config := sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{
			Vits: sherpa.OfflineTtsVitsModelConfig{
				Model:       onnxFile,
				Tokens:      tokensFile,
				DataDir:     dataDir,
				NoiseScale:  0.667,
				NoiseScaleW: 0.8,
				LengthScale: 1.0,
			},
			NumThreads: 2,
			Debug:      0,
			Provider:   "cpu",
		},
		MaxNumSentences: 2,
	}

	e.tts = sherpa.NewOfflineTts(&config)
	if e.tts == nil {
		return fmt.Errorf("sherpa-onnx NewOfflineTts returned nil")
	}
	return nil
}

// Close releases all sherpa-onnx resources.
func (e *Engine) Close() {
	if e.whisper != nil {
		sherpa.DeleteOfflineRecognizer(e.whisper)
		e.whisper = nil
	}
	if e.tts != nil {
		sherpa.DeleteOfflineTts(e.tts)
		e.tts = nil
	}
}

// CanTranscribe reports whether whisper transcription is available.
func (e *Engine) CanTranscribe() bool {
	return e.whisper != nil || e.OpenAIAPIKey != ""
}

// CanSynthesize reports whether TTS synthesis is available.
func (e *Engine) CanSynthesize() bool {
	return e.tts != nil || e.OpenAIAPIKey != ""
}

// Transcribe converts an audio file to text.
func (e *Engine) Transcribe(ctx context.Context, audioPath string) (string, error) {
	if e.whisper != nil {
		return e.transcribeSherpa(ctx, audioPath)
	}
	if e.OpenAIAPIKey != "" {
		return e.transcribeOpenAI(ctx, audioPath)
	}
	return "", ErrNotConfigured
}

// Synthesize converts text to an OGG audio file.
func (e *Engine) Synthesize(ctx context.Context, text, outDir string) (string, error) {
	if e.tts != nil {
		return e.synthesizeSherpa(ctx, text, outDir)
	}
	if e.OpenAIAPIKey != "" {
		return e.synthesizeOpenAI(ctx, text, outDir)
	}
	return "", ErrNotConfigured
}

// --- Whisper transcription ---

// Whisper has a hard 30-second window per decode pass (sherpa-onnx logs
// "Only waves less than 30 seconds are supported" and silently drops the
// rest). For longer audio we split into ≤maxChunkSeconds segments, prefer
// cut points that land on a low-energy frame (i.e. a likely word/sentence
// gap), and concatenate the partial transcripts.
const (
	whisperSampleRate = 16000
	maxChunkSeconds   = 28 // safety margin under the 30s hard cap
	lookbackSeconds   = 3  // search window before the ideal cut for a silence
	silenceFrameSize  = 320 // 20ms at 16kHz
)

func (e *Engine) transcribeSherpa(_ context.Context, audioPath string) (string, error) {
	// Decode audio to PCM float32 at 16kHz (sherpa-onnx whisper expects this).
	var samples []float32
	ext := strings.ToLower(filepath.Ext(audioPath))

	switch ext {
	case ".wav":
		wave := sherpa.ReadWave(audioPath)
		if wave == nil {
			return "", fmt.Errorf("failed to read WAV: %s", audioPath)
		}
		samples = wave.Samples
	case ".ogg", ".oga", ".opus":
		var err error
		samples, err = audio.DecodeOGGOpus(audioPath, whisperSampleRate)
		if err != nil {
			return "", fmt.Errorf("decode OGG/Opus: %w", err)
		}
	default:
		return "", fmt.Errorf("unsupported audio format %q — expected .wav, .ogg, .oga, or .opus", ext)
	}

	if len(samples) == 0 {
		return "", fmt.Errorf("no audio samples decoded from %s", audioPath)
	}

	e.whisperMu.Lock()
	defer e.whisperMu.Unlock()

	chunks := chunkForWhisper(samples)
	var parts []string
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		stream := sherpa.NewOfflineStream(e.whisper)
		stream.AcceptWaveform(whisperSampleRate, chunk)
		e.whisper.Decode(stream)
		text := strings.TrimSpace(stream.GetResult().Text)
		sherpa.DeleteOfflineStream(stream)
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " "), nil
}

// chunkForWhisper slices PCM samples into ≤maxChunkSeconds segments,
// preferring cut points on a low-energy frame inside a 3s lookback window
// so we don't slice mid-word. Audio at/under 29s is returned as a single
// chunk (one segment, no scan overhead).
func chunkForWhisper(samples []float32) [][]float32 {
	maxChunk := maxChunkSeconds * whisperSampleRate
	// Whisper still accepts up to 30s; only split when we're genuinely over.
	if len(samples) <= 29*whisperSampleRate {
		return [][]float32{samples}
	}

	// Pre-compute per-frame RMS once; reused for every cut search.
	nFrames := len(samples) / silenceFrameSize
	energies := make([]float32, nFrames)
	for i := 0; i < nFrames; i++ {
		var sum float64
		base := i * silenceFrameSize
		for j := 0; j < silenceFrameSize; j++ {
			v := float64(samples[base+j])
			sum += v * v
		}
		energies[i] = float32(math.Sqrt(sum / float64(silenceFrameSize)))
	}

	lookback := lookbackSeconds * whisperSampleRate
	var chunks [][]float32
	offset := 0
	for offset < len(samples) {
		remaining := len(samples) - offset
		if remaining <= 30*whisperSampleRate {
			chunks = append(chunks, samples[offset:])
			break
		}
		idealCut := offset + maxChunk
		// Quietest frame inside [idealCut-lookback, idealCut].
		startFrame := (idealCut - lookback) / silenceFrameSize
		endFrame := idealCut / silenceFrameSize
		if startFrame < offset/silenceFrameSize+1 {
			startFrame = offset/silenceFrameSize + 1
		}
		if endFrame > nFrames {
			endFrame = nFrames
		}
		cut := idealCut
		if startFrame < endFrame {
			bestFrame := startFrame
			for f := startFrame + 1; f < endFrame; f++ {
				if energies[f] < energies[bestFrame] {
					bestFrame = f
				}
			}
			cut = bestFrame * silenceFrameSize
		}
		if cut <= offset {
			cut = idealCut // safety: never advance backwards
		}
		chunks = append(chunks, samples[offset:cut])
		offset = cut
	}
	return chunks
}

func (e *Engine) transcribeOpenAI(ctx context.Context, audioPath string) (string, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper-1")
	fw, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		"https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+e.OpenAIAPIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("openai whisper: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Text), nil
}

// --- TTS synthesis ---

func (e *Engine) synthesizeSherpa(_ context.Context, text, outDir string) (string, error) {
	e.ttsMu.Lock()
	defer e.ttsMu.Unlock()

	cfg := sherpa.GenerationConfig{
		SilenceScale: 0.2,
		Speed:        float32(math.Max(1.0, 1e-6)),
		Sid:          0,
	}

	generated := e.tts.GenerateWithConfig(text, &cfg, nil)
	if len(generated.Samples) == 0 {
		return "", fmt.Errorf("sherpa-onnx produced no audio")
	}

	// Opus requires specific sample rates (8000/12000/16000/24000/48000).
	// Resample to 48000 if the TTS output rate isn't supported.
	samples := generated.Samples
	rate := generated.SampleRate
	switch rate {
	case 8000, 12000, 16000, 24000, 48000:
		// Supported natively.
	default:
		samples = audio.Resample(samples, rate, 48000)
		rate = 48000
	}

	// Encode directly to OGG/Opus (no intermediate WAV, no ffmpeg).
	oggPath := filepath.Join(outDir, fmt.Sprintf("tts-%d.ogg", time.Now().UnixNano()))
	if err := audio.EncodeOGGOpus(samples, rate, oggPath); err != nil {
		return "", fmt.Errorf("encode OGG/Opus: %w", err)
	}
	return oggPath, nil
}

func (e *Engine) synthesizeOpenAI(ctx context.Context, text, outDir string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"model":           "tts-1",
		"input":           text,
		"voice":           "alloy",
		"response_format": "opus",
	})

	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		"https://api.openai.com/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+e.OpenAIAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai tts: HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	outPath := filepath.Join(outDir, fmt.Sprintf("tts-%d.ogg", time.Now().UnixNano()))
	out, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return outPath, nil
}
