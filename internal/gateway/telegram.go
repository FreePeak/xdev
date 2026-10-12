// Package gateway implements the Telegram bridge: one background daemon that
// long-polls the Bot API, maps each chat to one xdev session, and runs each
// turn as a detached worker process of the same binary.
//
// Why a worker process per turn rather than an in-process agent — the choice
// Prime Agent's Rust rewrite measured, and the one xdev's own --bg mode
// already implements:
//
//   - isolation  a wedged provider call is a killed child, not a dead daemon
//   - no drift   the worker is the ordinary `xdev print` path, so the bridge
//     can never fall behind the CLI's behavior
//   - durability the session FILE is the state; the daemon holds only a
//     chat→session pointer, so a restart loses nothing
//
// The daemon's whole job is therefore: poll, authorize, lease, spawn, read
// the log, chunk, send. Every one of those is a few dozen lines.
//
// Secrets never live in a committed file: the bot token comes from
// TELEGRAM_BOT_TOKEN or <data dir>/gateway.token (0600), exactly the split
// internal/serve uses for its per-install tokens.
//
// Memory is not this package's job: workers are ordinary xdev runs, so they
// take the ordinary memory settings. `xdev gateway setup` writes the
// LeanKG-compatible pair (memory: hindsight + hindsight.apiUrl) that
// docs/decisions/leankg-memory-backend.md records as Option A.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Telegram Bot API limits and defaults.
const (
	// TelegramMessageLimit is the Bot API's hard cap on one message,
	// counted in UTF-16 code units (not runes, not bytes).
	TelegramMessageLimit = 4096
	// DefaultReplyChunk is the default chunk size. Below the hard cap so a
	// fence-aware split has room to move the boundary.
	DefaultReplyChunk = 4000
	// DefaultPollTimeout is the long-poll timeout in seconds. Telegram
	// holds the request open this long when nothing arrives.
	DefaultPollTimeout = 50
	// telegramErrBodyCap bounds an error body echoed into a message.
	telegramErrBodyCap = 300
	// maxBackoff caps the poll retry backoff.
	maxBackoff = 30 * time.Second
	// typingInterval is how often a running turn re-fires sendChatAction.
	// Telegram's typing indicator expires after ~5s, and one post is cheap,
	// so 2s keeps the bubble alive across a slow turn — the same cadence
	// Hermes' gateway keepalive loop uses.
	typingInterval = 2 * time.Second
)

// Telegram is a minimal Bot API client over stdlib net/http: getUpdates,
// sendMessage, sendChatAction, and getMe. No SDK, no dependency — the four
// methods the bridge needs are shorter than the import list of a client
// library.
type Telegram struct {
	// Token is the bot token from @BotFather.
	Token string
	// BaseURL overrides the API root (tests point this at an httptest
	// server); empty means https://api.telegram.org.
	BaseURL string
	// HTTPClient is the transport (tests inject one).
	HTTPClient *http.Client
	// Logf receives lifecycle lines.
	Logf func(format string, args ...any)
}

func (t *Telegram) base() string {
	if t.BaseURL != "" {
		return strings.TrimRight(t.BaseURL, "/")
	}
	return "https://api.telegram.org"
}

func (t *Telegram) client() *http.Client {
	if t.HTTPClient != nil {
		return t.HTTPClient
	}
	// A long poll holds the connection for pollTimeout+slack; the client
	// timeout must clear that or every quiet poll looks like a failure.
	return &http.Client{Timeout: 2 * time.Minute}
}

func (t *Telegram) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// apiResponse is Telegram's envelope: every method answers
// {"ok":bool,"result":…,"description":"…"}.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

// call posts one Bot API method as JSON and decodes its result into out.
func (t *Telegram) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram %s: marshal: %w", method, err)
	}
	endpoint := t.base() + "/bot" + t.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	return t.send(ctx, method, req, out)
}

