package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAPI records sendMessage payloads and serves canned getUpdates batches.
type fakeAPI struct {
	mu       sync.Mutex
	sent     []sentMessage
	edits    []editCall
	answered []string
	editErr  bool
	updates  [][]byte
	updateIx int
}

// sentMessage is one sendMessage, with the buttons it carried and the chat it
// was addressed to. The chat matters as much as the text: a reply with the
// right words in the wrong chat is invisible to the person who asked.
type sentMessage struct {
	text   string
	chat   string
	markup map[string]any
}

// editCall is one editMessageText the bot issued.
type editCall struct {
	messageID int64
	text      string
	markup    map[string]any
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var body struct {
				ChatID string         `json:"chat_id"`
				Text   string         `json:"text"`
				Markup map[string]any `json:"reply_markup"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.sent = append(f.sent, sentMessage{text: body.Text, chat: body.ChatID, markup: body.Markup})
			id := len(f.sent)
			f.mu.Unlock()
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, id)))
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			var body struct {
				CallbackID string `json:"callback_query_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.answered = append(f.answered, body.CallbackID)
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var body struct {
				MessageID int64          `json:"message_id"`
				Text      string         `json:"text"`
				Markup    map[string]any `json:"reply_markup"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			refuse := f.editErr
			if !refuse {
				f.edits = append(f.edits, editCall{messageID: body.MessageID, text: body.Text, markup: body.Markup})
			}
			f.mu.Unlock()
			if refuse {
				_, _ = w.Write([]byte(`{"ok":false,"description":"message to edit not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1000,"username":"vibeli_ao_bot","is_bot":true}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			f.mu.Lock()
			var batch []byte
			if f.updateIx < len(f.updates) {
				batch = f.updates[f.updateIx]
				f.updateIx++
			} else {
				batch = []byte(`{"ok":true,"result":[]}`)
			}
			f.mu.Unlock()
			_, _ = w.Write(batch)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAPI) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	texts := make([]string, 0, len(f.sent))
	for _, message := range f.sent {
		texts = append(texts, message.text)
	}
	return texts
}

// posts returns the sent messages with their buttons.
func (f *fakeAPI) posts() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

// acknowledgements returns the callback ids the bot answered.
func (f *fakeAPI) acknowledgements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answered...)
}

func (f *fakeAPI) rewrites() []editCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]editCall(nil), f.edits...)
}

func newTestClient(t *testing.T, api *fakeAPI) *Client {
	t.Helper()
	return New(Config{Token: "tok", ChatID: "42", APIBase: api.server(t).URL})
}

// newTestClientWithExtras is newTestClient plus the chats allowed besides the
// main one (AO_TELEGRAM_EXTRA_CHATS in a deployment).
func newTestClientWithExtras(t *testing.T, api *fakeAPI, extras string) *Client {
	t.Helper()
	return New(Config{Token: "tok", ChatID: "42", ExtraChats: extras, APIBase: api.server(t).URL})
}

func TestSendPostsToTheConfiguredChat(t *testing.T) {
	api := &fakeAPI{}
	client := newTestClient(t, api)

	if err := client.Send(context.Background(), "привет"); err != nil {
		t.Fatal(err)
	}
	if got := api.messages(); len(got) != 1 || got[0] != "привет" {
		t.Fatalf("sent = %#v, want [привет]", got)
	}
}

func TestNewFromEnvNeedsBothTokenAndChat(t *testing.T) {
	t.Setenv(EnvBotToken, "tok")
	t.Setenv(EnvChatID, "")
	if _, ok := NewFromEnv(); ok {
		t.Fatal("a token without a chat id must not enable the notifier")
	}
	t.Setenv(EnvChatID, "42")
	if _, ok := NewFromEnv(); !ok {
		t.Fatal("token + chat id must enable the notifier")
	}
}

