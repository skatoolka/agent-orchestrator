package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	// pollTimeout is the server-side long-poll window. Long enough that an idle
	// conveyor makes ~1 request/minute, short enough that a shutdown is not
	// held up for long.
	pollTimeout = 50 * time.Second
	// errorBackoff throttles retries when Telegram is unreachable, so an outage
	// does not turn into a hot loop.
	errorBackoff = 30 * time.Second
)

// SessionLister is the read surface /status needs.
type SessionLister interface {
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

// Killer terminates a session on /kill.
type Killer interface {
	Kill(ctx context.Context, id domain.SessionID) error
}

// Gate is the intake pause switch shared with the usage-limit backoff.
type Gate interface {
	Pause()
	Resume()
	Paused() bool
}

// QueueItem is one card waiting to be claimed.
type QueueItem struct {
	Project string
	Issue   string
	Title   string
}

// ClaimResult reports the session a manual claim started.
type ClaimResult struct {
	SessionID string
	Issue     string
	Title     string
}

// Conveyor exposes the backlog to the chat: what is queued, and "start this one
// now". It is an interface so the bot stays free of intake internals.
type Conveyor interface {
	Queue(ctx context.Context) ([]QueueItem, error)
	Claim(ctx context.Context, ref string) (ClaimResult, error)
}

// ErrNoDutyAgent reports that no orchestrator session is live to take a
// question. The bot turns it into an answer, because silence in the chat is
// indistinguishable from a broken bot.
var ErrNoDutyAgent = errors.New("telegram: no orchestrator on duty")

// Duty carries a free-form message — a human talking, not a command — to the
// agent on duty, and reports which session took it.
type Duty interface {
	Ask(ctx context.Context, text string) (session string, err error)
	// Await says the chat is holding messageID for this session's answer, so
	// the answer overwrites it instead of adding another message.
	Await(session string, messageID int64)
}

// Bot answers control commands from the configured chat: what is running, what
// is queued, start this card, pause claiming, drop a session. Anything that
// writes code or opens a PR stays in the agent's hands. Whatever is not a
// command goes to the agent on duty.
type Bot struct {
	client *Client
	// identity is filled by the first successful getMe and only ever read and
	// written by the poll goroutine.
	identity Identity
	sessions SessionLister
	killer   Killer
	gate     Gate
	conveyor Conveyor
	duty     Duty
	auth     Auth
	spawner  Spawner
	remote   RemoteControl
	links    Links
	// desk remembers what the buttons of an open menu mean, and which question
	// the bot is still waiting an answer to.
	desk   *desk
	logger *slog.Logger
}

// Deps are the bot's collaborators. Any of them may be nil; the matching
// command then reports that it is unavailable instead of panicking.
type Deps struct {
	Client   *Client
	Sessions SessionLister
	Killer   Killer
	Gate     Gate
	Conveyor Conveyor
	Duty     Duty
	Auth     Auth
	Spawner  Spawner
	Remote   RemoteControl
	Links    Links
	Logger   *slog.Logger
}

// NewBot wires a command bot.
func NewBot(deps Deps) *Bot {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Bot{
		client:   deps.Client,
		sessions: deps.Sessions,
		killer:   deps.Killer,
		gate:     deps.Gate,
		conveyor: deps.Conveyor,
		duty:     deps.Duty,
		auth:     deps.Auth,
		spawner:  deps.Spawner,
		remote:   deps.Remote,
		links:    deps.Links,
		desk:     newDesk(),
		logger:   logger,
	}
}

// Start runs the long-poll loop until ctx is done and returns a channel closed
// when it has stopped.
func (b *Bot) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var offset int64
		for {
			if ctx.Err() != nil {
				return
			}
			b.ensureIdentity(ctx)
			updates, err := b.client.GetUpdates(ctx, offset, pollTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				b.logger.Warn("telegram: poll failed", "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(errorBackoff):
				}
				continue
			}
			for _, update := range updates {
				if update.ID >= offset {
					offset = update.ID + 1
				}
				b.handle(ctx, update)
			}
		}
	}()
	return done
}