// send performs one prepared request and decodes the Bot API envelope. It is
// the half every method shares — the token masking, the bounded body read and
// the ok:false decoding — split out so the multipart uploads reach the same
// error handling as the JSON calls rather than reimplementing it.
func (t *Telegram) send(ctx context.Context, method string, req *http.Request, out any) error {
	resp, err := t.client().Do(req)
	if err != nil {
		// The transport error embeds the URL, and the URL carries the token
		// — a raw one in a log line or an error reply is a secret in a
		// place the operator did not choose. Masked: the endpoint is still
		// named (as api.telegram.org/bot<mask>/getMe), the secret is not.
		return fmt.Errorf("telegram %s: %w", method, maskTokenInError(err, t.Token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read: %w", method, err)
	}
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		// A non-JSON body is the shape a proxy or a wrong base URL leaves
		// behind; echoing the first bytes names it instead of a parse error.
		return fmt.Errorf("telegram %s: HTTP %d: %s", method, resp.StatusCode, clip(string(raw), telegramErrBodyCap))
	}
	if !env.OK {
		msg := env.Description
		if msg == "" {
			msg = clip(string(raw), telegramErrBodyCap)
		}
		return &APIError{Method: method, Code: env.ErrorCode, HTTPStatus: resp.StatusCode, Description: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

// APIError is a Bot API-level failure (ok:false), carrying the code so a
// caller can distinguish a 409 Conflict — a second poller on the same token,
// the single most common setup mistake — from everything else.
type APIError struct {
	Method      string
	Code        int
	HTTPStatus  int
	Description string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
	}
	return fmt.Sprintf("telegram %s: HTTP %d: %s", e.Method, e.HTTPStatus, e.Description)
}

// IsConflict reports whether err is the 409 a second getUpdates causes.
//
// Telegram's rule: a new getUpdates terminates the previous long poll, and
// the terminated one surfaces as 409 to whoever was holding it. So a 409
// means "something else polled this token just now" — a rival daemon, or a
// curl, or a health check. The daemon must not treat it as fatal (see
// daemon.go's conflict handling), but the caller still needs to tell it from
// a network error.
func IsConflict(err error) bool {
	var api *APIError
	return errors.As(err, &api) && (api.Code == http.StatusConflict || api.HTTPStatus == http.StatusConflict)
}

// Update is one inbound update, narrowed to the fields the bridge uses.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is an inbound message.
type Message struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
}

// Chat identifies the conversation a reply goes to.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// User identifies the sender (the allowlist checks this id for DMs, where
// chat id and user id are the same number, and for groups it is who spoke).
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// SentMessage is sendMessage's result (only the id is used).
type SentMessage struct {
	MessageID int64 `json:"message_id"`
}

// GetUpdates long-polls for updates after offset. A timeout of 0 uses the
// default. The caller passes the next offset (last update id + 1) so
// Telegram stops redelivering acknowledged updates.
func (t *Telegram) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	if timeout <= 0 {
		timeout = DefaultPollTimeout
	}
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message"},
	}
	var updates []Update
	if err := t.call(ctx, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// SendMessage sends one already-chunked message, HTML-parsed.
//
// parse_mode is HTML rather than MarkdownV2 on purpose: the two dialects
// differ per chunk of model output, and the failure modes are not symmetric.
// MarkdownV2 has ~20 reserved characters that must ALL be escaped, and its
// grammar is strict enough that one unescaped '_' inside an identifier or a
// stray '.' after a number rejects the entire send — so a near-perfect
// conversion loses the whole answer. HTML escaping has exactly three
// reserved characters and they never collide with code, which is why a
// rendering tag is constructed from the text rather than by escaping prose.
//
// There is still ONE attempt per message: if the API rejects the parsed
// form, the same text is resent with parse_mode omitted. The phone renders
// it as text — a plain answer beats a lost one.
func (t *Telegram) SendMessage(ctx context.Context, chatID int64, text string) error {
	rendered := ToTelegramHTML(text)
	payload := map[string]any{"chat_id": chatID, "text": rendered, "parse_mode": "HTML"}
	var sent SentMessage
	err := t.call(ctx, "sendMessage", payload, &sent)
	if err == nil {
		return nil
	}
	if !isParseError(err) {
		return err
	}
	// The API rejected the parsed form. Resend the same text as plain text;
	// a readable answer matters more than its formatting.
	t.logf("telegram: %v; resending as plain text", err)
	plain := map[string]any{"chat_id": chatID, "text": text}
	return t.call(ctx, "sendMessage", plain, &sent)
}

// SendPhoto sends one JPEG with an optional caption. Used for the per-model
// badge: the phone shows WHICH model answered before it shows the words, and
// a caption is the only text Telegram lets ride a photo.
//
// Uploaded as multipart rather than as a file_id: Telegram's file_ids are
// per-bot and are obtained by uploading once, so a bridge that only ever
// sends these has nothing to reuse — and a cache of file_ids would be a
// second state store to keep honest for no saved bytes.
func (t *Telegram) SendPhoto(ctx context.Context, chatID int64, jpeg []byte, caption string) error {
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10)}
	if caption != "" {
		fields["caption"] = ToTelegramHTML(caption)
		fields["parse_mode"] = "HTML"
	}
	var sent SentMessage
	err := t.callUpload(ctx, "sendPhoto", fields, "photo", "model.jpg", jpeg, &sent)
	if err == nil || caption == "" || !isParseError(err) {
		return err
	}
	// Same rule as SendMessage: a rejected caption costs the formatting,
	// never the photo.
	delete(fields, "parse_mode")
	return t.callUpload(ctx, "sendPhoto", fields, "photo", "model.jpg", jpeg, &sent)
}