func waitForMessages(t *testing.T, api *fakeAPI, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := api.messages(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return api.messages()
}

func TestPublisherSendsCreatedNotifications(t *testing.T) {
	api := &fakeAPI{}
	pub := NewPublisher(newTestClient(t, api), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub.Start(ctx)

	record := domain.NotificationRecord{
		ID:        "ntf_1",
		SessionID: "vibeli-3",
		ProjectID: "vibeli",
		PRURL:     "https://github.com/acme/demo/pull/7",
		Type:      domain.NotificationReadyToMerge,
		Title:     "PR #7 is ready to merge",
		CreatedAt: time.Now(),
	}
	if err := pub.Publish(ctx, domain.NotificationEvent{Kind: domain.NotificationCreated, Record: record}); err != nil {
		t.Fatal(err)
	}

	got := waitForMessages(t, api, 1)
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want 1", len(got))
	}
	for _, want := range []string{"ready to merge", "vibeli-3", "https://github.com/acme/demo/pull/7"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("message missing %q:\n%s", want, got[0])
		}
	}
}

func TestPublisherIgnoresResolvedEvents(t *testing.T) {
	api := &fakeAPI{}
	pub := NewPublisher(newTestClient(t, api), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub.Start(ctx)

	if err := pub.Publish(ctx, domain.NotificationEvent{
		Kind:   domain.NotificationResolved,
		Record: domain.NotificationRecord{SessionID: "vibeli-3", Type: domain.NotificationNeedsInput, Title: "resolved"},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := api.messages(); len(got) != 0 {
		t.Fatalf("a resolution means the human already acted; nothing should be sent: %#v", got)
	}
}

// --- bot -------------------------------------------------------------------

type fakeSessions struct{ sessions []domain.SessionRecord }

func (f fakeSessions) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return f.sessions, nil
}

type fakeKiller struct {
	mu     sync.Mutex
	killed []domain.SessionID
}

func (f *fakeKiller) Kill(_ context.Context, id domain.SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, id)
	return nil
}

func (f *fakeKiller) list() []domain.SessionID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.SessionID(nil), f.killed...)
}

type fakeGate struct{ paused bool }

func (f *fakeGate) Pause()       { f.paused = true }
func (f *fakeGate) Resume()      { f.paused = false }
func (f *fakeGate) Paused() bool { return f.paused }

// updateBatch is one message in the conveyor's group chat — the shape the bot
// sees in production.
func updateBatch(id int64, chat, text string) []byte {
	return messageBatch(id, chat, "supergroup", map[string]any{"text": text})
}

// replyBatch is a message answering someone else's, identified by author id.
func replyBatch(id int64, chat, text string, replyFrom int64, replyText string) []byte {
	return messageBatch(id, chat, "supergroup", map[string]any{
		"text": text,
		"reply_to_message": map[string]any{
			"text": replyText,
			"from": map[string]any{"id": replyFrom, "is_bot": replyFrom == 1000},
		},
	})
}

// privateBatch is a one-to-one chat with the bot.
func privateBatch(id int64, chat, text string) []byte {
	return messageBatch(id, chat, "private", map[string]any{"text": text})
}

func messageBatch(id int64, chat, chatType string, message map[string]any) []byte {
	message["chat"] = map[string]any{"id": json.RawMessage(chat), "type": chatType}
	payload := map[string]any{
		"ok": true,
		"result": []map[string]any{{
			"update_id": id,
			"message":   message,
		}},
	}
	out, _ := json.Marshal(payload)
	return out
}

type fakeConveyor struct {
	items   []QueueItem
	claimed []string
	err     error
}

func (f *fakeConveyor) Queue(context.Context) ([]QueueItem, error) { return f.items, f.err }

func (f *fakeConveyor) Claim(_ context.Context, ref string) (ClaimResult, error) {
	if f.err != nil {
		return ClaimResult{}, f.err
	}
	f.claimed = append(f.claimed, ref)
	return ClaimResult{SessionID: "vibeli-9", Issue: "acme/demo#" + ref, Title: "починить"}, nil
}

func runBot(t *testing.T, api *fakeAPI, sessions SessionLister, killer Killer, gate Gate) {
	t.Helper()
	runBotWithConveyor(t, api, sessions, killer, gate, nil)
}

func runBotWithConveyor(t *testing.T, api *fakeAPI, sessions SessionLister, killer Killer, gate Gate, conveyor Conveyor) {
	t.Helper()
	runBotWithDuty(t, api, sessions, killer, gate, conveyor, nil)
}

func runBotWithDuty(t *testing.T, api *fakeAPI, sessions SessionLister, killer Killer, gate Gate, conveyor Conveyor, duty Duty) {
	t.Helper()
	runBotWithAuth(t, api, sessions, killer, gate, conveyor, duty, nil)
}

