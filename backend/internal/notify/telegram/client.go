// Package telegram delivers AO notifications to a Telegram chat and accepts a
// small set of control commands back.
//
// It exists because the operator is not sitting in front of the dashboard: the
// daemon runs on a remote box, and the two events worth interrupting a human
// for — a session blocked on a permission prompt, and a PR waiting for a merge
// decision — need to reach a phone.
//
// The whole package is inert without AO_TELEGRAM_BOT_TOKEN, so a stock install
// never talks to Telegram.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// EnvBotToken and EnvChatID are the only required configuration. Both must
	// be set for the notifier to come up.
	EnvBotToken = "AO_TELEGRAM_BOT_TOKEN"
	EnvChatID   = "AO_TELEGRAM_CHAT_ID"
	// EnvAPIBase overrides the Telegram API root; tests point it at httptest.
	EnvAPIBase = "AO_TELEGRAM_API_BASE"
	// EnvProxy routes Telegram traffic through an HTTP proxy. Required wherever
	// api.telegram.org is blocked at the network level — the connection just
	// times out there, so without a proxy the notifier retries forever.
	EnvProxy = "AO_TELEGRAM_PROXY"

	defaultAPIBase = "https://api.telegram.org"
	// requestTimeout bounds a single sendMessage. Long polling sets its own.
	requestTimeout = 15 * time.Second
)

// Client is a minimal Telegram Bot API client: send a message, read updates.
type Client struct {
	http      *http.Client
	transport http.RoundTripper
	apiBase   string
	token     string
	chatID    string
}

// Config configures a Client. Empty fields fall back to the environment.
type Config struct {
	Token   string
	ChatID  string
	APIBase string
	// Proxy is an HTTP proxy URL for Telegram traffic only. Empty means direct.
	Proxy      string
	HTTPClient *http.Client
}

// NewFromEnv returns a client, or ok=false when the deployment did not
// configure Telegram. A missing token is the normal case, not an error.
func NewFromEnv() (*Client, bool) {
	token := strings.TrimSpace(os.Getenv(EnvBotToken))
	chatID := strings.TrimSpace(os.Getenv(EnvChatID))
	if token == "" || chatID == "" {
		return nil, false
	}
	return New(Config{
		Token:   token,
		ChatID:  chatID,
		APIBase: os.Getenv(EnvAPIBase),
		Proxy:   os.Getenv(EnvProxy),
	}), true
}

// New builds a client from an explicit config.
func New(cfg Config) *Client {
	c := &Client{
		http:    cfg.HTTPClient,
		apiBase: strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/"),
		token:   strings.TrimSpace(cfg.Token),
		chatID:  strings.TrimSpace(cfg.ChatID),
	}
	c.transport = proxyTransport(cfg.Proxy)
	if c.http == nil {
		c.http = &http.Client{Timeout: requestTimeout, Transport: c.transport}
	}
	if c.apiBase == "" {
		c.apiBase = defaultAPIBase
	}
	return c
}

// ChatID reports the chat this client talks to. Updates from any other chat are
// ignored, so a leaked bot username cannot drive the conveyor.
func (c *Client) ChatID() string { return c.chatID }

// Send posts one message. Text is sent as-is (no parse mode), so issue titles
// and agent output cannot break formatting or be interpreted as markup.
func (c *Client) Send(ctx context.Context, text string) error {
	_, err := c.SendMessage(ctx, text)
	return err
}

// SendMessage posts one message and returns its id, which is what a later edit
// needs: an answer that overwrites the message promising it keeps one question
// to one line in the chat.
func (c *Client) SendMessage(ctx context.Context, text string) (int64, error) {
	return c.send(ctx, text, nil)
}

// SendWithKeyboard posts a message carrying inline buttons. A menu the human
// taps beats a command they have to remember the syntax of, and the buttons are
// also the only way to offer a link and an action in the same breath.
func (c *Client) SendWithKeyboard(ctx context.Context, text string, keyboard Keyboard) (int64, error) {
	return c.send(ctx, text, keyboard.markup())
}

// Ask posts a message Telegram pre-opens a reply box for, so the human types
// the answer into the right place instead of into a fresh message the bot has
// no way to tie back to the question. mention is the @name the prompt is meant
// for: with one, the reply box opens for that person alone, which is what keeps
// a group chat from being told to answer somebody else's question.
func (c *Client) Ask(ctx context.Context, text, mention string) (int64, error) {
	markup := map[string]any{"force_reply": true}
	if strings.TrimSpace(mention) != "" {
		markup["selective"] = true
	}
	return c.send(ctx, text, markup)
}