// handle runs one command. Updates from any chat other than the configured one
// are dropped without a reply: the bot's username is discoverable, its chat is
// the authorization boundary.
func (b *Bot) handle(ctx context.Context, update Update) {
	if update.CallbackID != "" {
		b.press(ctx, update)
		return
	}
	if update.Text == "" {
		return
	}
	if update.ChatID != b.client.ChatID() {
		b.logger.Warn("telegram: ignoring command from unknown chat", "chat", update.ChatID)
		return
	}
	// A reply to a question the bot asked belongs to that question — including
	// one that starts with a slash, since a task brief may well open with a
	// path or a command the agent is meant to run.
	if pending, ok := b.desk.claimReply(update.ReplyToMessageID); ok {
		b.spawnAnswer(ctx, pending.project, update.Text)
		return
	}
	// Not a command means a human is talking — to the bot, or to the other
	// humans in the chat. Only the first kind is the bot's business.
	if !strings.HasPrefix(update.Text, "/") {
		if !b.addressed(update) {
			return
		}
		reply, session := b.ask(ctx, b.question(update))
		messageID, err := b.client.SendMessage(ctx, reply)
		if err != nil {
			b.logger.Warn("telegram: reply failed", "command", "<question>", "err", err)
			return
		}
		if session != "" && b.duty != nil {
			b.duty.Await(session, messageID)
		}
		return
	}
	command, arg := splitCommand(update.Text)
	// The session menu answers with buttons, so it writes its own message
	// instead of returning text into the shared reply path below.
	if command == "/new" || command == "/spawn" {
		text, keyboard := b.newSessionMenu(ctx, arg)
		if _, err := b.client.SendWithKeyboard(ctx, text, keyboard); err != nil {
			b.logger.Warn("telegram: reply failed", "command", command, "err", err)
		}
		return
	}
	// Bare /rc reports the sessions that lost the link and offers each as a
	// button; with an id it repairs that one.
	if command == "/rc" && strings.TrimSpace(arg) == "" {
		text, keyboard := b.remoteControlReport(ctx)
		if _, err := b.client.SendWithKeyboard(ctx, text, keyboard); err != nil {
			b.logger.Warn("telegram: reply failed", "command", command, "err", err)
		}
		return
	}
	var reply string
	switch command {
	case "/status":
		reply = b.status(ctx)
	case "/pause":
		reply = b.pause()
	case "/resume":
		reply = b.resume()
	case "/kill":
		reply = b.kill(ctx, arg)
	case "/queue":
		reply = b.queue(ctx)
	case "/take":
		reply = b.take(ctx, arg)
	case "/rc":
		reply = b.reconnect(ctx, arg)
	case "/relogin":
		reply = b.relogin(ctx)
	case "/code":
		reply = b.loginCode(ctx, arg)
	case "/help", "/start":
		reply = strings.Join([]string{
			"/new — новая сессия: проект, задача текстом или карточка из Ready — кнопками",
			"/new <задача> — то же одной строкой",
			"/status — сессии и состояние очереди",
			"/queue — что лежит в Ready, по порядку",
			"/take <номер> — взять конкретную задачу сейчас",
			"/pause — не брать новые карточки",
			"/resume — снова брать",
			"/kill <id> — снять сессию",
			"/rc — у кого отвалился Remote Control; /rc <id> — переподключить",
			"/relogin — переподключить авторизацию Claude Code, если агенты встали",
			"/code <код> — вернуть код со страницы входа",
			"",
			"вопрос дежурному агенту — тэгом (@" + b.tag() + ") или реплаем на моё сообщение",
		}, "\n")
	default:
		return
	}
	if err := b.client.Send(ctx, reply); err != nil {
		b.logger.Warn("telegram: reply failed", "command", command, "err", err)
	}
}