func runBotWithAuth(t *testing.T, api *fakeAPI, sessions SessionLister, killer Killer, gate Gate, conveyor Conveyor, duty Duty, auth Auth) {
	t.Helper()
	runBotWithDeps(t, Deps{
		Client:   newTestClient(t, api),
		Sessions: sessions,
		Killer:   killer,
		Gate:     gate,
		Conveyor: conveyor,
		Duty:     duty,
		Auth:     auth,
	})
}

// runBotWithDeps starts a bot from an explicit dependency set, for the flows
// that need one the telescoping helpers above do not carry.
func runBotWithDeps(t *testing.T, deps Deps) {
	t.Helper()
	if deps.Logger == nil {
		deps.Logger = discardLogger()
	}
	bot := NewBot(deps)
	ctx, cancel := context.WithCancel(context.Background())
	done := bot.Start(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("bot did not stop after cancellation")
		}
	})
}

func TestBotStatusListsLiveSessions(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/status")}}
	sessions := fakeSessions{sessions: []domain.SessionRecord{
		{ID: "vibeli-3", IssueID: "github:acme/demo#16", Activity: domain.Activity{State: domain.ActivityActive}},
		{ID: "vibeli-1", IssueID: "github:acme/demo#74", IsTerminated: true},
	}}
	runBot(t, api, sessions, &fakeKiller{}, &fakeGate{})

	got := waitForMessages(t, api, 1)
	if len(got) == 0 {
		t.Fatal("no reply to /status")
	}
	if !strings.Contains(got[0], "vibeli-3") {
		t.Errorf("reply must list the live session:\n%s", got[0])
	}
	if strings.Contains(got[0], "vibeli-1") {
		t.Errorf("reply must not list a terminated session:\n%s", got[0])
	}
}

func TestBotPauseFlipsTheGate(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/pause")}}
	gate := &fakeGate{}
	runBot(t, api, fakeSessions{}, &fakeKiller{}, gate)

	waitForMessages(t, api, 1)
	if !gate.paused {
		t.Fatal("/pause must suspend claiming")
	}
}

func TestBotKillTerminatesTheNamedSession(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/kill vibeli-3")}}
	killer := &fakeKiller{}
	runBot(t, api, fakeSessions{}, killer, &fakeGate{})

	waitForMessages(t, api, 1)
	if got := killer.list(); len(got) != 1 || got[0] != "vibeli-3" {
		t.Fatalf("killed = %v, want [vibeli-3]", got)
	}
}

func TestBotIgnoresCommandsFromOtherChats(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "999", "/kill vibeli-3")}}
	killer := &fakeKiller{}
	runBot(t, api, fakeSessions{}, killer, &fakeGate{})

	time.Sleep(100 * time.Millisecond)
	if got := killer.list(); len(got) != 0 {
		t.Fatalf("a command from an unknown chat must be ignored, killed=%v", got)
	}
	if got := api.messages(); len(got) != 0 {
		t.Fatalf("an unknown chat must not even get a reply: %#v", got)
	}
}

// Личка оператора с ботом: он пишет туда, а не в общий чат конвейера. До
// AO_TELEGRAM_EXTRA_CHATS бот такие сообщения молча выбрасывал — апдейты он
// получал, в лог писал «ignoring command from unknown chat», а человек видел
// бота, который не отвечает ничем и никогда.
func TestBotAnswersAnExtraChatInThatChat(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{privateBatch(1, "777", "/kill vibeli-3")}}
	killer := &fakeKiller{}
	runBotWithDeps(t, Deps{
		Client:   newTestClientWithExtras(t, api, "777"),
		Sessions: fakeSessions{},
		Killer:   killer,
		Gate:     &fakeGate{},
	})

	waitForMessages(t, api, 1)
	if got := killer.list(); len(got) != 1 || got[0] != "vibeli-3" {
		t.Fatalf("команда из разрешённой лички обязана исполниться, killed=%v", got)
	}
	// Главное в этом тесте — адрес, а не факт ответа: раньше chat_id брался из
	// конфига, и ответ уехал бы в общий чат «42», где спросивший его не увидит.
	posts := api.posts()
	if len(posts) != 1 || posts[0].chat != "777" {
		t.Fatalf("ответ обязан уйти в чат вопроса, got=%#v", posts)
	}
}

