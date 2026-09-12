package telegram

import (
	"context"
	"fmt"
	"strings"
)

// RemoteControlState is one session's link to Claude: whether the session is
// still mirrored into claude.ai/code and the phone app, and why not when it is
// not.
//
// Known false means the session's pane says nothing either way — the normal
// state of a busy agent. It is neither an outage nor proof of health, and the
// chat is told exactly that rather than a guess.
type RemoteControlState struct {
	SessionID string
	Known     bool
	Connected bool
	Detail    string
}

// RemoteControl reads and repairs the Claude side of live sessions.
//
// It exists because a dropped Remote Control link is silent: the agent keeps
// working, the board keeps moving, the daemon is healthy — the only thing lost
// is the ability to answer that agent from a phone, which is noticed at the
// worst possible moment. /relogin covers the case where the whole login died;
// this covers the one session whose link fell over on its own.
type RemoteControl interface {
	States(ctx context.Context) ([]RemoteControlState, error)
	Reconnect(ctx context.Context, sessionID string) (RemoteControlState, error)
}

// remoteControlReport answers /rc with no argument: which sessions lost the
// link, each one a button away from being repaired.
func (b *Bot) remoteControlReport(ctx context.Context) (string, Keyboard) {
	if b.remote == nil {
		return "состояние Remote Control недоступно", nil
	}
	states, err := b.remote.States(ctx)
	if err != nil {
		return "не смог прочитать состояние Remote Control: " + err.Error(), nil
	}
	var (
		out     strings.Builder
		buttons Keyboard
		down    int
		quiet   int
		live    int
	)
	for _, state := range states {
		switch {
		case !state.Known:
			quiet++
		case state.Connected:
			live++
		default:
			down++
			line := "• " + state.SessionID
			if state.Detail != "" {
				line += " — " + truncate(state.Detail, 60)
			}
			out.WriteString(line + "\n")
			buttons = append(buttons, []InlineButton{{
				Text: "🔌 " + state.SessionID,
				Data: b.desk.register(action{kind: actionReconnect, ref: state.SessionID}),
			}})
		}
	}
	if down == 0 {
		return fmt.Sprintf("🔌 обрывов Remote Control нет (на связи %d, молчат %d)", live, quiet), nil
	}
	header := fmt.Sprintf("🔌 Remote Control отвалился у %d сессий:\n\n", down)
	// A pane that said nothing is not a healthy pane; saying so keeps the
	// report from being read as a clean bill of health.
	footer := fmt.Sprintf("\nна связи %d, молчат %d (молчание — не обрыв: агент просто не печатал про Remote Control)", live, quiet)
	return header + strings.TrimRight(out.String(), "\n") + footer, buttons
}

// reconnect repairs one session and reports what actually happened — the answer
// is read back from the pane, not assumed from the fact that the command was
// delivered.
func (b *Bot) reconnect(ctx context.Context, sessionID string) string {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return "нужен id сессии: /rc vibeli-3"
	}
	if b.remote == nil {
		return "переподключение Remote Control недоступно"
	}
	state, err := b.remote.Reconnect(ctx, id)
	if err != nil {
		return "не смог переподключить " + id + ": " + err.Error()
	}
	switch {
	case state.Connected:
		return "🔌 " + id + ": Remote Control поднят"
	case !state.Known:
		// The command went in and the pane stayed quiet. Claiming success here
		// is exactly the lie this command exists to avoid.
		return "отправил " + id + " команду " + remoteControlCommand + ", но панель молчит — проверь сессию в AO web"
	default:
		answer := id + ": не поднялся"
		if state.Detail != "" {
			answer += " — " + truncate(state.Detail, 80)
		}
		return answer + "\n\nесли дело в логине Claude — /relogin"
	}
}

// remoteControlCommand is what the bot types into the pane. It is spelled out
// in replies so a human can do the same by hand.
const remoteControlCommand = "/remote-control"