func (b *Bot) status(ctx context.Context) string {
	var out strings.Builder
	if b.gate != nil && b.gate.Paused() {
		out.WriteString("⏸ приём карточек на паузе\n\n")
	} else {
		out.WriteString("▶ приём карточек включён\n\n")
	}
	if b.sessions == nil {
		out.WriteString("список сессий недоступен")
		return out.String()
	}
	sessions, err := b.sessions.ListAllSessions(ctx)
	if err != nil {
		return out.String() + "не смог прочитать сессии: " + err.Error()
	}
	var live int
	for _, session := range sessions {
		if session.IsTerminated {
			continue
		}
		live++
		out.WriteString(fmt.Sprintf("• %s [%s] %s", session.ID, session.Activity.State, session.IssueID))
		out.WriteString("\n")
	}
	if live == 0 {
		out.WriteString("живых сессий нет")
	}
	return strings.TrimRight(out.String(), "\n")
}

// queue shows the backlog in claim order, so /take can name a card without
// opening the board in a browser.
func (b *Bot) queue(ctx context.Context) string {
	if b.conveyor == nil {
		return "очередь недоступна"
	}
	items, err := b.conveyor.Queue(ctx)
	if err != nil {
		return "не смог прочитать очередь: " + err.Error()
	}
	if len(items) == 0 {
		return "очередь пуста — в Ready ничего нет"
	}
	var out strings.Builder
	out.WriteString("в очереди (в порядке, в котором будут взяты):\n")
	for i, item := range items {
		out.WriteString(fmt.Sprintf("%d. %s — %s\n", i+1, item.Issue, truncate(item.Title, 60)))
	}
	out.WriteString("\n/take <номер issue> — взять сейчас")
	return strings.TrimRight(out.String(), "\n")
}

// take claims one specific card immediately, ahead of the queue.
func (b *Bot) take(ctx context.Context, arg string) string {
	ref := strings.TrimSpace(arg)
	if ref == "" {
		return "нужен номер issue: /take 53"
	}
	if b.conveyor == nil {
		return "запуск задач недоступен"
	}
	result, err := b.conveyor.Claim(ctx, ref)
	if err != nil {
		return "не смог взять " + ref + ": " + err.Error()
	}
	return fmt.Sprintf("🤖 взял %s — %s\n\nсессия: %s", result.Issue, truncate(result.Title, 60), result.SessionID)
}

// ensureIdentity learns who the bot is. Without its username a tag cannot be
// recognised, and without its id a reply to the bot cannot be told from a reply
// to a human. It is retried on every poll because the daemon usually boots
// before the network is reachable.
func (b *Bot) ensureIdentity(ctx context.Context) {
	if b.identity.Username != "" || b.client == nil {
		return
	}
	me, err := b.client.GetMe(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.logger.Warn("telegram: getMe failed; tags stay unrecognised until it succeeds", "err", err)
		}
		return
	}
	b.identity = me
	b.logger.Info("telegram: bot identity resolved", "username", me.Username, "id", me.ID)
}

// tag renders the bot's @name for help text.
func (b *Bot) tag() string {
	if b.identity.Username == "" {
		return "имя бота"
	}
	return b.identity.Username
}

// addressed reports whether a non-command message is meant for the bot.
//
// The conveyor chat is a room where humans also talk to each other; a bot that
// forwards every line to the agent on duty would turn that conversation into a
// stream of interruptions. So in a group it answers only when tagged or when
// someone replies to one of its own messages. A private chat is nothing but a
// conversation with the bot, and every message there is addressed to it.
func (b *Bot) addressed(update Update) bool {
	if update.ChatType == "private" {
		return true
	}
	if b.repliesToBot(update) {
		return true
	}
	return b.tagged(update.Text)
}

// repliesToBot reports a reply to the bot's own message. Until getMe succeeds
// the bot's id is unknown, and a reply to any bot in the configured chat is
// taken as its own: over-answering beats swallowing a question.
func (b *Bot) repliesToBot(update Update) bool {
	if b.identity.ID != 0 {
		return update.ReplyToFromID == b.identity.ID
	}
	return update.ReplyToFromIsBot
}

// tagged reports an @mention of the bot anywhere in the message. The match must
// end on a word boundary, or @bot would also answer for @bot2.
func (b *Bot) tagged(text string) bool {
	if b.identity.Username == "" {
		return false
	}
	needle := "@" + b.identity.Username
	for i := 0; i+len(needle) <= len(text); i++ {
		if strings.EqualFold(text[i:i+len(needle)], needle) && !nameByte(text, i+len(needle)) {
			return true
		}
	}
	return false
}