// Список — чтобы добавить человека без пересборки демона; пробелы и хвостовая
// запятая в переменной окружения неизбежны, и пустой id из них получаться не
// должен: он совпал бы с пустым ChatID у апдейта без чата.
func TestBotAllowsEveryChatInTheList(t *testing.T) {
	client := New(Config{Token: "tok", ChatID: "42", ExtraChats: " 777 , 888 ,", APIBase: "http://example.invalid"})
	for _, chat := range []string{"42", "777", "888"} {
		if !client.AllowsChat(chat) {
			t.Fatalf("чат %s обязан быть разрешён", chat)
		}
	}
	for _, chat := range []string{"999", ""} {
		if client.AllowsChat(chat) {
			t.Fatalf("чат %q разрешать нельзя", chat)
		}
	}
}

// Уведомления конвейера — его собственная инициатива, у них нет «чата вопроса».
// Они обязаны идти в главный чат, даже когда разрешены другие: иначе карточки и
// эскалации начали бы приходить туда, где их никто не ждёт.
func TestConveyorNotificationsStayInTheMainChat(t *testing.T) {
	api := &fakeAPI{}
	client := newTestClientWithExtras(t, api, "777")
	if err := client.Send(context.Background(), "карточка взята"); err != nil {
		t.Fatal(err)
	}
	posts := api.posts()
	if len(posts) != 1 || posts[0].chat != "42" {
		t.Fatalf("уведомление обязано уйти в главный чат, got=%#v", posts)
	}
}

// Чат из контекста не берётся на веру: если туда попадёт чужой id (из апдейта,
// который мы отбросили, или по ошибке вызывающего), ответ уйдёт в главный чат,
// а не чужому. Адресация подчиняется тому же правилу, что и допуск команд.
func TestUnknownChatInContextFallsBackToTheMainChat(t *testing.T) {
	api := &fakeAPI{}
	client := newTestClientWithExtras(t, api, "777")
	ctx := WithChat(context.Background(), "999")
	if err := client.Send(ctx, "ответ"); err != nil {
		t.Fatal(err)
	}
	posts := api.posts()
	if len(posts) != 1 || posts[0].chat != "42" {
		t.Fatalf("чужой чат из контекста доверять нельзя, got=%#v", posts)
	}
}

func TestSplitCommandHandlesGroupSuffix(t *testing.T) {
	command, arg := splitCommand("/kill@vibeli_ao_bot vibeli-3")
	if command != "/kill" || arg != "vibeli-3" {
		t.Fatalf("splitCommand = (%q, %q), want (/kill, vibeli-3)", command, arg)
	}
}

func TestProxyTransportRoutesThroughTheProxy(t *testing.T) {
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.Host)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxy.Close()

	// http:// target so the proxy sees an absolute-form request rather than
	// CONNECT, which httptest cannot answer.
	client := New(Config{Token: "tok", ChatID: "42", APIBase: "http://api.telegram.invalid", Proxy: proxy.URL})
	if err := client.Send(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	if len(proxied) != 1 || proxied[0] != "api.telegram.invalid" {
		t.Fatalf("proxy saw %v, want one request for api.telegram.invalid", proxied)
	}
}

func TestNoProxyConfiguredMeansDirect(t *testing.T) {
	if got := proxyTransport("   "); got != nil {
		t.Fatalf("empty proxy must mean direct, got %#v", got)
	}
	if got := proxyTransport("::not a url::"); got != nil {
		t.Fatalf("malformed proxy must not construct a transport, got %#v", got)
	}
}

func TestTransportErrorsNeverCarryTheToken(t *testing.T) {
	// Nothing listens on this port, so client.Do fails and net/http quotes the
	// full request URL — which is where the token lives.
	client := New(Config{Token: "8981925447:SECRET", ChatID: "42", APIBase: "http://127.0.0.1:1"})

	err := client.Send(context.Background(), "ping")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("bot token leaked into the error text: %v", err)
	}
	if !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("token should be redacted in place, got: %v", err)
	}
}