// SetMyProfilePhoto changes the bot's own profile photo, which is what a chat
// with the bot shows in its header. Returns the API error unchanged so the
// caller can decide whether a missing permission is worth reporting.
//
// Telegram refuses to reuse a photo it already has, so every call uploads.
// The InputProfilePhoto object is passed as a field and refers to the upload
// by its attach name.
func (t *Telegram) SetMyProfilePhoto(ctx context.Context, jpeg []byte) error {
	fields := map[string]string{
		"photo": `{"type":"static","photo":"attach://` + profilePhotoField + `"}`,
	}
	return t.callUpload(ctx, "setMyProfilePhoto", fields, profilePhotoField, "profile.jpg", jpeg, nil)
}

// profilePhotoField is the multipart part name the InputProfilePhoto object
// above attaches by. One constant, so the two cannot drift.
const profilePhotoField = "photo_file"

// callUpload posts one multipart/form-data method call: the text fields plus
// exactly one file part. Kept beside call (rather than folded into it) because
// the JSON path is what every other method uses and a branch there would make
// each of them read as if it might upload.
func (t *Telegram) callUpload(ctx context.Context, method string, fields map[string]string, fileField, fileName string, file []byte, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return fmt.Errorf("telegram %s: field %s: %w", method, k, err)
		}
	}
	fw, err := mw.CreateFormFile(fileField, fileName)
	if err != nil {
		return fmt.Errorf("telegram %s: file part: %w", method, err)
	}
	if _, err := fw.Write(file); err != nil {
		return fmt.Errorf("telegram %s: write file: %w", method, err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("telegram %s: close: %w", method, err)
	}
	endpoint := t.base() + "/bot" + t.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return t.send(ctx, method, req, out)
}

// SendChatAction posts one chat action — the bridge only ever needs "typing".
// Telegram's typing bubble expires after ~5s, so a running turn re-fires it:
// without that the chat looks idle for the whole of a slow model call.
// A chat action is a courtesy, not the answer, so a failure is the caller's
// to log and never a reason to fail the turn.
func (t *Telegram) SendChatAction(ctx context.Context, chatID int64) error {
	return t.call(ctx, "sendChatAction",
		map[string]any{"chat_id": chatID, "action": "typing"}, nil)
}

// isParseError reports whether err is Telegram refusing a parse_mode body.
func isParseError(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.Code {
	case http.StatusBadRequest:
		return strings.Contains(strings.ToLower(api.Description), "parse") ||
			strings.Contains(strings.ToLower(api.Description), "entity") ||
			strings.Contains(strings.ToLower(api.Description), "tag")
	}
	return false
}

