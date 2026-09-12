package claudecode

import "testing"

func TestReadRemoteControlPaneFindsADisconnectAndItsReason(t *testing.T) {
	pane := "● Baked for 21m\n● Remote Control disconnected — OAuth token unavailable — run /login to restore Remote Control\n"
	state := ReadRemoteControlPane(pane)
	if !state.Known || state.Connected {
		t.Fatalf("state = %+v, want a known disconnect", state)
	}
	if state.Detail != "OAuth token unavailable" {
		t.Fatalf("detail = %q, want the cause Claude gave", state.Detail)
	}
}

func TestReadRemoteControlPaneTakesTheLastWord(t *testing.T) {
	// A session that dropped and came back carries both lines; the reconnect is
	// the current truth.
	pane := "● Remote Control disconnected — network error — run /remote-control to reconnect\n" +
		"● New /remote-control session is active\n"
	state := ReadRemoteControlPane(pane)
	if !state.Known || !state.Connected {
		t.Fatalf("state = %+v, want connected: the reconnect came after the drop", state)
	}
}

func TestReadRemoteControlPaneStaysSilentWhenThePaneIs(t *testing.T) {
	// A busy agent whose scrollback moved past the last Remote Control line.
	// Reporting that as an outage would bury the real one.
	state := ReadRemoteControlPane("● читаю файлы\n● правлю тесты\n")
	if state.Known {
		t.Fatalf("state = %+v, want unknown when the pane says nothing", state)
	}
}

func TestReadRemoteControlPaneReadsAClosedSessionAsDown(t *testing.T) {
	state := ReadRemoteControlPane("● /remote-control is no longer active. Run /remote-control to start a new session.\n")
	if !state.Known || state.Connected {
		t.Fatalf("state = %+v, want a known disconnect", state)
	}
}
