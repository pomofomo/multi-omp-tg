// Package telegram is a minimal Bot API client built on net/http.
// Only the methods TRD needs are implemented.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const apiBase = "https://api.telegram.org"

// Client is a Bot API client.
type Client struct {
	token string
	http  *http.Client
}

// New builds a Client. The token should be "123:ABC..." from @BotFather.
func New(token string) *Client {
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 65 * time.Second}, // long-poll uses 30s, allow slack
	}
}

// Update is a subset of the Bot API Update object.
type Update struct {
	UpdateID      int      `json:"update_id"`
	Message       *Message `json:"message,omitempty"`
	EditedMessage *Message `json:"edited_message,omitempty"`
}

// Message is a subset of the Bot API Message object.
type Message struct {
	MessageID       int         `json:"message_id"`
	MessageThreadID int         `json:"message_thread_id,omitempty"`
	From            *User       `json:"from,omitempty"`
	Chat            Chat        `json:"chat"`
	Date            int64       `json:"date"`
	Text            string      `json:"text,omitempty"`
	Caption         string      `json:"caption,omitempty"`
	Photo           []PhotoSize `json:"photo,omitempty"`
	Document        *Document   `json:"document,omitempty"`
	Voice           *Voice      `json:"voice,omitempty"`
	Audio           *Audio      `json:"audio,omitempty"`
	IsTopicMessage  bool        `json:"is_topic_message,omitempty"`
}

// User is a subset of the Bot API User object.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

// Chat is a subset of the Bot API Chat object.
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // "supergroup", "group", "private", etc.
	Title     string `json:"title,omitempty"`
	IsForum   bool   `json:"is_forum,omitempty"`
}

// PhotoSize identifies one rendition of a photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int    `json:"file_size,omitempty"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// Document is a generic file attachment.
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int    `json:"file_size,omitempty"`
}

// Voice is a voice message (OGG/Opus).
type Voice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int    `json:"file_size,omitempty"`
}

