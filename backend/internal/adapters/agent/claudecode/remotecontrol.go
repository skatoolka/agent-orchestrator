package claudecode

import "strings"

// RemoteControlState is what a session's pane says about its Remote Control
// link: whether Claude is still mirroring the session, and why not when it is
// not.
//
// Known is false when the pane says nothing either way. That is the normal
// state of a busy agent whose scrollback has moved past the last Remote Control
// line, and it must not be reported as an outage: a false alarm for every
// working session would bury the real one.
type RemoteControlState struct {
	Known     bool
	Connected bool
	// Detail is the reason Claude gave for the disconnect, e.g. "OAuth token
	// unavailable". Empty when connected or unknown.
	Detail string
}

// Remote Control announces itself in the pane, and the last announcement wins:
// scrollback keeps the whole history, so a session that dropped and came back
// carries both lines.
const (
	disconnectedMarker = "Remote Control disconnected"
	inactiveMarker     = "/remote-control is no longer active"
	activeMarker       = "/remote-control is active"
	sessionActiveMark  = "session is active"
)

// ReadRemoteControlPane reads the tail of a session's pane and reports the last
// thing Remote Control said there.
func ReadRemoteControlPane(pane string) RemoteControlState {
	state := RemoteControlState{}
	for _, line := range strings.Split(pane, "\n") {
		switch {
		case strings.Contains(line, disconnectedMarker):
			state = RemoteControlState{Known: true, Connected: false, Detail: disconnectReason(line)}
		case strings.Contains(line, inactiveMarker):
			state = RemoteControlState{Known: true, Connected: false, Detail: "сессия Remote Control закрыта"}
		case strings.Contains(line, activeMarker), strings.Contains(line, sessionActiveMark):
			state = RemoteControlState{Known: true, Connected: true}
		}
	}
	return state
}

// disconnectReason pulls the cause out of
// "Remote Control disconnected — <cause> — run /remote-control to reconnect".
// Claude writes the parts with em dashes; a line in some other shape yields no
// reason rather than a mangled one.
func disconnectReason(line string) string {
	_, rest, ok := strings.Cut(line, disconnectedMarker)
	if !ok {
		return ""
	}
	rest = strings.TrimLeft(strings.TrimSpace(rest), "—- ")
	if cause, _, ok := strings.Cut(rest, "—"); ok {
		rest = cause
	}
	return strings.TrimSpace(rest)
}

// RemoteControlCommand is the line that brings Remote Control back in a live
// session. Claude itself prints "run /remote-control to reconnect", and typing
// it costs the agent nothing: the session and its context stay, only the link
// to Claude is rebuilt.
const RemoteControlCommand = "/remote-control"
