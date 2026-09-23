package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Project is one registered project the chat can start a session in.
type Project struct {
	ID string
	// Name is the human label; empty falls back to the id.
	Name string
}

// SpawnResult reports the session a chat-driven spawn started.
type SpawnResult struct {
	SessionID string
}

// Spawner starts a session from a task typed into the chat. It is the half of
// "new session" the board cannot give: a card in Ready already has an issue,
// while a thought someone has on a phone has nothing but text.
type Spawner interface {
	Projects(ctx context.Context) ([]Project, error)
	Spawn(ctx context.Context, projectID, prompt string) (SpawnResult, error)
}

// Links renders the two places a live session can be opened from: the AO web
// dashboard, which shows its terminal, and Claude, where Remote Control mirrors
// the same agent for answering prompts from a phone.
//
// Both are optional. A deployment without a public dashboard, or one that does
// not publish sessions to a Claude account, simply gets fewer buttons — never a
// link that leads nowhere.
type Links struct {
	// WebBase is the public origin of the AO web dashboard, e.g.
	// https://ao-web.example.com. Empty hides the dashboard button.
	WebBase string
	// RemoteControlName renders the Claude Remote Control name of a session, or
	// "" when Remote Control is off. It is a function because the name is built
	// from the same deployment env the agent launcher reads.
	RemoteControlName func(sessionID string) string
	// ClaudeURL is where Remote Control sessions are driven from. Empty means
	// the default claude.ai/code.
	ClaudeURL string
}

const defaultClaudeURL = "https://claude.ai/code"

// sessionURL is the deep link to one session's terminal. The dashboard is a
// hash-routed SPA, so the session is addressed after the "#".
func (l Links) sessionURL(sessionID string) string {
	base := strings.TrimRight(strings.TrimSpace(l.WebBase), "/")
	if base == "" || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	return base + "/#/sessions/" + strings.TrimSpace(sessionID)
}

// remoteControl reports the Claude session name and where to drive it, or empty
// strings when Remote Control is not published for this deployment.
func (l Links) remoteControl(sessionID string) (name, url string) {
	if l.RemoteControlName == nil {
		return "", ""
	}
	name = strings.TrimSpace(l.RemoteControlName(sessionID))
	if name == "" {
		return "", ""
	}
	url = strings.TrimSpace(l.ClaudeURL)
	if url == "" {
		url = defaultClaudeURL
	}
	return name, url
}

// buttons renders the "open it" row for a session.
func (l Links) buttons(sessionID string) []InlineButton {
	var row []InlineButton
	if url := l.sessionURL(sessionID); url != "" {
		row = append(row, InlineButton{Text: "🖥 AO web", URL: url})
	}
	if _, url := l.remoteControl(sessionID); url != "" {
		row = append(row, InlineButton{Text: "📱 Claude", URL: url})
	}
	return row
}

// Callback payloads. Telegram caps callback_data at 64 bytes and hands it back
// verbatim, so a button carries a registry key rather than a project id and an
// issue reference that may not fit.
const (
	actionNew     = "new"     // open the menu again
	actionProject = "project" // a project was chosen
	actionAsk     = "ask"     // ask the human for a task in their own words
	actionQueue   = "queue"   // show what is waiting in Ready
	actionTake    = "take"    // claim one card from Ready
	actionCancel  = "cancel"  // close the menu
	// actionReconnect repairs one session's Remote Control link. It rides on a
	// session card rather than the menu, since that is where a human is when
	// they notice the link is gone.
	actionReconnect = "reconnect"
	// actionApprove and actionDecline answer a proposal from the agent on duty.
	// Duty may not spawn sessions itself — it coordinates, and a coordinator
	// that quietly starts work is no longer one — but refusing it any path at
	// all left the person doing the typing: duty would explain what to run and
	// wait for a human to run it. A proposal keeps the decision with the human
	// and the typing with the machine.
	actionApprove = "approve"
	actionDecline = "decline"
)