// Me is getMe's result (identity + a cheap token check).
type Me struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// BotCommand is one entry in the client's "/" menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands registers the command menu, so a phone user tapping "/" sees
// the available commands with descriptions instead of having to know them.
// Every scope is registered separately, because Telegram picks the narrowest
// matching one and a group chat never falls through to the default.
func (t *Telegram) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	for _, scope := range []map[string]any{
		{"type": "default"},
		{"type": "all_private_chats"},
		{"type": "all_group_chats"},
	} {
		payload := map[string]any{"commands": cmds, "scope": scope}
		if err := t.call(ctx, "setMyCommands", payload, nil); err != nil {
			return err
		}
	}
	return nil
}

// GetMe validates the token and names the bot. This is the one call the
// daemon makes at startup, so a wrong token fails immediately and loudly
// rather than as a silent loop that never receives anything.
func (t *Telegram) GetMe(ctx context.Context) (*Me, error) {
	var me Me
	if err := t.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return nil, err
	}
	return &me, nil
}

// ChunkText splits text into pieces Telegram will accept, each at most
// limit UTF-16 code units. Empty text yields nothing (a turn that produced
// no answer sends nothing rather than an empty bubble).
//
// Three rules, each of which a naive rune-count split gets wrong:
//
//  1. The limit is UTF-16 code units. An emoji is one rune and two units;
//     counting runes lets a message of emoji overrun the cap and 400.
//  2. Never split a surrogate pair — a lone half is invalid UTF-16 and
//     Telegram rejects the whole send.
//  3. Prefer a newline boundary near the cut, so a code block or a sentence
//     is not sliced mid-line when a nearby break exists.
func ChunkText(text string, limit int) []string {
	if limit <= 0 || limit > TelegramMessageLimit {
		limit = DefaultReplyChunk
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	var out []string
	rest := text
	for rest != "" {
		if utf16Len(rest) <= limit {
			out = append(out, rest)
			break
		}
		cut := utf16Cut(rest, limit)
		if cut <= 0 {
			// A zero cut would loop forever — take one rune and move on.
			_, size := decodeFirstRune(rest)
			cut = size
		}
		// Back off to the last newline in the second half of the window:
		// far enough back to matter, close enough that no chunk is tiny.
		if nl := strings.LastIndex(rest[:cut], "\n"); nl > cut/2 {
			cut = nl + 1
		}
		out = append(out, rest[:cut])
		rest = rest[cut:]
	}
	return out
}

// utf16Len is the length of s in UTF-16 code units.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// utf16Cut returns the largest byte index i such that s[:i] is at most
// limit UTF-16 code units AND ends on a rune boundary.
func utf16Cut(s string, limit int) int {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > limit {
			return i
		}
		units += w
	}
	return len(s)
}

// decodeFirstRune returns the first rune and its byte width ("" → 0, 0).
func decodeFirstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

// clip bounds a string for an error message.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// MaskToken hides all but the last four characters of a token, for logs.
func MaskToken(tok string) string {
	if len(tok) <= 4 {
		return "****"
	}
	return "…" + tok[len(tok)-4:]
}

// ValidTokenShape reports whether a string looks like a BotFather token
// (<digits>:<35+ chars>). Same rule Hermes applies at setup
// (hermes_cli/setup_platforms.py:12), so a pasted token is caught before a
// daemon is installed around it.
func ValidTokenShape(tok string) bool {
	tok = strings.TrimSpace(tok)
	i := strings.Index(tok, ":")
	if i <= 0 || i == len(tok)-1 {
		return false
	}
	for _, r := range tok[:i] {
		if r < '0' || r > '9' {
			return false
		}
	}
	secret := tok[i+1:]
	if len(secret) < 30 {
		return false
	}
	for _, r := range secret {
		ok := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// maskTokenInError rewrites a token that a transport error carries inside a
// URL. net/http errors of every kind ("Post “https://api.telegram.org/bot827…/getMe”:
// context canceled", dial errors, TLS errors) all quote the URL, so the fix is
// one redaction at the single place the URL is built.
func maskTokenInError(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, token) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, token, MaskToken(token)))
}
