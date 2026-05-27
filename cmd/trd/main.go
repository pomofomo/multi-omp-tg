// Command trd is the Telegram Repo Dispatcher binary.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pomofomo/multi-claude-tg/internal/config"
	"github.com/pomofomo/multi-claude-tg/internal/dispatcher"
	"github.com/pomofomo/multi-claude-tg/internal/storage"

)

const usage = `trd — Telegram Repo Dispatcher

Usage:
  trd start --telegram-token <token> [--port 7777]
  trd status
  trd list
  trd stop    <name-or-prefix>
  trd watch   <name-or-prefix>
  trd shell   <name-or-prefix>
  trd cd      <name-or-prefix>
  trd allow   <username>
  trd deny    <username>
  trd allowed

<name-or-prefix> matches against repo name first, then instance ID prefix.

Env:
  TELEGRAM_BOT_TOKEN      default for --telegram-token
  TRD_PORT                default for --port (7777)
  TRD_HEALTH_INTERVAL_SEC health-loop interval (default 30)
  TRD_ALLOWED_USERNAMES   comma-separated allowlist (merged with stored list)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	switch sub {
	case "start":
		cmdStart(args)
	case "status", "list":
		cmdStatus(args)
	case "stop":
		cmdStop(args)
	case "watch", "logs":
		cmdWatch(args)
	case "shell":
		cmdShell(args)
	case "cd":
		cmdCd(args)
	case "allow":
		cmdAllow(args)
	case "deny":
		cmdDeny(args)
	case "allowed":
		cmdAllowed(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", sub, usage)
		os.Exit(2)
	}
}

// persistedEnvKeys are the env vars that get saved to bbolt on start and
// restored on subsequent starts when the env var isn't set.
var persistedEnvKeys = []string{
	"TELEGRAM_BOT_TOKEN",
	"TRD_WHISPER_MODEL_DIR",
	"TRD_TTS_MODEL_DIR",
	"TRD_CHANNEL_ENTRY",
	"TRD_OPENAI_API_KEY",
	"TRD_ALLOWED_USERNAMES",
}

// loadSavedSettings opens the DB, and for each persisted key where the env
// var is not set, restores it from the stored value. Returns the store so
// the caller can save new values after startup.
func loadSavedSettings() {
	dbPath, err := config.StateDBPath()
	if err != nil {
		return
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		return
	}
	defer store.Close()
	for _, key := range persistedEnvKeys {
		if os.Getenv(key) == "" {
			if val := store.GetSetting(key); val != "" {
				os.Setenv(key, val)
			}
		}
	}
}

func cmdStart(args []string) {
	// Restore saved settings as env fallbacks before parsing flags.
	_ = config.EnsureRoot()
	loadSavedSettings()

	fs := flag.NewFlagSet("start", flag.ExitOnError)
	token := fs.String("telegram-token", os.Getenv("TELEGRAM_BOT_TOKEN"), "Telegram bot token")
	port := fs.Int("port", envInt("TRD_PORT", 7777), "dispatcher HTTP/WS port")
	debug := fs.Bool("debug", os.Getenv("TRD_DEBUG") == "1", "enable debug logging and pass --debug to Claude instances")
	_ = fs.Parse(args)
	if *token == "" {
		fmt.Fprintln(os.Stderr, "--telegram-token is required (or set TELEGRAM_BOT_TOKEN)")
		os.Exit(2)
	}

	logger := newLogger(*debug)
	d, err := dispatcher.New(dispatcher.Options{
		TelegramToken: *token,
		Port:          *port,
		Logger:        logger,
		Debug:         *debug,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start failed:", err)
		os.Exit(1)
	}
	defer d.Close()

	// Persist current env vars so future restarts work without them.
	d.SaveSettings(persistedEnvKeys)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("trd started", "port", *port)
	if err := d.Run(ctx); err != nil {
		logger.Error("run", "err", err)
		os.Exit(1)
	}
	logger.Info("trd stopped")
}

// instanceInfo mirrors dispatcher.InstanceInfo for decoding the API response.
type instanceInfo struct {
	storage.Instance
	// Running is set by the dispatcher when an omp -p run is in flight
	// for this instance. Older API responses sent `connected`/`tmux_alive`;
	// those keys are ignored after the headless port.
	Running bool `json:"running"`
}

// allInstances tries the running dispatcher's HTTP API first, then falls back
// to opening the bbolt DB directly (which only works when the server is stopped).
func allInstances() ([]instanceInfo, error) {
	port := envInt("TRD_PORT", 7777)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/instances", port)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var instances []instanceInfo
			if err := json.NewDecoder(resp.Body).Decode(&instances); err == nil {
				return instances, nil
			}
		}
	}
	// Fallback: open DB directly (works when server is not running).
	dbPath, _ := config.StateDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil // no DB yet
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer store.Close()
	all, err := store.All()
	if err != nil {
		return nil, err
	}
	infos := make([]instanceInfo, len(all))
	for i, inst := range all {
		infos[i] = instanceInfo{Instance: inst}
	}
	return infos, nil
}

func cmdStatus(args []string) {
	_ = args
	all, err := allInstances()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(all) == 0 {
		fmt.Println("no instances")
		return
	}
	for _, inst := range all {
		name := inst.RepoName
		if name == "" {
			name = storage.RepoNameFromURL(inst.RepoURL)
		}
		session := inst.SessionID
		if session == "" {
			session = "-"
		} else if len(session) > 8 {
			session = session[:8]
		}
		fmt.Printf("%-20s %s  %s  state=%-8s session=%s  running=%v  %s\n",
			name, inst.InstanceID[:8], shortTime(inst.CreatedAt),
			inst.State, session, inst.Running, inst.RepoURL)
	}
}

func cmdShell(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: trd shell <name-or-prefix>")
		os.Exit(2)
	}
	inst, err := findInstance(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	fmt.Fprintf(os.Stderr, "opening shell in %s (%s)\n", inst.RepoPath, inst.RepoName)
	cmd := exec.Command(shell)
	cmd.Dir = inst.RepoPath
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

func cmdCd(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: trd cd <name-or-prefix>")
		os.Exit(2)
	}
	inst, err := findInstance(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(inst.RepoPath)
}

func cmdStop(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: trd stop <name-or-prefix>")
		os.Exit(2)
	}
	inst, err := findInstance(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port := envInt("TRD_PORT", 7777)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/instances/%s/cancel", port, inst.InstanceID)
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cancel via dispatcher:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "cancel failed: %s\n%s\n", resp.Status, body)
		os.Exit(1)
	}
	fmt.Println("cancel requested for", inst.InstanceID[:8])
}

func cmdWatch(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: trd watch <name-or-prefix>")
		os.Exit(2)
	}
	inst, err := findInstance(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logPath, err := config.InstanceLogPath(inst.InstanceID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log path:", err)
		os.Exit(1)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "no log yet — send a message in the topic to spawn the agent.")
			return
		}
		fmt.Fprintln(os.Stderr, "read log:", err)
		os.Exit(1)
	}
	_, _ = io.Copy(os.Stdout, strings.NewReader(string(data)))
}

func cmdAllow(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: trd allow <username>")
		os.Exit(2)
	}
	username := strings.ToLower(strings.TrimPrefix(args[0], "@"))
	port := envInt("TRD_PORT", 7777)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/allowed/%s", port, username)
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatcher not running:", err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		fmt.Fprintf(os.Stderr, "unexpected status: %d\n", resp.StatusCode)
		os.Exit(1)
	}
	fmt.Printf("allowed: %s\n", username)
}

func cmdDeny(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: trd deny <username>")
		os.Exit(2)
	}
	username := strings.ToLower(strings.TrimPrefix(args[0], "@"))
	port := envInt("TRD_PORT", 7777)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/allowed/%s", port, username)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatcher not running:", err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		fmt.Fprintf(os.Stderr, "unexpected status: %d\n", resp.StatusCode)
		os.Exit(1)
	}
	fmt.Printf("denied: %s\n", username)
}

func cmdAllowed(args []string) {
	_ = args
	port := envInt("TRD_PORT", 7777)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/allowed", port)
	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatcher not running:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var users []string
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		fmt.Fprintln(os.Stderr, "decode:", err)
		os.Exit(1)
	}
	if len(users) == 0 {
		fmt.Println("allowlist empty — all users permitted")
		return
	}
	fmt.Printf("allowed users (%d):\n", len(users))
	for _, u := range users {
		fmt.Printf("  @%s\n", u)
	}
}

func findInstance(query string) (*instanceInfo, error) {
	all, err := allInstances()
	if err != nil {
		return nil, err
	}
	// First pass: match on repo name (exact or prefix).
	var byName []instanceInfo
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
		return nil, fmt.Errorf("%d instances match repo name %q — use a longer prefix or instance ID", len(byName), query)
	}
	// Second pass: match on instance ID prefix.
	var byID []instanceInfo
	for _, inst := range all {
		if strings.HasPrefix(inst.InstanceID, query) {
			byID = append(byID, inst)
		}
	}
	switch len(byID) {
	case 0:
		return nil, fmt.Errorf("no instance matching %q", query)
	case 1:
		return &byID[0], nil
	default:
		return nil, fmt.Errorf("%d instances match %q — use a longer prefix", len(byID), query)
	}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func newLogger(debug bool) *slog.Logger {
	logPath, _ := config.LogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	var out io.Writer = os.Stderr
	if err == nil {
		out = io.MultiWriter(os.Stderr, f)
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
}

func shortTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") }