// nameByte reports whether position i continues a Telegram username, which is
// made of letters, digits and underscores.
func nameByte(text string, i int) bool {
	if i >= len(text) {
		return false
	}
	c := text[i]
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// question is what the agent on duty actually reads: the message without the
// addressing, plus the line being replied to — the agent has no other way to
// tell which of its own messages the human means.
func (b *Bot) question(update Update) string {
	text := b.stripTag(update.Text)
	if quoted := truncate(update.ReplyToText, 300); quoted != "" && b.repliesToBot(update) {
		if text == "" {
			return "человек ответил на сообщение в чате «" + quoted + "»"
		}
		return "в ответ на сообщение в чате «" + quoted + "»:\n" + text
	}
	return text
}

// stripTag drops the bot's @name where it is pure addressing — at the start or
// the end of the message. A tag inside a sentence is left alone: there it is
// part of what the human wrote.
func (b *Bot) stripTag(text string) string {
	if b.identity.Username == "" {
		return text
	}
	needle := "@" + b.identity.Username
	trimmed := strings.TrimSpace(text)
	for {
		switch {
		case len(trimmed) >= len(needle) && strings.EqualFold(trimmed[:len(needle)], needle) && !nameByte(trimmed, len(needle)):
			trimmed = strings.TrimSpace(trimmed[len(needle):])
		case len(trimmed) >= len(needle) && strings.EqualFold(trimmed[len(trimmed)-len(needle):], needle):
			trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(needle)])
		default:
			return strings.TrimLeft(trimmed, " ,.:;-—\t")
		}
	}
}

// ask hands a human's message to the agent on duty and returns what the chat is
// told, plus the session that took the question — empty when nobody did.
//
// Every branch answers something: an unanswered message in a chat is
// indistinguishable from a dead bot. The answer branch is deliberately a
// placeholder — the agent's reply overwrites it — so one question stays one
// message.
func (b *Bot) ask(ctx context.Context, text string) (string, string) {
	if strings.TrimSpace(text) == "" {
		return "не понял вопрос — напиши, что нужно, тем же сообщением", ""
	}
	if b.duty == nil {
		return "передать некому: дежурный агент не подключён к боту", ""
	}
	session, err := b.duty.Ask(ctx, text)
	switch {
	case errors.Is(err, ErrNoDutyAgent):
		return "дежурного сейчас нет — вопрос никому не ушёл. Подними оркестратора проекта и повтори.", ""
	case err != nil:
		return "не смог передать дежурному: " + err.Error(), ""
	}
	return "…спросил дежурного (" + session + ") — ответ появится здесь же", session
}

func truncate(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

func (b *Bot) pause() string {
	if b.gate == nil {
		return "пауза недоступна"
	}
	b.gate.Pause()
	return "⏸ новые карточки не берём. Живые сессии продолжают работать."
}

func (b *Bot) resume() string {
	if b.gate == nil {
		return "пауза недоступна"
	}
	b.gate.Resume()
	return "▶ снова берём карточки из очереди."
}

func (b *Bot) kill(ctx context.Context, arg string) string {
	id := strings.TrimSpace(arg)
	if id == "" {
		return "нужен id сессии: /kill vibeli-3"
	}
	if b.killer == nil {
		return "kill недоступен"
	}
	if err := b.killer.Kill(ctx, domain.SessionID(id)); err != nil {
		return fmt.Sprintf("не смог снять %s: %v", id, err)
	}
	return fmt.Sprintf("сессия %s снята", id)
}

// splitCommand parses "/kill vibeli-3" and the "/kill@bot_name vibeli-3" form
// Telegram uses in groups.
func splitCommand(text string) (command, arg string) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", ""
	}
	command = fields[0]
	if at := strings.IndexByte(command, '@'); at > 0 {
		command = command[:at]
	}
	return strings.ToLower(command), strings.Join(fields[1:], " ")
}
