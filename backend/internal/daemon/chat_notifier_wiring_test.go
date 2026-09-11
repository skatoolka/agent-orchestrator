package daemon

import (
	"strings"
	"testing"
)

func TestChatDisplayNameFitsTheSidebarCap(t *testing.T) {
	long := strings.Repeat("я", 60)
	got := chatDisplayName(long)
	if count := len([]rune(got)); count > chatDisplayNameLen {
		// The daemon rejects a longer label outright, so a task typed into the
		// chat would fail to spawn rather than get a shorter name.
		t.Fatalf("label is %d runes, want at most %d", count, chatDisplayNameLen)
	}
}

func TestChatDisplayNameTakesTheFirstLine(t *testing.T) {
	got := chatDisplayName("починить логин\n\nподробности ниже")
	if got != "починить логин" {
		t.Fatalf("label = %q, want the first line of the task", got)
	}
}

func TestChatLinksCarryTheDashboardOrigin(t *testing.T) {
	t.Setenv("AO_WEB_BASE_URL", "https://ao-web.example.com")
	t.Setenv("AO_CLAUDE_REMOTE_CONTROL", "1")
	t.Setenv("AO_CLAUDE_REMOTE_CONTROL_PREFIX", "vibeli")
	links := chatLinks()
	if links.WebBase != "https://ao-web.example.com" {
		t.Fatalf("WebBase = %q, want the configured dashboard origin", links.WebBase)
	}
	// The bot must read the Remote Control switch from the launcher's own
	// helper, or a chat link would promise a session Claude never sees.
	if got := links.RemoteControlName("vibeli-42"); got != "vibeli/vibeli-42" {
		t.Fatalf("RemoteControlName = %q, want vibeli/vibeli-42", got)
	}
}

func TestChatLinksDropClaudeWhenRemoteControlIsOff(t *testing.T) {
	t.Setenv("AO_CLAUDE_REMOTE_CONTROL", "")
	if got := chatLinks().RemoteControlName("vibeli-42"); got != "" {
		t.Fatalf("RemoteControlName = %q, want empty with Remote Control off", got)
	}
}
