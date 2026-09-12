package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/notify/telegram"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// paneReader is the slice of the runtime adapter this needs: the tail of a
// session's pane. Remote Control reports its own state there and nowhere else —
// the daemon is not told when the link drops.
type paneReader interface {
	GetOutput(ctx context.Context, handle ports.RuntimeHandle, lines int) (string, error)
}

// sessionRoster and sessionInbox are the two halves of the session service this
// needs — read the sessions, type into one. They are interfaces so the repair
// logic is testable without a store and a live runtime behind it.
type sessionRoster interface {
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

type sessionInbox interface {
	Send(ctx context.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error
}

// chatRemoteControl reads and repairs the Claude side of live sessions on
// behalf of the chat bot.
//
// The repair is a line typed into the agent's own pane, which is what Claude
// itself prescribes ("run /remote-control to reconnect"). That costs the
// session nothing: the agent, its worktree and its context stay; only the link
// to claude.ai is rebuilt. Killing and respawning the session would achieve the
// same and throw away the work in flight.
type chatRemoteControl struct {
	roster  sessionRoster
	inbox   sessionInbox
	runtime paneReader
	logger  *slog.Logger
	// after is time.After, injected so the tests do not wait out the poll.
	after func(time.Duration) <-chan time.Time
}

const (
	// paneLines is how much scrollback is read.
	//
	// Measured on the dev stand: sixteen of seventeen live panes carry no Remote
	// Control line at all, and widening the window from 40 to 400 changed none
	// of them — the marker is printed when the link changes state and is gone
	// from tmux history long before a busy agent is looked at. So "quiet" is the
	// normal reading, not a narrow window, and it is reported as quiet rather
	// than as health.
	//
	// The wide window is still the right default: it is one tmux read, and it is
	// what keeps a drop under an agent that kept working from being pushed out
	// of the tail. The last-word rule keeps the extra history from being
	// misread as a current outage.
	paneLines = 400
	// reconnectPolls and reconnectInterval bound the wait for Claude to answer
	// the typed command. Reconnecting takes a couple of seconds when it works;
	// past this the honest answer is "the pane stayed quiet".
	reconnectPolls    = 6
	reconnectInterval = 2 * time.Second
)

func (r chatRemoteControl) wait(d time.Duration) <-chan time.Time {
	if r.after != nil {
		return r.after(d)
	}
	return time.After(d)
}

// States reports the Remote Control link of every live Claude Code session.
func (r chatRemoteControl) States(ctx context.Context) ([]telegram.RemoteControlState, error) {
	sessions, err := r.liveSessions(ctx)
	if err != nil {
		return nil, err
	}
	states := make([]telegram.RemoteControlState, 0, len(sessions))
	for _, session := range sessions {
		states = append(states, r.readState(ctx, session))
	}
	return states, nil
}

// Reconnect types the reconnect command into one session's pane and reads back
// what happened, rather than reporting the delivery as a repair.
func (r chatRemoteControl) Reconnect(ctx context.Context, sessionID string) (telegram.RemoteControlState, error) {
	if r.inbox == nil || r.roster == nil {
		return telegram.RemoteControlState{}, fmt.Errorf("remote control is not wired")
	}
	sessions, err := r.liveSessions(ctx)
	if err != nil {
		return telegram.RemoteControlState{}, err
	}
	var target domain.SessionRecord
	found := false
	for _, session := range sessions {
		if strings.EqualFold(string(session.ID), strings.TrimSpace(sessionID)) {
			target = session
			found = true
			break
		}
	}
	if !found {
		return telegram.RemoteControlState{}, fmt.Errorf("нет живой сессии %s", sessionID)
	}
	if err := r.inbox.Send(ctx, target.ID, claudecode.RemoteControlCommand, nil); err != nil {
		return telegram.RemoteControlState{}, err
	}
	// Claude answers in the pane within a second or two. Polling beats one long
	// sleep: a link that comes back fast is reported fast.
	var state telegram.RemoteControlState
	for attempt := 0; attempt < reconnectPolls; attempt++ {
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-r.wait(reconnectInterval):
		}
		state = r.readState(ctx, target)
		if state.Known && state.Connected {
			return state, nil
		}
	}
	return state, nil
}

// readState reads one session's pane. A session with no runtime handle, or one
// the runtime cannot read, is reported as unknown rather than as an outage.
func (r chatRemoteControl) readState(ctx context.Context, session domain.SessionRecord) telegram.RemoteControlState {
	state := telegram.RemoteControlState{SessionID: string(session.ID)}
	handle := strings.TrimSpace(session.Metadata.RuntimeHandleID)
	if handle == "" || r.runtime == nil {
		return state
	}
	pane, err := r.runtime.GetOutput(ctx, ports.RuntimeHandle{ID: handle}, paneLines)
	if err != nil {
		r.logger.Warn("remote control: could not read pane", "session", session.ID, "err", err)
		return state
	}
	read := claudecode.ReadRemoteControlPane(pane)
	state.Known = read.Known
	state.Connected = read.Connected
	state.Detail = read.Detail
	return state
}

// liveSessions returns the sessions Remote Control can apply to: running ones
// driven by Claude Code. Other harnesses have no Remote Control to lose.
func (r chatRemoteControl) liveSessions(ctx context.Context) ([]domain.SessionRecord, error) {
	if r.roster == nil {
		return nil, fmt.Errorf("session list is unavailable")
	}
	all, err := r.roster.ListAllSessions(ctx)
	if err != nil {
		return nil, err
	}
	live := make([]domain.SessionRecord, 0, len(all))
	for _, session := range all {
		if session.IsTerminated || session.Harness != domain.HarnessClaudeCode {
			continue
		}
		live = append(live, session)
	}
	return live, nil
}
