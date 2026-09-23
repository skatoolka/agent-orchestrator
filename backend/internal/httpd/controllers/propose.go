package controllers

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// maxProposalPromptLen bounds the task brief a proposal carries. Same order as
// an announce: it has to fit one chat message alongside the buttons.
const maxProposalPromptLen = 2000

// maxProposalReasonLen bounds the one-line justification.
const maxProposalReasonLen = 500

// ChatProposer posts an action for a human to authorize.
//
// It exists because the agent on duty coordinates rather than works: spawning
// sessions is not its call. Before this the only honest answer duty could give
// was "run this yourself", and the person went and typed it — the machine had
// the hands and the human had the decision, and neither could reach the other.
// A proposal joins them: duty states what it wants, a human presses a button.
type ChatProposer interface {
	// Propose posts the proposal and returns once it is in the chat. It must
	// NOT start anything: authorization happens on the press, not here.
	Propose(ctx context.Context, project, prompt, session, reason string) error
}

// ProposeController owns POST /propose.
type ProposeController struct {
	// Chat is nil on a build without a chat transport; the route then answers
	// 501 rather than accepting a proposal nobody will ever see.
	Chat ChatProposer
}

// Register mounts the propose route on the supplied router.
func (c *ProposeController) Register(r chi.Router) {
	r.Post("/propose", c.propose)
}

func (c *ProposeController) propose(w http.ResponseWriter, r *http.Request) {
	if c.Chat == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/propose")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAnnounceBodyBytes)
	var in ProposeRequest
	if err := decodeJSON(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	project := strings.TrimSpace(domain.SanitizeControlChars(in.Project))
	if project == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "PROJECT_REQUIRED", "Project is required", nil)
		return
	}
	prompt := domain.SanitizeControlChars(in.Prompt)
	if strings.TrimSpace(prompt) == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "PROMPT_REQUIRED", "Prompt is required", nil)
		return
	}
	if len([]rune(prompt)) > maxProposalPromptLen {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "PROMPT_TOO_LONG", "Prompt is too long", nil)
		return
	}
	reason := domain.SanitizeControlChars(in.Reason)
	if len([]rune(reason)) > maxProposalReasonLen {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "REASON_TOO_LONG", "Reason is too long", nil)
		return
	}
	session := strings.TrimSpace(domain.SanitizeControlChars(in.Session))
	if len([]rune(session)) > maxAnnounceSessionLen {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "SESSION_TOO_LONG", "Session is too long", nil)
		return
	}
	if err := c.Chat.Propose(r.Context(), project, prompt, session, reason); err != nil {
		envelope.WriteAPIError(w, r, http.StatusServiceUnavailable, "unavailable", "CHAT_UNAVAILABLE", err.Error(), nil)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProposeResponse{OK: true, Project: project})
}

// ProposeRequest is the body of POST /api/v1/propose.
type ProposeRequest struct {
	Project string `json:"project"`
	Prompt  string `json:"prompt"`
	Reason  string `json:"reason,omitempty"`
	Session string `json:"session,omitempty"`
}

// ProposeResponse reports that the proposal reached the chat — not that it was
// approved. The answer arrives as a button press, minutes or hours later.
type ProposeResponse struct {
	OK      bool   `json:"ok"`
	Project string `json:"project"`
}
