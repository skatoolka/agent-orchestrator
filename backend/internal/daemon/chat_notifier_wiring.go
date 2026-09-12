package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/notify"
	"github.com/aoagents/agent-orchestrator/backend/internal/notify/telegram"
	trackerintake "github.com/aoagents/agent-orchestrator/backend/internal/observe/trackerintake"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// chatNotifier bundles the opt-in chat side-channel: notifications out, control
// commands in, and the pause gate both share with intake.
//
// A disabled notifier (no token configured) is a live zero value rather than
// nil, so call sites stay branch-free: the publisher is nil, Announce is a
// no-op, and the gate still works for other pause sources.
type chatNotifier struct {
	client   *telegram.Client
	pub      *telegram.Publisher
	gate     *trackerintake.Gate
	conveyor telegram.Conveyor
	answers  *answerSlots
	logger   *slog.Logger
}

// startChatNotifier builds the notifier from the environment and starts its
// delivery worker. Without AO_TELEGRAM_BOT_TOKEN it returns an inert notifier
// and nothing ever leaves the machine.
func startChatNotifier(ctx context.Context, logger *slog.Logger) *chatNotifier {
	notifier := &chatNotifier{gate: &trackerintake.Gate{}, answers: &answerSlots{}, logger: logger}
	client, ok := telegram.NewFromEnv()
	if !ok {
		return notifier
	}
	notifier.client = client
	notifier.pub = telegram.NewPublisher(client, logger)
	notifier.pub.Start(ctx)
	logger.Info("chat notifier enabled", "transport", "telegram")
	return notifier
}

// publisher returns the notify.Publisher to fan out to, or nil when disabled.
// A typed nil would satisfy the interface while claiming to be non-nil, so the
// nil case is returned explicitly.
func (c *chatNotifier) publisher() notify.Publisher {
	if c == nil || c.pub == nil {
		return nil
	}
	return c.pub
}

// Announce implements trackerintake.Announcer. Safe on a disabled notifier.
func (c *chatNotifier) Announce(text string) {
	if c == nil || c.pub == nil {
		return
	}
	c.pub.Announce(text)
}

// intakeGate hands intake its pause switch.
func (c *chatNotifier) intakeGate() *trackerintake.Gate {
	if c == nil {
		return nil
	}
	return c.gate
}

// attachConveyor hands the bot the backlog surface. Intake is built after the
// notifier (the notifier owns the pause gate intake needs), so the dependency
// arrives in a second step rather than through the constructor.
func (c *chatNotifier) attachConveyor(observer *trackerintake.Observer) {
	if c == nil || observer == nil {
		return
	}
	c.conveyor = intakeConveyor{observer: observer}
}

// intakeConveyor translates intake's types into the chat-facing ones.
type intakeConveyor struct {
	observer *trackerintake.Observer
}

func (i intakeConveyor) Queue(ctx context.Context) ([]telegram.QueueItem, error) {
	queued, err := i.observer.Queue(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]telegram.QueueItem, 0, len(queued))
	for _, item := range queued {
		items = append(items, telegram.QueueItem{Project: item.ProjectID, Issue: item.Issue, Title: item.Title})
	}
	return items, nil
}

func (i intakeConveyor) Claim(ctx context.Context, ref string) (telegram.ClaimResult, error) {
	result, err := i.observer.Claim(ctx, ref)
	if err != nil {
		return telegram.ClaimResult{}, err
	}
	return telegram.ClaimResult{SessionID: result.SessionID, Issue: result.Issue, Title: result.Title}, nil
}

// startBot launches the command loop. It runs after the store and session
// service exist, since /status and /kill need both. Returns nil when chat is
// not configured, which the daemon's shutdown path treats as "nothing to wait
// for".
func (c *chatNotifier) startBot(ctx context.Context, store *sqlite.Store, sessions *sessionsvc.Service, duty *orchestratorEscalator, runtime paneReader) <-chan struct{} {
	if c == nil || c.client == nil {
		return nil
	}
	// A nil escalator must stay a nil interface: wrapped in one, the bot would
	// believe it has somewhere to send questions.
	var desk telegram.Duty
	if duty != nil {
		desk = dutyDesk{escalator: duty, answers: c.answers}
	}
	bot := telegram.NewBot(telegram.Deps{
		Client:   c.client,
		Sessions: store,
		Killer:   chatSessionKiller{sessions: sessions},
		Gate:     c.gate,
		Conveyor: c.conveyor,
		Duty:     desk,
		Auth:     claudeAuthScript{},
		Spawner:  chatSpawner{store: store, sessions: sessions},
		Remote:   chatRemoteControlFor(store, sessions, runtime, c.logger),
		Links:    chatLinks(),
		Logger:   c.logger,
	})
	return bot.Start(ctx)
}

// chatSpawner starts a session for a task typed into the chat. Unlike a claim,
// there is no card behind it: the prompt is the whole task, which is what makes
// this the path for work that occurs to someone away from the board.
type chatSpawner struct {
	store    *sqlite.Store
	sessions *sessionsvc.Service
}

// Projects lists what the chat may spawn into. Archived projects are left out —
// they are not places work goes any more.
func (s chatSpawner) Projects(ctx context.Context) ([]telegram.Project, error) {
	if s.store == nil {
		return nil, fmt.Errorf("project list is unavailable")
	}
	records, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	projects := make([]telegram.Project, 0, len(records))
	for _, record := range records {
		if !record.ArchivedAt.IsZero() {
			continue
		}
		projects = append(projects, telegram.Project{ID: record.ID, Name: record.DisplayName})
	}
	return projects, nil
}