// Audio is an audio file (MP3, etc.).
type Audio struct {
	FileID    string `json:"file_id"`
	Duration  int    `json:"duration"`
	Performer string `json:"performer,omitempty"`
	Title     string `json:"title,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	FileSize  int    `json:"file_size,omitempty"`
}

type apiResp[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result,omitempty"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
}

func (c *Client) url(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", apiBase, c.token, method)
}

func (c *Client) filesURL(path string) string {
	return fmt.Sprintf("%s/file/bot%s/%s", apiBase, c.token, path)
}

// doJSON posts a JSON body to method and decodes into out (which must be &apiResp[T]).
func doJSON[T any](ctx context.Context, c *Client, method string, body any) (T, error) {
	var zero T
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return zero, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(method), &buf)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, err
	}
	var parsed apiResp[T]
	if err := json.Unmarshal(data, &parsed); err != nil {
		return zero, fmt.Errorf("decode %s: %w (body=%s)", method, err, string(data))
	}
	if !parsed.OK {
		return zero, fmt.Errorf("telegram %s: %s (code=%d)", method, parsed.Description, parsed.ErrorCode)
	}
	return parsed.Result, nil
}

// GetMe verifies the token works.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	return doJSON[User](ctx, c, "getMe", nil)
}

// GetUpdates long-polls for new updates starting at offset.
func (c *Client) GetUpdates(ctx context.Context, offset int, timeoutSec int) ([]Update, error) {
	return doJSON[[]Update](ctx, c, "getUpdates", map[string]any{
		"offset":  offset,
		"timeout": timeoutSec,
		// Empty/omitted allowed_updates means "all types except chat_member-ish".
		// We want to *see* everything the bot is permitted to see, so omit the filter.
	})
}

// GetUpdatesRaw is like GetUpdates but also returns each update as its raw JSON
// so callers can log exactly what Telegram delivered (including fields this
// client doesn't model, e.g. edited_message, message_reaction, callback_query).
func (c *Client) GetUpdatesRaw(ctx context.Context, offset int, timeoutSec int) ([]Update, []json.RawMessage, error) {
	raws, err := doJSON[[]json.RawMessage](ctx, c, "getUpdates", map[string]any{
		"offset":  offset,
		"timeout": timeoutSec,
	})
	if err != nil {
		return nil, nil, err
	}
	ups := make([]Update, 0, len(raws))
	for _, r := range raws {
		var u Update
		if err := json.Unmarshal(r, &u); err != nil {
			return nil, nil, fmt.Errorf("decode update: %w (body=%s)", err, string(r))
		}
		ups = append(ups, u)
	}
	return ups, raws, nil
}

// SendMessageParams is a subset of sendMessage params.
type SendMessageParams struct {
	ChatID          int64  `json:"chat_id"`
	MessageThreadID int    `json:"message_thread_id,omitempty"`
	Text            string `json:"text"`
	ReplyToMessageID int   `json:"reply_to_message_id,omitempty"`
	ParseMode       string `json:"parse_mode,omitempty"`
}

// SendMessage sends a text message.
func (c *Client) SendMessage(ctx context.Context, p SendMessageParams) (Message, error) {
	return doJSON[Message](ctx, c, "sendMessage", p)
}

// EditMessageTextParams is a subset of editMessageText params.
type EditMessageTextParams struct {
	ChatID    int64  `json:"chat_id"`
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// EditMessageText edits an existing message.
func (c *Client) EditMessageText(ctx context.Context, p EditMessageTextParams) error {
	_, err := doJSON[json.RawMessage](ctx, c, "editMessageText", p)
	return err
}

// SetReaction sets a single-emoji reaction on a message.
func (c *Client) SetReaction(ctx context.Context, chatID int64, messageID int, emoji string) error {
	type reactionEmoji struct {
		Type  string `json:"type"`
		Emoji string `json:"emoji"`
	}
	body := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   []reactionEmoji{{Type: "emoji", Emoji: emoji}},
	}
	_, err := doJSON[json.RawMessage](ctx, c, "setMessageReaction", body)
	return err
}

// sendFile is a shared helper for sendDocument/sendPhoto/sendVoice/sendAudio.
// fieldName is the multipart field ("document", "photo", "voice", "audio").
// replyToMsgID=0 omits the reply binding.
func (c *Client) sendFile(ctx context.Context, method, fieldName string, chatID int64, threadID, replyToMsgID int, path, caption string) (Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return Message{}, err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if threadID != 0 {
		_ = mw.WriteField("message_thread_id", strconv.Itoa(threadID))
	}
	if replyToMsgID != 0 {
		_ = mw.WriteField("reply_to_message_id", strconv.Itoa(replyToMsgID))
	}
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile(fieldName, filepath.Base(path))
	if err != nil {
		return Message{}, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return Message{}, err
	}
	if err := mw.Close(); err != nil {
		return Message{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(method), &buf)
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, err
	}
	var parsed apiResp[Message]
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Message{}, fmt.Errorf("decode %s: %w (body=%s)", method, err, string(data))
	}
	if !parsed.OK {
		return Message{}, fmt.Errorf("telegram %s: %s", method, parsed.Description)
	}
	return parsed.Result, nil
}

// SendDocument uploads a file and sends it as a document.
func (c *Client) SendDocument(ctx context.Context, chatID int64, threadID int, path string, caption string) (Message, error) {
	return c.sendFile(ctx, "sendDocument", "document", chatID, threadID, 0, path, caption)
}

// SendPhoto uploads and sends a photo (inline display in Telegram).
func (c *Client) SendPhoto(ctx context.Context, chatID int64, threadID int, path string, caption string) (Message, error) {
	return c.sendFile(ctx, "sendPhoto", "photo", chatID, threadID, 0, path, caption)
}

// SendVoice uploads and sends a voice message (OGG/Opus, inline playback).
// replyToMsgID=0 leaves the message unthreaded inside the topic.
func (c *Client) SendVoice(ctx context.Context, chatID int64, threadID, replyToMsgID int, path string, caption string) (Message, error) {
	return c.sendFile(ctx, "sendVoice", "voice", chatID, threadID, replyToMsgID, path, caption)
}

// SendAudio uploads and sends an audio file (MP3, etc., with player UI).
func (c *Client) SendAudio(ctx context.Context, chatID int64, threadID int, path string, caption string) (Message, error) {
	return c.sendFile(ctx, "sendAudio", "audio", chatID, threadID, 0, path, caption)
}

// fileInfo is what getFile returns.
type fileInfo struct {
	FileID   string `json:"file_id"`
	FileSize int    `json:"file_size"`
	FilePath string `json:"file_path"`
}

// DownloadFile fetches a file by file_id and writes it to destDir, returning the full path.
func (c *Client) DownloadFile(ctx context.Context, fileID, destDir string) (string, error) {
	info, err := doJSON[fileInfo](ctx, c, "getFile", map[string]any{"file_id": fileID})
	if err != nil {
		return "", err
	}
	if info.FilePath == "" {
		return "", errors.New("no file_path in getFile response")
	}
	dlURL := c.filesURL(info.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: HTTP %d", dlURL, resp.StatusCode)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", err
	}
	// Use the base of the telegram-provided path as local filename.
	local := filepath.Join(destDir, filepath.Base(info.FilePath))
	out, err := os.Create(local)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return local, nil
}

// BotCommand represents a single bot command for the menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands registers the bot's command list (shows in autocomplete).
func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	_, err := doJSON[bool](ctx, c, "setMyCommands", map[string]any{
		"commands": commands,
	})
	return err
}
