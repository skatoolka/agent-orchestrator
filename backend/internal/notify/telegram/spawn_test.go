package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSpawner records what the chat asked to start.
type fakeSpawner struct {
	mu       sync.Mutex
	projects []Project
	spawned  []spawnCall
	err      error
}

type spawnCall struct {
	project string
	prompt  string
}

func (f *fakeSpawner) Projects(context.Context) ([]Project, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.projects, nil
}

func (f *fakeSpawner) Spawn(_ context.Context, project, prompt string) (SpawnResult, error) {
	if f.err != nil {
		return SpawnResult{}, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawned = append(f.spawned, spawnCall{project: project, prompt: prompt})
	return SpawnResult{SessionID: "vibeli-42"}, nil
}

func (f *fakeSpawner) calls() []spawnCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]spawnCall(nil), f.spawned...)
}

// callbackBatch is one button press, as Telegram delivers it.
func callbackBatch(id int64, chat, data string, messageID int64) []byte {
	payload := map[string]any{
		"ok": true,
		"result": []map[string]any{{
			"update_id": id,
			"callback_query": map[string]any{
				"id":   fmt.Sprintf("cb-%d", id),
				"data": data,
				"from": map[string]any{"username": "operator"},
				"message": map[string]any{
					"message_id": messageID,
					"chat":       map[string]any{"id": json.RawMessage(chat), "type": "supergroup"},
				},
			},
		}},
	}
	out, _ := json.Marshal(payload)
	return out
}

// replyToBotBatch answers one of the bot's own messages by id.
func replyToBotBatch(id int64, chat, text string, replyTo int64) []byte {
	return messageBatch(id, chat, "supergroup", map[string]any{
		"text": text,
		"from": map[string]any{"username": "operator"},
		"reply_to_message": map[string]any{
			"message_id": replyTo,
			"text":       "✍️ Ответь на это сообщение текстом задачи",
			"from":       map[string]any{"id": 1000, "is_bot": true},
		},
	})
}

// buttons flattens an inline keyboard into "text -> callback data or url" pairs.
func buttons(markup map[string]any) map[string]string {
	out := map[string]string{}
	rows, _ := markup["inline_keyboard"].([]any)
	for _, row := range rows {
		cells, _ := row.([]any)
		for _, cell := range cells {
			button, _ := cell.(map[string]any)
			text, _ := button["text"].(string)
			if data, ok := button["callback_data"].(string); ok {
				out[text] = data
				continue
			}
			if url, ok := button["url"].(string); ok {
				out[text] = url
			}
		}
	}
	return out
}

// findButton returns the callback payload of the first button whose label
// contains want.
func findButton(t *testing.T, markup map[string]any, want string) string {
	t.Helper()
	for text, data := range buttons(markup) {
		if strings.Contains(text, want) {
			return data
		}
	}
	t.Fatalf("no button matching %q in %v", want, buttons(markup))
	return ""
}

func waitForPosts(t *testing.T, api *fakeAPI, want int) []sentMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := api.posts(); len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %d messages, got %d: %v", want, len(api.posts()), api.messages())
	return nil
}

func waitForEdits(t *testing.T, api *fakeAPI, want int) []editCall {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := api.rewrites(); len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %d edits, got %d", want, len(api.rewrites()))
	return nil
}

func testLinks() Links {
	return Links{
		WebBase:           "https://ao-web.example.com",
		RemoteControlName: func(id string) string { return "vibeli/" + id },
	}
}

func TestNewOffersEveryProjectAsAButton(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}, {ID: "websites"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	posts := waitForPosts(t, api, 1)
	labels := buttons(posts[0].markup)
	if _, ok := labels["vibeli"]; !ok {
		t.Fatalf("the project picker must offer vibeli: %v", labels)
	}
	if _, ok := labels["websites"]; !ok {
		t.Fatalf("the project picker must offer websites: %v", labels)
	}
}

func TestNewWithASingleProjectSkipsThePicker(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	posts := waitForPosts(t, api, 1)
	if !strings.Contains(posts[0].text, "Как ставим задачу") {
		t.Fatalf("one project is not a choice; the menu must move on:\n%s", posts[0].text)
	}
	if _, ok := buttons(posts[0].markup)["← проекты"]; ok {
		t.Fatalf("there is nowhere to go back to: %v", buttons(posts[0].markup))
	}
}

func TestNewWithATaskStartsTheSessionRightAway(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new почини логин")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	posts := waitForPosts(t, api, 1)
	calls := spawner.calls()
	if len(calls) != 1 || calls[0].prompt != "почини логин" || calls[0].project != "vibeli" {
		t.Fatalf("spawned = %v, want one vibeli session with the typed task", calls)
	}
	if !strings.Contains(posts[0].text, "vibeli-42") {
		t.Fatalf("the answer must name the session:\n%s", posts[0].text)
	}
}

func TestTaskButtonAsksForTheTextAndSpawnsOnTheReply(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	duty := &fakeDuty{}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Duty: duty, Links: testLinks()})

	menu := waitForPosts(t, api, 1)[0]
	press := findButton(t, menu.markup, "Задача текстом")
	api.mu.Lock()
	api.updates = append(api.updates, callbackBatch(2, "42", press, 1))
	api.mu.Unlock()

	// The request for the text is message #2; answering it is what starts the
	// session.
	question := waitForPosts(t, api, 2)[1]
	if !strings.Contains(question.text, "Ответь на это сообщение") {
		t.Fatalf("the bot must open a reply box:\n%s", question.text)
	}
	api.mu.Lock()
	api.updates = append(api.updates, replyToBotBatch(3, "42", "почини логин", 2))
	api.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(spawner.calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := spawner.calls()
	if len(calls) != 1 || calls[0].prompt != "почини логин" {
		t.Fatalf("spawned = %v, want the replied text as the task", calls)
	}
	// The reply answered the bot's own question, so it is not a question for
	// the agent on duty.
	if asked := duty.list(); len(asked) != 0 {
		t.Fatalf("the task text must not be forwarded to the duty agent: %v", asked)
	}
}

