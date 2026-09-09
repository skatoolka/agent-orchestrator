package telegram

import (
	"context"
	"strings"
)

// Auth reopens a Claude Code login from the chat.
//
// Why the bot owns this at all: the login the conveyor runs on expires, and an
// expired one is invisible from outside — the daemon is up, tmux is up, and the
// agents just stand on the login screen. Recovering it used to mean an ssh
// session on the stand, which is exactly what nobody has at the moment they
// notice the board stopped moving.
//
// Two calls rather than one because the flow is interactive by nature: the CLI
// prints a link, a human confirms it in a browser, and brings back a code. The
// live process between the two calls holds the PKCE challenge, so a second
// LoginStart invalidates the code from the first.
type Auth interface {
	LoginStart(ctx context.Context) (url string, err error)
	LoginCode(ctx context.Context, code string) (result string, err error)
}

func (b *Bot) relogin(ctx context.Context) string {
	if b.auth == nil {
		return "Релогин недоступен: не настроен."
	}
	url, err := b.auth.LoginStart(ctx)
	if err != nil {
		return "Не смог начать логин: " + err.Error()
	}
	return strings.Join([]string{
		"Открой ссылку, подтверди вход и пришли код: /code <код>",
		"",
		url,
	}, "\n")
}

func (b *Bot) loginCode(ctx context.Context, arg string) string {
	if b.auth == nil {
		return "Релогин недоступен: не настроен."
	}
	code := strings.TrimSpace(arg)
	if code == "" {
		return "Нужен код: /code <код со страницы входа>"
	}
	result, err := b.auth.LoginCode(ctx, code)
	if err != nil {
		return "Код не принят: " + err.Error()
	}
	return result
}