func (c *Client) send(ctx context.Context, text string, markup any) (int64, error) {
	payload := map[string]any{
		"chat_id":                  c.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := c.call(ctx, "sendMessage", payload, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, fmt.Errorf("telegram: sendMessage rejected: %s", resp.Description)
	}
	return resp.Result.MessageID, nil
}

// InlineButton is one button under a message: either a callback button, which
// sends Data back to the bot, or a link button, which opens URL. Exactly one of
// the two is set.
type InlineButton struct {
	Text string
	Data string
	URL  string
}

// Keyboard is the rows of buttons under a message.
type Keyboard [][]InlineButton

// markup renders the keyboard as Telegram's reply_markup, or nil when there is
// nothing to show — an empty inline_keyboard is rejected by the API.
func (k Keyboard) markup() any {
	rows := make([][]map[string]any, 0, len(k))
	for _, row := range k {
		rendered := make([]map[string]any, 0, len(row))
		for _, button := range row {
			switch {
			case button.URL != "":
				rendered = append(rendered, map[string]any{"text": button.Text, "url": button.URL})
			case button.Data != "":
				rendered = append(rendered, map[string]any{"text": button.Text, "callback_data": button.Data})
			}
		}
		if len(rendered) > 0 {
			rows = append(rows, rendered)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return map[string]any{"inline_keyboard": rows}
}

// AnswerCallback closes the spinner Telegram shows on a pressed button. An
// unanswered press keeps spinning for seconds and reads as a dead bot, so this
// is sent even when the answer is empty.
func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	payload := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		payload["text"] = text
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := c.call(ctx, "answerCallbackQuery", payload, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("telegram: answerCallbackQuery rejected: %s", resp.Description)
	}
	return nil
}

// Edit replaces the text of a message the bot sent earlier. Telegram refuses an
// edit that changes nothing and one on a message older than 48 hours, so the
// caller must be ready to fall back to a fresh message.
func (c *Client) Edit(ctx context.Context, messageID int64, text string) error {
	return c.EditWithKeyboard(ctx, messageID, text, nil)
}

// EditWithKeyboard rewrites a message and replaces its buttons. A menu that
// walks through steps edits itself rather than posting a message per step: the
// chat keeps one card for one task instead of a trail of dead menus.
func (c *Client) EditWithKeyboard(ctx context.Context, messageID int64, text string, keyboard Keyboard) error {
	payload := map[string]any{
		"chat_id":                  c.chatID,
		"message_id":               messageID,
		"text":                     text,
		"disable_web_page_preview": true,
		// An edit that omits reply_markup leaves the old buttons in place, so a
		// step with no buttons must say so explicitly.
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]any{}},
	}
	if markup := keyboard.markup(); markup != nil {
		payload["reply_markup"] = markup
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := c.call(ctx, "editMessageText", payload, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("telegram: editMessageText rejected: %s", resp.Description)
	}
	return nil
}

// Identity is who Telegram says this bot is. The username is how a human tags
// it in a group; the id is how a reply to one of its own messages is told from
// a reply to somebody else's.
type Identity struct {
	ID       int64
	Username string
}

// GetMe reads the bot's own identity.
func (c *Client) GetMe(ctx context.Context) (Identity, error) {
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := c.call(ctx, "getMe", map[string]any{}, &resp); err != nil {
		return Identity{}, err
	}
	if !resp.OK {
		return Identity{}, fmt.Errorf("telegram: getMe rejected: %s", resp.Description)
	}
	return Identity{ID: resp.Result.ID, Username: resp.Result.Username}, nil
}

// Update is the subset of a Telegram update AO acts on: a text message, the
// chat it came from, and — for a reply — who wrote the message being answered.
// A group chat carries human conversation the bot is not part of, so knowing
// whether a message is addressed to it takes more than the text.
type Update struct {
	ID       int64
	ChatID   string
	ChatType string
	Text     string
	// ReplyToFromID is the author of the message this one replies to; zero when
	// it is not a reply.
	ReplyToFromID int64
	// ReplyToFromIsBot marks a reply to some bot, used only while the bot's own
	// id is still unknown.
	ReplyToFromIsBot bool
	// ReplyToText is what is being replied to, quoted back to the agent so it
	// knows which of its messages the human means.
	ReplyToText string
	// ReplyToMessageID identifies the message being answered. A reply to a
	// question the bot itself asked belongs to that question, not to the agent
	// on duty, and the id is the only thing that tells the two apart.
	ReplyToMessageID int64
	// FromUsername is the @name of whoever produced the update, used to open a
	// reply box for that person alone.
	FromUsername string
	// CallbackID is non-empty when the update is a button press. It must be
	// answered, or Telegram spins on the button.
	CallbackID string
	// CallbackData is the payload of the pressed button.
	CallbackData string
	// MessageID is the message carrying the pressed button, which the bot edits
	// in place so a menu advances instead of piling up.
	MessageID int64
}

// GetUpdates long-polls for messages after offset. timeout is the server-side
// long-poll window; the HTTP deadline is that plus a margin, so a healthy poll
// never trips the client timeout.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	payload := map[string]any{
		"timeout": seconds,
		// Button presses arrive as a separate update type: a poll that does not
		// ask for them leaves every button dead.
		"allowed_updates": []string{"message", "callback_query"},
	}
	if offset > 0 {
		payload["offset"] = offset
	}
	type messagePayload struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		From      *struct {
			Username string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID   json.Number `json:"id"`
			Type string      `json:"type"`
		} `json:"chat"`
		ReplyTo *struct {
			MessageID int64  `json:"message_id"`
			Text      string `json:"text"`
			From      *struct {
				ID    int64 `json:"id"`
				IsBot bool  `json:"is_bot"`
			} `json:"from"`
		} `json:"reply_to_message"`
	}
	var resp struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID      int64           `json:"update_id"`
			Message       *messagePayload `json:"message"`
			CallbackQuery *struct {
				ID   string `json:"id"`
				Data string `json:"data"`
				From *struct {
					Username string `json:"username"`
				} `json:"from"`
				Message *messagePayload `json:"message"`
			} `json:"callback_query"`
		} `json:"result"`
		Description string `json:"description"`
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout+requestTimeout)
	defer cancel()
	// A long poll needs its own deadline, but the same transport: dropping it
	// here would silently bypass the proxy the rest of the client uses.
	poller := &http.Client{Timeout: timeout + requestTimeout, Transport: c.transport}
	if err := c.callWithClient(callCtx, poller, "getUpdates", payload, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram: getUpdates rejected: %s", resp.Description)
	}
	updates := make([]Update, 0, len(resp.Result))
	for _, item := range resp.Result {
		if query := item.CallbackQuery; query != nil {
			update := Update{
				ID:           item.UpdateID,
				CallbackID:   query.ID,
				CallbackData: strings.TrimSpace(query.Data),
			}
			if query.From != nil {
				update.FromUsername = query.From.Username
			}
			if message := query.Message; message != nil {
				update.ChatID = message.Chat.ID.String()
				update.ChatType = message.Chat.Type
				update.MessageID = message.MessageID
			}
			updates = append(updates, update)
			continue
		}
		if item.Message == nil {
			// Still surface the id: the offset must advance past updates AO
			// does not act on, or the same batch is re-read forever.
			updates = append(updates, Update{ID: item.UpdateID})
			continue
		}
		update := Update{
			ID:        item.UpdateID,
			ChatID:    item.Message.Chat.ID.String(),
			ChatType:  item.Message.Chat.Type,
			MessageID: item.Message.MessageID,
			Text:      strings.TrimSpace(item.Message.Text),
		}
		if from := item.Message.From; from != nil {
			update.FromUsername = from.Username
		}
		if reply := item.Message.ReplyTo; reply != nil {
			update.ReplyToText = strings.TrimSpace(reply.Text)
			update.ReplyToMessageID = reply.MessageID
			if reply.From != nil {
				update.ReplyToFromID = reply.From.ID
				update.ReplyToFromIsBot = reply.From.IsBot
			}
		}
		updates = append(updates, update)
	}
	return updates, nil
}

