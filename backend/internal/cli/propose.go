package cli

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type proposeOptions struct {
	project string
	prompt  string
	reason  string
}

// proposeAPIRequest mirrors the daemon's ProposeRequest body for
// POST /api/v1/propose. As with announce, the CLI keeps its own copy so it need
// not import httpd.
type proposeAPIRequest struct {
	Project string `json:"project"`
	Prompt  string `json:"prompt"`
	Reason  string `json:"reason,omitempty"`
	Session string `json:"session,omitempty"`
}

func newProposeCommand(ctx *commandContext) *cobra.Command {
	var opts proposeOptions
	cmd := &cobra.Command{
		Use:   "propose",
		Short: "Ask a human to authorize starting a session",
		Long: "Post a session proposal to the operator chat as a card with confirm/decline buttons.\n" +
			"Nothing starts here: the session begins only if a human presses confirm.\n\n" +
			"This is the path for the agent on duty, which coordinates and must not spawn\n" +
			"work itself. Without it the only honest answer duty could give was \"run this\n" +
			"yourself\" — the machine had the hands and the human had the decision, and\n" +
			"neither could reach the other.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.propose(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "Project the session would start in (required)")
	cmd.Flags().StringVar(&opts.prompt, "prompt", "", "Task brief the session would be given (required)")
	cmd.Flags().StringVar(&opts.reason, "reason", "", "Why this should happen — one line, shown above the buttons")
	return cmd
}

func (c *commandContext) propose(ctx context.Context, opts proposeOptions) error {
	if strings.TrimSpace(opts.project) == "" {
		return usageError{errors.New("usage: --project is required")}
	}
	if strings.TrimSpace(opts.prompt) == "" {
		return usageError{errors.New("usage: --prompt is required")}
	}
	// Same as announce: the session travels as its own field, so the card can
	// say who is asking without the agent formatting the label itself.
	session := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	return c.postJSON(ctx, "propose", proposeAPIRequest{
		Project: strings.TrimSpace(opts.project),
		Prompt:  opts.prompt,
		Reason:  opts.reason,
		Session: session,
	}, nil)
}