// action is what one button does when pressed.
type action struct {
	kind    string
	project string
	ref     string
	// prompt carries a task typed alongside the command, so picking a project
	// finishes the spawn instead of asking for the text again.
	prompt string
	// back marks that there were several projects to choose from, and the menu
	// therefore has somewhere to go back to.
	back bool
	born time.Time
}

// actionTTL bounds how long a button stays live. A menu left open overnight is
// answered with "the menu expired" instead of acting on a backlog that has
// moved on since.
const actionTTL = 6 * time.Hour

// maxActions caps the registry so a chat that keeps opening menus cannot grow
// the daemon's memory without bound.
const maxActions = 500

// promptTTL bounds how long the bot waits for the task text it asked for.
const promptTTL = time.Hour

// desk holds the state a button-driven flow needs between two updates: what
// each button means, and which of the bot's own questions is still waiting for
// an answer.
//
// Both live in memory only. A daemon restart drops them, and every lookup
// answers "this is stale, start again" rather than guessing — a button press
// resolved against the wrong state would spawn a session for the wrong task.
type desk struct {
	mu      sync.Mutex
	seq     int64
	actions map[string]action
	prompts map[int64]action
	// waiting is the same question keyed by CHAT, not by the message it was
	// asked in. Telegram pre-opens a reply box, but nothing makes a person use
	// it — in a direct message especially, where the whole chat is one
	// conversation and the obvious move is to just type the next message.
	// Without this the brief went to the agent on duty (the catch-all for
	// unrecognized text), who cannot spawn sessions and says so — and the
	// person sees the bot ignore the task it had just asked for.
	waiting map[string]action
	now     func() time.Time
}

func newDesk() *desk {
	return &desk{actions: map[string]action{}, prompts: map[int64]action{}, waiting: map[string]action{}, now: time.Now}
}

// register stores what a button does and returns its callback payload.
func (d *desk) register(a action) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	a.born = d.now()
	d.sweepLocked()
	d.seq++
	key := strconv.FormatInt(d.seq, 36)
	d.actions[key] = a
	return actionNew + ":" + key
}

