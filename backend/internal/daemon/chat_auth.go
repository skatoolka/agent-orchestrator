package daemon

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// claudeAuthScript drives `claude-auth`, the script that owns the login flow in
// the conveyor image.
//
// The daemon shells out instead of reimplementing the flow because the same
// script must work from a plain shell: the situation it exists for is the one
// where the daemon itself may be the thing that is unhappy, and a login that
// can only be reached through a healthy daemon is no recovery path at all.
type claudeAuthScript struct {
	bin string
}

// Waiting is part of the contract, not a hazard: login-start polls the pane for
// the link the CLI prints, login-code waits for the CLI to accept the code.
// The budgets are the script's own, plus room for process startup.
const (
	loginStartBudget = 90 * time.Second
	loginCodeBudget  = 70 * time.Second
)

func (a claudeAuthScript) LoginStart(ctx context.Context) (string, error) {
	out, err := a.run(ctx, loginStartBudget, "login-start")
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(out)
	if !strings.HasPrefix(url, "https://") {
		return "", errors.New(firstLines(out, 6))
	}
	return url, nil
}

func (a claudeAuthScript) LoginCode(ctx context.Context, code string) (string, error) {
	return a.run(ctx, loginCodeBudget, "login-code", code)
}

func (a claudeAuthScript) run(ctx context.Context, budget time.Duration, args ...string) (string, error) {
	bin := a.bin
	if bin == "" {
		bin = "claude-auth"
	}
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// CombinedOutput on purpose: the script talks to a human, and its
	// diagnostics — the pane screen when something went wrong — go to stderr.
	out, err := exec.CommandContext(runCtx, bin, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return "", err
		}
		return "", errors.New(firstLines(text, 8))
	}
	return text, nil
}

func firstLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