func TestBotQueueListsBacklogInOrder(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/queue")}}
	conveyor := &fakeConveyor{items: []QueueItem{
		{Project: "vibeli", Issue: "acme/demo#46", Title: "SEO оптимизация"},
		{Project: "vibeli", Issue: "acme/demo#51", Title: "действия под сообщением"},
	}}
	runBotWithConveyor(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, conveyor)

	got := waitForMessages(t, api, 1)
	if len(got) == 0 {
		t.Fatal("no reply to /queue")
	}
	first := strings.Index(got[0], "acme/demo#46")
	second := strings.Index(got[0], "acme/demo#51")
	if first < 0 || second < 0 {
		t.Fatalf("reply must list both cards:\n%s", got[0])
	}
	if first > second {
		t.Fatalf("cards must keep claim order:\n%s", got[0])
	}
}

func TestBotQueueReportsEmptyBacklog(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/queue")}}
	runBotWithConveyor(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, &fakeConveyor{})

	got := waitForMessages(t, api, 1)
	if len(got) == 0 || !strings.Contains(got[0], "пуста") {
		t.Fatalf("empty backlog must say so, got %#v", got)
	}
}

func TestBotTakeClaimsTheNamedIssue(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/take 53")}}
	conveyor := &fakeConveyor{}
	runBotWithConveyor(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, conveyor)

	got := waitForMessages(t, api, 1)
	if len(conveyor.claimed) != 1 || conveyor.claimed[0] != "53" {
		t.Fatalf("claimed = %v, want [53]", conveyor.claimed)
	}
	if len(got) == 0 || !strings.Contains(got[0], "vibeli-9") {
		t.Fatalf("reply must name the started session, got %#v", got)
	}
}

func TestBotTakeWithoutArgumentExplainsUsage(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/take")}}
	conveyor := &fakeConveyor{}
	runBotWithConveyor(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, conveyor)

	got := waitForMessages(t, api, 1)
	if len(conveyor.claimed) != 0 {
		t.Fatalf("nothing should be claimed without an argument: %v", conveyor.claimed)
	}
	if len(got) == 0 || !strings.Contains(got[0], "/take 53") {
		t.Fatalf("reply must show the usage, got %#v", got)
	}
}

func TestTruncateCountsRunesNotBytes(t *testing.T) {
	// Cyrillic is 2 bytes per rune: a byte-based cut would slice a character
	// in half and put a replacement glyph in the chat.
	if got := truncate("почини сборку", 6); got != "почини…" {
		t.Fatalf("truncate = %q, want почини…", got)
	}
	if got := truncate("короткий", 20); got != "короткий" {
		t.Fatalf("short text must pass through unchanged, got %q", got)
	}
}

// --- questions to the agent on duty ----------------------------------------

type fakeDuty struct {
	mu       sync.Mutex
	asked    []string
	awaiting map[string]int64
	err      error
}

func (f *fakeDuty) Await(session string, messageID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.awaiting == nil {
		f.awaiting = make(map[string]int64)
	}
	f.awaiting[session] = messageID
}

func (f *fakeDuty) held(session string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.awaiting[session]
}

func (f *fakeDuty) Ask(_ context.Context, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.asked = append(f.asked, text)
	return "vibeli-24", nil
}

func (f *fakeDuty) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func runDutyBot(t *testing.T, api *fakeAPI, duty Duty) {
	t.Helper()
	runBotWithDuty(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, nil, duty)
}

func TestBotRoutesATaggedMessageToTheAgentOnDuty(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "@vibeli_ao_bot почему vibeli-7 стоит?")}}
	duty := &fakeDuty{}
	runDutyBot(t, api, duty)

	got := waitForMessages(t, api, 1)
	if asked := duty.list(); len(asked) != 1 || asked[0] != "почему vibeli-7 стоит?" {
		t.Fatalf("asked = %#v, want the question without the addressing", asked)
	}
	if len(got) == 0 || !strings.Contains(got[0], "vibeli-24") {
		t.Errorf("the reply must name the session that took the question:\n%#v", got)
	}
}

// The chat is a room where humans also talk to each other. A line that is not
// addressed to the bot must not become an interruption for the agent on duty.
func TestBotIgnoresChatterItIsNotAddressedIn(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "я вечером посмотрю #171")}}
	duty := &fakeDuty{}
	runDutyBot(t, api, duty)

	time.Sleep(150 * time.Millisecond)
	if asked := duty.list(); len(asked) != 0 {
		t.Fatalf("an untagged line must not reach the agent: %#v", asked)
	}
	if got := api.messages(); len(got) != 0 {
		t.Fatalf("an untagged line must not even get a reply: %#v", got)
	}
}