func (c *Client) call(ctx context.Context, method string, payload any, out any) error {
	return c.callWithClient(ctx, c.http, method, payload, out)
}

func (c *Client) callWithClient(ctx context.Context, client *http.Client, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := c.apiBase + "/bot" + url.PathEscape(c.token) + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// net/http puts the full request URL in transport errors, and the bot
		// token lives in that URL. Logs must never carry it.
		return fmt.Errorf("telegram: %s: %s", method, c.scrub(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: %s http %d: %s", method, resp.StatusCode, c.scrub(strings.TrimSpace(string(raw))))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// proxyTransport returns a transport routed through proxyURL, or nil (meaning
// http.DefaultTransport) when no proxy is configured. A malformed URL also
// yields nil rather than failing construction: the daemon must still boot, and
// the misconfiguration surfaces on the first request as the underlying network
// error instead of a silent no-notifier state.
func proxyTransport(proxyURL string) http.RoundTripper {
	raw := strings.TrimSpace(proxyURL)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil
	}
	clone := transport.Clone()
	clone.Proxy = http.ProxyURL(parsed)
	return clone
}

// scrub removes the bot token from text bound for a log line. The token is part
// of every request URL, so transport errors quote it verbatim.
func (c *Client) scrub(text string) string {
	if c.token == "" {
		return text
	}
	return strings.ReplaceAll(text, c.token, "<token>")
}