// Spawn starts a worker on the task. TerminateOnPRMerge matches what intake
// does: a task that ends in a merged PR cleans up its own worktree instead of
// lingering as a live session nobody will return to.
func (s chatSpawner) Spawn(ctx context.Context, projectID, prompt string) (telegram.SpawnResult, error) {
	if s.sessions == nil {
		return telegram.SpawnResult{}, fmt.Errorf("session service is unavailable")
	}
	session, _, _, err := s.sessions.Spawn(ctx, ports.SpawnConfig{
		ProjectID:          domain.ProjectID(projectID),
		Kind:               domain.KindWorker,
		Prompt:             prompt,
		DisplayName:        chatDisplayName(prompt),
		TerminateOnPRMerge: true,
	})
	if err != nil {
		return telegram.SpawnResult{}, err
	}
	return telegram.SpawnResult{SessionID: string(session.ID)}, nil
}

// chatDisplayNameLen mirrors the daemon's own cap on a sidebar label.
const chatDisplayNameLen = 20

// chatDisplayName turns the first line of a task into the sidebar label, so a
// session started from a phone is recognisable on the board.
func chatDisplayName(prompt string) string {
	first := strings.TrimSpace(prompt)
	if line, _, ok := strings.Cut(first, "\n"); ok {
		first = strings.TrimSpace(line)
	}
	runes := []rune(first)
	if len(runes) > chatDisplayNameLen {
		return strings.TrimSpace(string(runes[:chatDisplayNameLen]))
	}
	return first
}

// chatRemoteControlFor wires the Remote Control surface, or returns a nil
// interface when this deployment does not publish sessions to Claude at all:
// with Remote Control off there is no link to lose, and a /rc that reported on
// one would be inventing a problem.
func chatRemoteControlFor(store *sqlite.Store, sessions *sessionsvc.Service, runtime paneReader, logger *slog.Logger) telegram.RemoteControl {
	if claudecode.RemoteControlSessionName("probe") == "" {
		return nil
	}
	return chatRemoteControl{roster: store, inbox: sessions, runtime: runtime, logger: logger}
}

// chatLinks tells the bot where a fresh session can be opened: the public
// dashboard, and Claude when this deployment mirrors sessions there. Both come
// from the deployment's environment, and an unset one simply drops a button.
func chatLinks() telegram.Links {
	return telegram.Links{
		WebBase:           strings.TrimSpace(os.Getenv("AO_WEB_BASE_URL")),
		RemoteControlName: claudecode.RemoteControlSessionName,
		ClaudeURL:         strings.TrimSpace(os.Getenv(envClaudeCodeURL)),
	}
}

// envClaudeCodeURL overrides where Remote Control sessions are driven from.
const envClaudeCodeURL = "AO_CLAUDE_CODE_URL"

// chatAnnounceAPI is the write half of the chat side-channel, mounted at
// POST /api/v1/announce and reached by `ao announce`.
//
// Agents run with the bot token in their environment and could call Telegram
// directly. They must not: routing through the daemon keeps message formation
// in one place and keeps the right to speak as the bot a daemon capability
// rather than an ambient one.
type chatAnnounceAPI struct {
	notifier *chatNotifier
}

func (a chatAnnounceAPI) Announce(_ context.Context, text, session string) error {
	if a.notifier == nil || a.notifier.pub == nil {
		return controllers.ErrChatUnavailable
	}
	// The label is stamped here rather than by the caller: the chat is the
	// daemon's surface, and a reader needs to know which session is talking.
	if session != "" {
		text = "💬 " + session + ": " + text
	}
	if messageID, ok := a.notifier.answers.take(session); ok {
		a.notifier.pub.Replace(messageID, text)
		return nil
	}
	a.notifier.pub.Announce(text)
	return nil
}

// dutyDesk is the bot's view of the agent on duty: where a question goes, and
// where its answer is expected to land.
type dutyDesk struct {
	escalator *orchestratorEscalator
	answers   *answerSlots
}

func (d dutyDesk) Ask(ctx context.Context, text string) (string, error) {
	return d.escalator.Ask(ctx, text)
}

func (d dutyDesk) Await(session string, messageID int64) {
	d.answers.hold(session, messageID)
}

// answerSlotTTL bounds how long a question keeps its place in the chat. An
// answer that arrives later belongs to a conversation nobody is still reading,
// and a fresh message is the honest form for it.
const answerSlotTTL = 30 * time.Minute

// answerSlots remembers, per duty session, the chat message that promised an
// answer. The session's next announce overwrites that message instead of adding
// another one, so a question costs the chat one line rather than two.
type answerSlots struct {
	mu   sync.Mutex
	held map[string]answerSlot
}

type answerSlot struct {
	messageID int64
	expires   time.Time
}

func (s *answerSlots) hold(session string, messageID int64) {
	if s == nil || session == "" || messageID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		s.held = make(map[string]answerSlot)
	}
	s.held[session] = answerSlot{messageID: messageID, expires: time.Now().Add(answerSlotTTL)}
}

// take consumes the slot: only the first message a session sends after a
// question is that question's answer. Everything after it is news of its own.
func (s *answerSlots) take(session string) (int64, bool) {
	if s == nil || session == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.held[session]
	if !ok {
		return 0, false
	}
	delete(s.held, session)
	if time.Now().After(slot.expires) {
		return 0, false
	}
	return slot.messageID, true
}

// chatSessionKiller adapts the session service to the bot's Killer surface.
type chatSessionKiller struct {
	sessions *sessionsvc.Service
}

func (k chatSessionKiller) Kill(ctx context.Context, id domain.SessionID) error {
	_, err := k.sessions.Kill(ctx, id)
	return err
}