func TestBotRoutesAReplyToItsOwnMessage(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{replyBatch(1, "42", "а почему так долго?", 1000, "💬 vibeli-24: взял #171")}}
	duty := &fakeDuty{}
	runDutyBot(t, api, duty)

	waitForMessages(t, api, 1)
	asked := duty.list()
	if len(asked) != 1 {
		t.Fatalf("a reply to the bot must reach the agent: %#v", asked)
	}
	if !strings.Contains(asked[0], "а почему так долго?") {
		t.Errorf("the question is missing:\n%s", asked[0])
	}
	if !strings.Contains(asked[0], "взял #171") {
		t.Errorf("the agent must see which message was replied to:\n%s", asked[0])
	}
}

func TestBotIgnoresAReplyToAnotherHuman(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{replyBatch(1, "42", "согласен", 777, "давай смёржим #171")}}
	duty := &fakeDuty{}
	runDutyBot(t, api, duty)

	time.Sleep(150 * time.Millisecond)
	if asked := duty.list(); len(asked) != 0 {
		t.Fatalf("a reply between humans must not reach the agent: %#v", asked)
	}
}

// A private chat is nothing but a conversation with the bot: demanding a tag
// there would be pointless ceremony.
func TestBotRoutesEveryMessageInAPrivateChat(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{privateBatch(1, "42", "что с конвейером?")}}
	duty := &fakeDuty{}
	runDutyBot(t, api, duty)

	waitForMessages(t, api, 1)
	if asked := duty.list(); len(asked) != 1 || asked[0] != "что с конвейером?" {
		t.Fatalf("asked = %#v, want the message verbatim", asked)
	}
}

func TestBotAnswersWhenNobodyIsOnDuty(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "@vibeli_ao_bot живой?")}}
	duty := &fakeDuty{err: ErrNoDutyAgent}
	runDutyBot(t, api, duty)

	got := waitForMessages(t, api, 1)
	if len(got) == 0 || !strings.Contains(got[0], "дежурного") {
		t.Errorf("a question with no one on duty must be answered, not swallowed:\n%#v", got)
	}
}

func TestBotIgnoresTaggedMessagesFromOtherChats(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "999", "@vibeli_ao_bot кто дежурный?")}}
	duty := &fakeDuty{}
	runDutyBot(t, api, duty)

	time.Sleep(150 * time.Millisecond)
	if asked := duty.list(); len(asked) != 0 {
		t.Fatalf("a message from an unknown chat must not reach the agent: %#v", asked)
	}
	if got := api.messages(); len(got) != 0 {
		t.Fatalf("an unknown chat must not even get a reply: %#v", got)
	}
}

func TestStripTagKeepsAMentionInsideASentence(t *testing.T) {
	bot := &Bot{identity: Identity{Username: "vibeli_ao_bot"}}
	if got := bot.stripTag("@vibeli_ao_bot, что там с #171?"); got != "что там с #171?" {
		t.Errorf("leading tag = %q", got)
	}
	if got := bot.stripTag("что там с #171? @vibeli_ao_bot"); got != "что там с #171?" {
		t.Errorf("trailing tag = %q", got)
	}
	if got := bot.stripTag("скажи @vibeli_ao_bot спасибо"); got != "скажи @vibeli_ao_bot спасибо" {
		t.Errorf("a tag inside a sentence is part of the text, got %q", got)
	}
}

// @bot must not answer for @bot2 — a different bot in the same chat.
func TestTaggedRequiresAWordBoundary(t *testing.T) {
	bot := &Bot{identity: Identity{Username: "vibeli_ao_bot"}}
	if bot.tagged("@vibeli_ao_bot2 подскажи") {
		t.Error("a longer username must not count as this bot's tag")
	}
	if !bot.tagged("@VIBELI_AO_BOT, что там?") {
		t.Error("a tag is case-insensitive")
	}
}

// --- one question, one message ---------------------------------------------

