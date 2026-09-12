package daemon

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeRoster struct{ sessions []domain.SessionRecord }

func (f fakeRoster) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return f.sessions, nil
}

type fakeInbox struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeInbox) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, string(id)+": "+message)
	return nil
}

func (f *fakeInbox) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// fakePanes serves pane text per handle, switching to the second answer once
// the reconnect command has been typed.
type fakePanes struct {
	mu     sync.Mutex
	before string
	after  string
	typed  *fakeInbox
}

func (f *fakePanes) GetOutput(_ context.Context, _ ports.RuntimeHandle, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.typed != nil && len(f.typed.list()) > 0 && f.after != "" {
		return f.after, nil
	}
	return f.before, nil
}

func claudeSession(id, handle string) domain.SessionRecord {
	return domain.SessionRecord{
		ID:       domain.SessionID(id),
		Harness:  domain.HarnessClaudeCode,
		Metadata: domain.SessionMetadata{RuntimeHandleID: handle},
	}
}

func testRemoteControl(roster sessionRoster, inbox sessionInbox, panes paneReader) chatRemoteControl {
	return chatRemoteControl{
		roster:  roster,
		inbox:   inbox,
		runtime: panes,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Fire the poll immediately: the test is about what is read back, not
		// about waiting out the interval.
		after: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
	}
}

func TestStatesReportTheDisconnectedSessions(t *testing.T) {
	roster := fakeRoster{sessions: []domain.SessionRecord{claudeSession("vibeli-1", "pane-1")}}
	panes := &fakePanes{before: "● Remote Control disconnected — OAuth token unavailable — run /login\n"}
	states, err := testRemoteControl(roster, &fakeInbox{}, panes).States(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Connected || !states[0].Known {
		t.Fatalf("states = %+v, want one known disconnect", states)
	}
	if states[0].Detail != "OAuth token unavailable" {
		t.Fatalf("detail = %q, want the cause from the pane", states[0].Detail)
	}
}

func TestStatesSkipSessionsWithoutRemoteControlToLose(t *testing.T) {
	terminated := claudeSession("vibeli-old", "pane-old")
	terminated.IsTerminated = true
	codex := claudeSession("vibeli-codex", "pane-codex")
	codex.Harness = domain.HarnessCodex
	roster := fakeRoster{sessions: []domain.SessionRecord{terminated, codex, claudeSession("vibeli-1", "pane-1")}}
	states, err := testRemoteControl(roster, &fakeInbox{}, &fakePanes{before: ""}).States(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].SessionID != "vibeli-1" {
		t.Fatalf("states = %+v, want only the live Claude Code session", states)
	}
}

func TestStatesLeaveAHandlelessSessionUnknown(t *testing.T) {
	// No runtime handle means no pane to read. Calling that an outage would put
	// a healthy session on the repair list.
	roster := fakeRoster{sessions: []domain.SessionRecord{claudeSession("vibeli-1", "")}}
	states, err := testRemoteControl(roster, &fakeInbox{}, &fakePanes{before: "● Remote Control disconnected — x\n"}).States(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Known {
		t.Fatalf("states = %+v, want unknown without a pane", states)
	}
}

func TestReconnectTypesTheCommandAndReadsTheAnswerBack(t *testing.T) {
	inbox := &fakeInbox{}
	panes := &fakePanes{
		before: "● Remote Control disconnected — network error — run /remote-control to reconnect\n",
		after:  "● New /remote-control session is active\n",
		typed:  inbox,
	}
	roster := fakeRoster{sessions: []domain.SessionRecord{claudeSession("vibeli-1", "pane-1")}}
	state, err := testRemoteControl(roster, inbox, panes).Reconnect(context.Background(), "vibeli-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := inbox.list(); len(got) != 1 || !strings.HasSuffix(got[0], "/remote-control") {
		t.Fatalf("sent = %v, want the reconnect command typed into the pane", got)
	}
	if !state.Connected {
		t.Fatalf("state = %+v, want connected after the pane said so", state)
	}
}

func TestReconnectReportsAPaneThatStayedQuiet(t *testing.T) {
	// The command went in and Claude said nothing. Reporting success here is
	// the lie this whole path exists to avoid.
	inbox := &fakeInbox{}
	panes := &fakePanes{before: "● правлю тесты\n", after: "● правлю тесты\n", typed: inbox}
	roster := fakeRoster{sessions: []domain.SessionRecord{claudeSession("vibeli-1", "pane-1")}}
	state, err := testRemoteControl(roster, inbox, panes).Reconnect(context.Background(), "vibeli-1")
	if err != nil {
		t.Fatal(err)
	}
	if state.Known {
		t.Fatalf("state = %+v, want unknown when the pane never answered", state)
	}
}

func TestReconnectRefusesASessionThatIsNotLive(t *testing.T) {
	roster := fakeRoster{sessions: []domain.SessionRecord{claudeSession("vibeli-1", "pane-1")}}
	inbox := &fakeInbox{}
	_, err := testRemoteControl(roster, inbox, &fakePanes{}).Reconnect(context.Background(), "vibeli-404")
	if err == nil {
		t.Fatal("a session that is not live must be refused, not typed into")
	}
	if got := inbox.list(); len(got) != 0 {
		t.Fatalf("nothing may be typed for an unknown session: %v", got)
	}
}