// lookup resolves a pressed button, or reports that it is no longer live.
func (d *desk) lookup(data string) (action, bool) {
	key, ok := strings.CutPrefix(strings.TrimSpace(data), actionNew+":")
	if !ok {
		return action{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a, found := d.actions[key]
	if !found || d.now().Sub(a.born) > actionTTL {
		return action{}, false
	}
	return a, true
}

// await records that messageID is a question the bot asked, so the reply to it
// is read as a task brief instead of being forwarded to the agent on duty.
func (d *desk) await(messageID int64, a action) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a.born = d.now()
	d.sweepLocked()
	d.prompts[messageID] = a
}

// awaitChat records that this chat owes the bot a task brief, so the next plain
// message counts even when it is not a reply. One outstanding question per
// chat: a second /new replaces the first rather than queueing behind it.
func (d *desk) awaitChat(chat string, a action) {
	if strings.TrimSpace(chat) == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a.born = d.now()
	d.sweepLocked()
	d.waiting[chat] = a
}

// claimChat takes the question this chat owes an answer to, removing it.
func (d *desk) claimChat(chat string) (action, bool) {
	if strings.TrimSpace(chat) == "" {
		return action{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.waiting[chat]
	if !ok {
		return action{}, false
	}
	delete(d.waiting, chat)
	if d.now().Sub(a.born) > promptTTL {
		return action{}, false
	}
	return a, true
}

// claimReply takes the pending question a reply answers, removing it: one
// question is answered once.
func (d *desk) claimReply(messageID int64) (action, bool) {
	if messageID == 0 {
		return action{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.prompts[messageID]
	if !ok {
		return action{}, false
	}
	delete(d.prompts, messageID)
	if d.now().Sub(a.born) > promptTTL {
		return action{}, false
	}
	return a, true
}

// sweepLocked drops expired entries, and — when a chat has been busy enough to
// blow past the cap — the whole registry rather than half of it: a partial
// eviction would leave live menus whose buttons silently do nothing.
func (d *desk) sweepLocked() {
	now := d.now()
	for key, a := range d.actions {
		if now.Sub(a.born) > actionTTL {
			delete(d.actions, key)
		}
	}
	for id, a := range d.prompts {
		if now.Sub(a.born) > promptTTL {
			delete(d.prompts, id)
		}
	}
	for chat, a := range d.waiting {
		if now.Sub(a.born) > promptTTL {
			delete(d.waiting, chat)
		}
	}
	if len(d.actions) > maxActions {
		d.actions = map[string]action{}
	}
	if len(d.prompts) > maxActions {
		d.prompts = map[int64]action{}
	}
	if len(d.waiting) > maxActions {
		d.waiting = map[string]action{}
	}
}

// newSessionMenu is the first step: pick a project. With a single project there
// is nothing to pick, so it skips straight to the second step — a menu whose
// only purpose is one button wastes a tap.
func (b *Bot) newSessionMenu(ctx context.Context, prompt string) (string, Keyboard) {
	if b.spawner == nil {
		return "запуск сессий недоступен", nil
	}
	projects, err := b.spawner.Projects(ctx)
	if err != nil {
		return "не смог прочитать проекты: " + err.Error(), nil
	}
	switch len(projects) {
	case 0:
		return "проектов нет — сначала зарегистрируй проект в AO", nil
	case 1:
		if strings.TrimSpace(prompt) != "" {
			return b.spawnFromPrompt(ctx, projects[0].ID, prompt)
		}
		return b.projectMenu(projects[0].ID, false)
	}
	var row []InlineButton
	for _, project := range projects {
		row = append(row, InlineButton{Text: projectLabel(project), Data: b.desk.register(action{kind: actionProject, project: project.ID, prompt: prompt, back: true})})
	}
	return "🤖 Новая сессия\n\nВ каком проекте?", Keyboard{row, b.cancelRow()}
}

// projectMenu is the second step: how the task is going to be stated. back
// carries the way home only when there was a choice to go back to.
func (b *Bot) projectMenu(project string, back bool) (string, Keyboard) {
	keyboard := Keyboard{{
		{Text: "✍️ Задача текстом", Data: b.desk.register(action{kind: actionAsk, project: project, back: back})},
		{Text: "📋 Из очереди", Data: b.desk.register(action{kind: actionQueue, project: project, back: back})},
	}}
	tail := b.cancelRow()
	if back {
		tail = append([]InlineButton{{Text: "← проекты", Data: b.desk.register(action{kind: actionNew})}}, tail...)
	}
	return "🤖 Новая сессия · " + project + "\n\nКак ставим задачу?", append(keyboard, tail)
}

// queueMenu is the board as buttons: the same cards /queue prints, each one
// tappable.
func (b *Bot) queueMenu(ctx context.Context, project string, back bool) (string, Keyboard) {
	if b.conveyor == nil {
		return "очередь недоступна", b.backKeyboard(project, back)
	}
	items, err := b.conveyor.Queue(ctx)
	if err != nil {
		return "не смог прочитать очередь: " + err.Error(), b.backKeyboard(project, back)
	}
	var (
		out     strings.Builder
		buttons Keyboard
	)
	out.WriteString("📋 Ready · " + project + "\n\n")
	shown := 0
	for _, item := range items {
		if item.Project != "" && item.Project != project {
			continue
		}
		shown++
		out.WriteString(fmt.Sprintf("%d. %s — %s\n", shown, item.Issue, truncate(item.Title, 60)))
		buttons = append(buttons, []InlineButton{{
			Text: issueLabel(item),
			Data: b.desk.register(action{kind: actionTake, project: project, ref: item.Issue, back: back}),
		}})
		// Telegram renders a long keyboard as a scrolling wall; the rest of the
		// backlog stays one /queue away.
		if shown == 8 {
			break
		}
	}
	if shown == 0 {
		return "в Ready ничего нет для " + project, b.backKeyboard(project, back)
	}
	return strings.TrimRight(out.String(), "\n"), append(buttons, b.backRow(project, back))
}

func (b *Bot) backRow(project string, back bool) []InlineButton {
	return append([]InlineButton{{Text: "← назад", Data: b.desk.register(action{kind: actionProject, project: project, back: back})}}, b.cancelRow()...)
}

func (b *Bot) backKeyboard(project string, back bool) Keyboard {
	return Keyboard{b.backRow(project, back)}
}

func (b *Bot) cancelRow() []InlineButton {
	return []InlineButton{{Text: "отмена", Data: b.desk.register(action{kind: actionCancel})}}
}

// askForTask opens a reply box for the task text. The question is a separate
// message rather than an edit of the menu: Telegram only pre-opens the reply
// box for a message it has just delivered.
func (b *Bot) askForTask(ctx context.Context, chat, project, mention string, back bool) (string, Keyboard) {
	text := "✍️ Ответь на это сообщение текстом задачи для " + project +
		" — или просто пришли задачу следующим сообщением."
	if mention != "" {
		text = "@" + mention + ", " + text
	}
	messageID, err := b.client.Ask(ctx, text, mention)
	if err != nil {
		b.logger.Warn("telegram: prompt request failed", "err", err)
		return "не смог спросить задачу: " + err.Error(), b.backKeyboard(project, back)
	}
	// Оба ожидания сразу: по сообщению — для того, кто воспользовался полем
	// ответа, по чату — для того, кто просто напечатал следующее сообщение.
	// Второе и есть обычное поведение в личке, а раньше такой текст уходил
	// дежурному, который сессии не спавнит.
	pending := action{kind: actionAsk, project: project}
	b.desk.await(messageID, pending)
	b.desk.awaitChat(chat, pending)
	return "🤖 Новая сессия · " + project + "\n\nЖду текст задачи: ответом на сообщение ниже или просто следующим сообщением.", nil
}

// spawnFromPrompt starts the session and hands back the two ways into it.
func (b *Bot) spawnFromPrompt(ctx context.Context, project, prompt string) (string, Keyboard) {
	task := strings.TrimSpace(prompt)
	if task == "" {
		return "пустая задача — напиши, что нужно сделать", nil
	}
	if b.spawner == nil {
		return "запуск сессий недоступен", nil
	}
	result, err := b.spawner.Spawn(ctx, project, task)
	if err != nil {
		return "не смог запустить сессию в " + project + ": " + err.Error(), nil
	}
	return b.sessionCard(project, result.SessionID, task), b.sessionKeyboard(result.SessionID)
}

// takeFromQueue claims one card and answers with the same card as a typed task,
// so both paths out of the menu end in one shape.
func (b *Bot) takeFromQueue(ctx context.Context, project, ref string) (string, Keyboard) {
	if b.conveyor == nil {
		return "запуск задач недоступен", nil
	}
	result, err := b.conveyor.Claim(ctx, ref)
	if err != nil {
		return "не смог взять " + ref + ": " + err.Error(), nil
	}
	return b.sessionCard(project, result.SessionID, result.Issue+" — "+truncate(result.Title, 60)), b.sessionKeyboard(result.SessionID)
}

// sessionCard is what the chat is left with: which session took the task, and
// the name it answers to in Claude. The name is spelled out even though a
// button links to Claude — the button opens the list, and the list is where the
// name is needed.
func (b *Bot) sessionCard(project, sessionID, task string) string {
	var out strings.Builder
	out.WriteString("🤖 " + sessionID + " запущена · " + project + "\n\n")
	out.WriteString(truncate(task, 300))
	if name, _ := b.links.remoteControl(sessionID); name != "" {
		out.WriteString("\n\nClaude Code: " + name)
	}
	return out.String()
}

// sessionKeyboard offers the session's terminal and its Claude twin, plus a way
// straight into starting another one.
func (b *Bot) sessionKeyboard(sessionID string) Keyboard {
	var keyboard Keyboard
	if row := b.links.buttons(sessionID); len(row) > 0 {
		keyboard = append(keyboard, row)
	}
	tail := []InlineButton{{Text: "+ ещё сессия", Data: b.desk.register(action{kind: actionNew})}}
	// The card outlives the spawn, and a link to Claude that dropped an hour
	// later is repaired from right here.
	if name, _ := b.links.remoteControl(sessionID); name != "" {
		tail = append([]InlineButton{{Text: "🔌 переподключить", Data: b.desk.register(action{kind: actionReconnect, ref: sessionID})}}, tail...)
	}
	return append(keyboard, tail)
}

// projectLabel is the button text for a project.
func projectLabel(project Project) string {
	if name := strings.TrimSpace(project.Name); name != "" && name != project.ID {
		return truncate(name, 24)
	}
	return truncate(project.ID, 24)
}

// issueLabel keeps a card's number and enough of its title to recognise it
// inside a button.
func issueLabel(item QueueItem) string {
	number := item.Issue
	if _, after, ok := strings.Cut(item.Issue, "#"); ok {
		number = "#" + after
	}
	title := truncate(item.Title, 28)
	if title == "" {
		return number
	}
	return number + " " + title
}

// Proposal is an action the agent on duty asks a human to authorize.
type Proposal struct {
	// Project the session would start in.
	Project string
	// Prompt is the task brief the session would be given.
	Prompt string
	// Session names the duty session that proposed it, for the label.
	Session string
	// Reason is why duty thinks this should happen — one line, shown above the
	// buttons. A proposal without it is answerable only by guessing.
	Reason string
}

// Propose posts a proposal as a card with confirm/decline buttons. It returns
// once the card is in the chat: nothing is started until a human presses, and
// that is the whole point — duty gets hands, the human keeps the decision.
func (b *Bot) Propose(ctx context.Context, p Proposal) error {
	project := strings.TrimSpace(p.Project)
	prompt := strings.TrimSpace(p.Prompt)
	if project == "" || prompt == "" {
		return errors.New("telegram: proposal needs a project and a task")
	}
	head := "🙋 Дежурный предлагает запустить сессию"
	if session := strings.TrimSpace(p.Session); session != "" {
		head = "🙋 " + session + " предлагает запустить сессию"
	}
	text := head + " · " + project + "\n\n" + truncate(prompt, 600)
	if reason := strings.TrimSpace(p.Reason); reason != "" {
		text += "\n\nЗачем: " + truncate(reason, 300)
	}
	keyboard := Keyboard{{
		{Text: "✅ Запустить", Data: b.desk.register(action{kind: actionApprove, project: project, prompt: prompt})},
		{Text: "✖️ Отклонить", Data: b.desk.register(action{kind: actionDecline, project: project})},
	}}
	if _, err := b.client.SendWithKeyboard(ctx, text, keyboard); err != nil {
		return err
	}
	return nil
}

// press routes a button. Every branch ends in a rewrite of the menu message, so
// the chat holds one card per task rather than a trail of dead menus.
func (b *Bot) press(ctx context.Context, update Update) {
	// The same rule as for commands: the configured chat is the authorization
	// boundary, and an unknown one gets no answer at all — not even the
	// acknowledgement that stops Telegram's spinner.
	if !b.client.AllowsChat(update.ChatID) {
		b.logger.Warn("telegram: ignoring button press from unknown chat", "chat", update.ChatID)
		return
	}
	ctx = WithChat(ctx, update.ChatID)
	act, ok := b.desk.lookup(update.CallbackData)
	if !ok {
		// A daemon restart, or a menu left open for hours: the button's meaning
		// is gone, and guessing it could spawn the wrong task.
		b.acknowledge(ctx, update.CallbackID, "меню устарело")
		b.rewrite(ctx, update.MessageID, "меню устарело — набери /new", nil)
		return
	}
	b.acknowledge(ctx, update.CallbackID, "")
	switch act.kind {
	case actionNew:
		text, keyboard := b.newSessionMenu(ctx, act.prompt)
		b.rewrite(ctx, update.MessageID, text, keyboard)
	case actionProject:
		// A task typed with the command needs nothing more than the project it
		// belongs to, so choosing one finishes the spawn.
		if strings.TrimSpace(act.prompt) != "" {
			b.rewrite(ctx, update.MessageID, "⏳ запускаю сессию в "+act.project+"…", nil)
			text, keyboard := b.spawnFromPrompt(ctx, act.project, act.prompt)
			b.rewrite(ctx, update.MessageID, text, keyboard)
			return
		}
		text, keyboard := b.projectMenu(act.project, act.back)
		b.rewrite(ctx, update.MessageID, text, keyboard)
	case actionAsk:
		text, keyboard := b.askForTask(ctx, update.ChatID, act.project, update.FromUsername, act.back)
		b.rewrite(ctx, update.MessageID, text, keyboard)
	case actionQueue:
		text, keyboard := b.queueMenu(ctx, act.project, act.back)
		b.rewrite(ctx, update.MessageID, text, keyboard)
	case actionTake:
		// Claiming a card reaches the tracker and then starts an agent; saying
		// so first keeps the menu from looking frozen for those seconds.
		b.rewrite(ctx, update.MessageID, "⏳ беру "+act.ref+"…", nil)
		text, keyboard := b.takeFromQueue(ctx, act.project, act.ref)
		b.rewrite(ctx, update.MessageID, text, keyboard)
	case actionReconnect:
		// A fresh message rather than a rewrite: the card this button sits on
		// carries the links into the session, and repairing the Claude side
		// must not cost them.
		if _, err := b.client.SendMessage(ctx, b.reconnect(ctx, act.ref)); err != nil {
			b.logger.Warn("telegram: reply failed", "command", "/rc", "err", err)
		}
	case actionApprove:
		// The same path a menu spawn takes: approval changes who decided, not
		// what happens, so there is one way sessions start and one place it
		// can break.
		b.rewrite(ctx, update.MessageID, "⏳ запускаю сессию в "+act.project+"…", nil)
		text, keyboard := b.spawnFromPrompt(ctx, act.project, act.prompt)
		b.rewrite(ctx, update.MessageID, text, keyboard)
	case actionDecline:
		// The text stays in the chat above this line, so a declined proposal
		// remains readable — and re-runnable by hand — instead of vanishing.
		b.rewrite(ctx, update.MessageID, "✖️ отклонено", nil)
	case actionCancel:
		b.rewrite(ctx, update.MessageID, "отменено", nil)
	}
}

// spawnAnswer turns the task text a human replied with into a session. The
// waiting line is posted first and then rewritten, so one task is one message
// in the chat however long the spawn takes.
func (b *Bot) spawnAnswer(ctx context.Context, project, prompt string) {
	messageID, err := b.client.SendMessage(ctx, "⏳ запускаю сессию в "+project+"…")
	if err != nil {
		b.logger.Warn("telegram: reply failed", "command", "/new", "err", err)
	}
	text, keyboard := b.spawnFromPrompt(ctx, project, prompt)
	if messageID == 0 {
		if _, err := b.client.SendWithKeyboard(ctx, text, keyboard); err != nil {
			b.logger.Warn("telegram: reply failed", "command", "/new", "err", err)
		}
		return
	}
	b.rewrite(ctx, messageID, text, keyboard)
}

// rewrite replaces a message in place, falling back to a new one when Telegram
// refuses the edit: a session that started must be reported even if the card it
// was started from is gone.
func (b *Bot) rewrite(ctx context.Context, messageID int64, text string, keyboard Keyboard) {
	if messageID != 0 {
		if err := b.client.EditWithKeyboard(ctx, messageID, text, keyboard); err == nil {
			return
		} else {
			b.logger.Warn("telegram: menu edit failed", "err", err)
		}
	}
	if _, err := b.client.SendWithKeyboard(ctx, text, keyboard); err != nil {
		b.logger.Warn("telegram: reply failed", "command", "/new", "err", err)
	}
}

// acknowledge stops the spinner on the pressed button.
func (b *Bot) acknowledge(ctx context.Context, callbackID, text string) {
	if callbackID == "" {
		return
	}
	if err := b.client.AnswerCallback(ctx, callbackID, text); err != nil {
		b.logger.Warn("telegram: callback acknowledgement failed", "err", err)
	}
}