// The chat is told the question was taken, and the answer overwrites that very
// message: a question must not cost the chat two lines.
func TestBotHandsTheAcknowledgementToTheAnswer(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "@vibeli_ao_bot что там?")}}
	duty := &fakeDuty{}
	runDutyBot(t, api, duty)

	waitForMessages(t, api, 1)
	deadline := time.Now().Add(2 * time.Second)
	for duty.held("vibeli-24") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := duty.held("vibeli-24"); got != 1 {
		t.Fatalf("the session must be handed the message id of its acknowledgement, got %d", got)
	}
}

func TestPublisherReplaceOverwritesTheMessage(t *testing.T) {
	api := &fakeAPI{}
	pub := NewPublisher(newTestClient(t, api), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub.Start(ctx)

	pub.Replace(7, "💬 vibeli-24: всё готово")

	deadline := time.Now().Add(2 * time.Second)
	for len(api.rewrites()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	edits := api.rewrites()
	if len(edits) != 1 || edits[0].messageID != 7 || edits[0].text != "💬 vibeli-24: всё готово" {
		t.Fatalf("edits = %#v", edits)
	}
	if got := api.messages(); len(got) != 0 {
		t.Fatalf("an overwrite must not also post a new message: %#v", got)
	}
}

// Telegram refuses an edit on a message it no longer has. Losing the answer
// would be worse than an extra line, so it falls back to a fresh message.
func TestPublisherFallsBackWhenTheEditIsRefused(t *testing.T) {
	api := &fakeAPI{editErr: true}
	pub := NewPublisher(newTestClient(t, api), discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub.Start(ctx)

	pub.Replace(7, "ответ дежурного")

	got := waitForMessages(t, api, 1)
	if len(got) != 1 || got[0] != "ответ дежурного" {
		t.Fatalf("sent = %#v, want the answer as a new message", got)
	}
}

// fakeAuth stands in for the login script: the bot must never need a tmux
// server to be testable.
type fakeAuth struct {
	url    string
	codes  []string
	result string
	err    error
}

func (a *fakeAuth) LoginStart(context.Context) (string, error) { return a.url, a.err }

func (a *fakeAuth) LoginCode(_ context.Context, code string) (string, error) {
	a.codes = append(a.codes, code)
	return a.result, a.err
}

func TestBotReloginHandsBackTheLoginLink(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/relogin")}}
	auth := &fakeAuth{url: "https://claude.com/cai/oauth/authorize?code=true"}
	runBotWithAuth(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, nil, nil, auth)

	got := waitForMessages(t, api, 1)
	if len(got) == 0 || !strings.Contains(got[0], auth.url) {
		t.Fatalf("reply must carry the login link, got %#v", got)
	}
}

func TestBotCodeForwardsTheCodeToTheLogin(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/code abc#def")}}
	auth := &fakeAuth{result: "Готово: авторизация обновлена"}
	runBotWithAuth(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, nil, nil, auth)

	got := waitForMessages(t, api, 1)
	if len(auth.codes) != 1 || auth.codes[0] != "abc#def" {
		t.Fatalf("codes = %v, want [abc#def]", auth.codes)
	}
	if len(got) == 0 || !strings.Contains(got[0], "обновлена") {
		t.Fatalf("reply must report the result, got %#v", got)
	}
}

// A code without a live login is the likely mistake — the chat is a place where
// people paste things in the wrong order — and it must not read as a crash.
func TestBotCodeWithoutArgumentExplainsUsage(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/code")}}
	auth := &fakeAuth{}
	runBotWithAuth(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, nil, nil, auth)

	got := waitForMessages(t, api, 1)
	if len(auth.codes) != 0 {
		t.Fatalf("nothing should reach the login without an argument: %v", auth.codes)
	}
	if len(got) == 0 || !strings.Contains(got[0], "/code") {
		t.Fatalf("reply must show the usage, got %#v", got)
	}
}

// Без настроенного релогина команда обязана ответить, а не молчать: молчание в
// чате неотличимо от сломанного бота.
func TestBotReloginWithoutAuthStillAnswers(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/relogin")}}
	runBotWithAuth(t, api, fakeSessions{}, &fakeKiller{}, &fakeGate{}, nil, nil, nil)

	got := waitForMessages(t, api, 1)
	if len(got) == 0 || !strings.Contains(got[0], "недоступен") {
		t.Fatalf("reply must say the feature is off, got %#v", got)
	}
}