func TestQueueButtonClaimsTheCard(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	conveyor := &fakeConveyor{items: []QueueItem{{Project: "vibeli", Issue: "acme/demo#12", Title: "починить логин"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Conveyor: conveyor, Links: testLinks()})

	menu := waitForPosts(t, api, 1)[0]
	api.mu.Lock()
	api.updates = append(api.updates, callbackBatch(2, "42", findButton(t, menu.markup, "Из очереди"), 1))
	api.mu.Unlock()

	edits := waitForEdits(t, api, 1)
	card := findButton(t, edits[0].markup, "#12")
	api.mu.Lock()
	api.updates = append(api.updates, callbackBatch(3, "42", card, 1))
	api.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(conveyor.claimed) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := conveyor.claimed; len(got) != 1 || got[0] != "acme/demo#12" {
		t.Fatalf("claimed = %v, want [acme/demo#12]", got)
	}
}

func TestSessionCardLinksToTheDashboardAndToClaude(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new почини логин")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	post := waitForPosts(t, api, 1)[0]
	links := buttons(post.markup)
	var web, claude string
	for text, url := range links {
		switch {
		case strings.Contains(text, "AO web"):
			web = url
		case strings.Contains(text, "Claude"):
			claude = url
		}
	}
	if web != "https://ao-web.example.com/#/sessions/vibeli-42" {
		t.Fatalf("dashboard link = %q, want the session's own route", web)
	}
	if claude != defaultClaudeURL {
		t.Fatalf("claude link = %q, want %q", claude, defaultClaudeURL)
	}
	// The name is what identifies the session in Claude's own list, so the card
	// must spell it out.
	if !strings.Contains(post.text, "vibeli/vibeli-42") {
		t.Fatalf("the card must name the Remote Control session:\n%s", post.text)
	}
}

func TestSessionCardWithoutRemoteControlOffersOnlyTheDashboard(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new почини логин")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	// Remote Control off: the deployment does not publish sessions to Claude.
	links := Links{WebBase: "https://ao-web.example.com", RemoteControlName: func(string) string { return "" }}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: links})

	post := waitForPosts(t, api, 1)[0]
	for text := range buttons(post.markup) {
		if strings.Contains(text, "Claude") {
			t.Fatalf("a link to Claude must not appear when Remote Control is off: %v", buttons(post.markup))
		}
	}
}

func TestAStaleButtonSaysSoInsteadOfGuessing(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{callbackBatch(1, "42", "new:gone", 7)}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	edits := waitForEdits(t, api, 1)
	if !strings.Contains(edits[0].text, "устарело") {
		t.Fatalf("a button whose meaning is gone must say so:\n%s", edits[0].text)
	}
	if got := spawner.calls(); len(got) != 0 {
		t.Fatalf("nothing may be spawned from a stale button: %v", got)
	}
}

func TestEveryButtonPressIsAcknowledged(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	menu := waitForPosts(t, api, 1)[0]
	api.mu.Lock()
	api.updates = append(api.updates, callbackBatch(2, "42", findButton(t, menu.markup, "Задача текстом"), 1))
	api.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(api.acknowledgements()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := api.acknowledgements(); len(got) != 1 || got[0] != "cb-2" {
		t.Fatalf("acknowledged = %v, want [cb-2]: an unanswered press spins forever", got)
	}
}

func TestButtonPressFromAnUnknownChatIsIgnored(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{callbackBatch(1, "999", "new:1", 5)}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	time.Sleep(150 * time.Millisecond)
	if got := api.messages(); len(got) != 0 {
		t.Fatalf("an unknown chat must get no answer at all: %v", got)
	}
	if got := api.acknowledgements(); len(got) != 0 {
		t.Fatalf("an unknown chat must not even get its spinner stopped: %v", got)
	}
}

func TestDeskForgetsAnExpiredButton(t *testing.T) {
	desk := newDesk()
	now := time.Now()
	desk.now = func() time.Time { return now }
	data := desk.register(action{kind: actionProject, project: "vibeli"})
	if _, ok := desk.lookup(data); !ok {
		t.Fatal("a fresh button must resolve")
	}
	now = now.Add(actionTTL + time.Minute)
	if _, ok := desk.lookup(data); ok {
		t.Fatal("a button older than its TTL must not act on a backlog that has moved on")
	}
}

func TestDeskAnswersAPendingQuestionOnlyOnce(t *testing.T) {
	desk := newDesk()
	desk.await(7, action{kind: actionAsk, project: "vibeli"})
	if _, ok := desk.claimReply(7); !ok {
		t.Fatal("the reply to a pending question must be claimed")
	}
	if _, ok := desk.claimReply(7); ok {
		t.Fatal("one question is answered once; a second reply belongs to the duty agent")
	}
}

func TestSpawnFailureIsReportedInTheChat(t *testing.T) {
	api := &fakeAPI{updates: [][]byte{updateBatch(1, "42", "/new почини логин")}}
	spawner := &fakeSpawner{projects: []Project{{ID: "vibeli"}}, err: fmt.Errorf("worktree is dirty")}
	runBotWithDeps(t, Deps{Client: newTestClient(t, api), Spawner: spawner, Links: testLinks()})

	posts := waitForPosts(t, api, 1)
	if !strings.Contains(posts[0].text, "worktree is dirty") {
		t.Fatalf("a refused spawn must say why:\n%s", posts[0].text)
	}
}
