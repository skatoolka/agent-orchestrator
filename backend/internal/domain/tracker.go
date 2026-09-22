package domain

import (
	"fmt"
	"strings"
)

// TrackerProvider identifies an issue-tracker provider implementation.
type TrackerProvider string

// The supported issue-tracker providers.
const (
	// TrackerProviderGitHub reads the repository's issue list.
	TrackerProviderGitHub TrackerProvider = "github"
	// TrackerProviderGitHubProjects reads a GitHub Projects v2 board: the
	// queue is a Status column, not the issue list. Issue ids stay in
	// GitHub's native "owner/repo#123" form, so everything downstream of
	// intake (PR linking, Get, session metadata) is unchanged.
	TrackerProviderGitHubProjects TrackerProvider = "github-projects"
)

// DefaultProjectReadyStatus is the Status column intake claims cards from when
// a project config does not name one.
const DefaultProjectReadyStatus = "Ready"

// TrackerID identifies one issue. Native is the provider's own canonical form
// ("owner/repo#123" for GitHub) and is parsed by the adapter.
type TrackerID struct {
	Provider TrackerProvider `json:"provider"`
	Native   string          `json:"native"`
}

// NormalizedIssueState is the cross-provider issue-state vocabulary every
// adapter must implement. The closed list is intentional — adding a value
// here is a port-level decision because every adapter must map it.
type NormalizedIssueState string

// The normalized cross-provider issue states.
const (
	IssueOpen       NormalizedIssueState = "open"
	IssueInProgress NormalizedIssueState = "in_progress"
	IssueInReview   NormalizedIssueState = "review"
	IssueDone       NormalizedIssueState = "done"
	IssueCancelled  NormalizedIssueState = "cancelled"
)

// Issue is the minimum projection every tracker can produce. Provider-specific
// metadata stays inside provider-specific code paths.
type Issue struct {
	ID        TrackerID            `json:"id"`
	Title     string               `json:"title"`
	Body      string               `json:"body"`
	State     NormalizedIssueState `json:"state"`
	URL       string               `json:"url"`
	Labels    []string             `json:"labels,omitempty"`
	Assignees []string             `json:"assignees,omitempty"`
}

// TrackerRepo identifies a repository for cross-issue queries like Tracker.List.
// Native is the provider's canonical owner/project form, e.g. "owner/repo" for
// GitHub.
type TrackerRepo struct {
	Provider TrackerProvider `json:"provider"`
	Native   string          `json:"native"`
}

// ListStateFilter narrows Tracker.List results by the provider's coarse
// state (open vs closed). It is intentionally NOT the 5-value normalized
// enum — finer filtering (e.g. "only in-review issues") goes through the
// Labels field of ListFilter.
type ListStateFilter string

// Coarse list-state filters for Tracker.List.
const (
	// ListAll is the zero value and returns issues in any state.
	ListAll    ListStateFilter = ""
	ListOpen   ListStateFilter = "open"
	ListClosed ListStateFilter = "closed"
)

// ListFilter is the query the Session Manager passes to Tracker.List.
// Empty / zero values mean "no filter on this dimension".
//
// Limit is an optional total-result cap. Adapters choose their own provider
// page size.
type ListFilter struct {
	State    ListStateFilter `json:"state,omitempty"`
	Labels   []string        `json:"labels,omitempty"`
	Assignee string          `json:"assignee,omitempty"`
	Limit    int             `json:"limit,omitempty"`
}

// TrackerIntakeConfig controls issue-driven worker spawning for a project.
// Enabled requires an explicit assignee eligibility rule so turning intake on
// cannot accidentally drain an entire issue backlog.
type TrackerIntakeConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Provider defaults to github when Enabled is true.
	Provider TrackerProvider `json:"provider,omitempty" enum:"github,github-projects"`
	// Repo is the GitHub-native repository key ("owner/repo"). When empty, the
	// intake loop derives it from the project's repo origin URL. GitHub only.
	Repo string `json:"repo,omitempty"`
	// Assignee narrows eligible issues to one assignee. Provider-specific values
	// such as "*" are passed through unchanged.
	Assignee string `json:"assignee,omitempty"`
	// Labels narrows eligible issues to those carrying ALL of these labels.
	// github only. It is the other way to mark an issue ready, and the only one
	// open to tooling without an account of its own: an assignee must be a user
	// the repository accepts, and a GitHub App's bot is not one — measured
	// 2026-09-22, GET /repos/<repo>/assignees/<app>[bot] answers 404 and the
	// POST answers 403. An unknown login is worse still: it is dropped
	// silently, with 201 and an empty assignees list.
	Labels []string `json:"labels,omitempty"`
	// ProjectID is the Projects v2 board node id ("PVT_..."). Required by the
	// github-projects provider and ignored by github. A node id is used rather
	// than owner+number because it resolves identically for user- and
	// organization-owned boards.
	ProjectID string `json:"projectId,omitempty"`
	// ReadyStatus is the Status column intake claims cards from. Empty means
	// DefaultProjectReadyStatus. github-projects only.
	ReadyStatus string `json:"readyStatus,omitempty"`
	// MaxConcurrent caps how many live intake-started sessions a project may
	// have at once. Zero means unlimited. It is the throttle between a triaged
	// backlog and the operator's agent-provider rate limits: without it,
	// enabling intake on a board with a full Ready column spawns one session
	// per card in a single tick.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
}

// WithDefaults fills the provider only when intake is enabled. Disabled intake
// leaves the zero value untouched so empty project configs still store as NULL.
func (c TrackerIntakeConfig) WithDefaults() TrackerIntakeConfig {
	if c.Enabled && c.Provider == "" {
		c.Provider = TrackerProviderGitHub
	}
	if c.Enabled && c.Provider == TrackerProviderGitHubProjects && strings.TrimSpace(c.ReadyStatus) == "" {
		c.ReadyStatus = DefaultProjectReadyStatus
	}
	return c
}

// Validate rejects accidental broad intake and unknown providers.
func (c TrackerIntakeConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	c = c.WithDefaults()
	if c.Provider != TrackerProviderGitHub && c.Provider != TrackerProviderGitHubProjects {
		return fmt.Errorf("trackerIntake.provider: unsupported provider %q", c.Provider)
	}
	if err := validateNoWhitespaceField("trackerIntake.repo", c.Repo); err != nil {
		return err
	}
	if err := validateNoWhitespaceField("trackerIntake.assignee", c.Assignee); err != nil {
		return err
	}
	if err := validateNoWhitespaceField("trackerIntake.projectId", c.ProjectID); err != nil {
		return err
	}
	if c.MaxConcurrent < 0 {
		return fmt.Errorf("trackerIntake.maxConcurrent: must not be negative, got %d", c.MaxConcurrent)
	}
	if len(c.Labels) > 0 && c.Provider == TrackerProviderGitHubProjects {
		return fmt.Errorf("trackerIntake.labels: only supported by provider %q", TrackerProviderGitHub)
	}
	if c.Provider == TrackerProviderGitHubProjects {
		// The board column is the eligibility rule here, so an assignee is not
		// required — but the board itself must be named, otherwise there is
		// nothing to poll.
		if strings.TrimSpace(c.ProjectID) == "" {
			return fmt.Errorf("trackerIntake: projectId is required for provider %q", c.Provider)
		}
		return nil
	}
	if strings.TrimSpace(c.ProjectID) != "" {
		return fmt.Errorf("trackerIntake.projectId: only supported by provider %q", TrackerProviderGitHubProjects)
	}
	if strings.TrimSpace(c.ReadyStatus) != "" {
		return fmt.Errorf("trackerIntake.readyStatus: only supported by provider %q", TrackerProviderGitHubProjects)
	}
	for _, label := range c.Labels {
		if err := validateNoWhitespaceField("trackerIntake.labels", label); err != nil {
			return err
		}
	}
	// Plain issue-list intake has no column to narrow on, so an explicit rule —
	// an assignee or a label — is the only thing standing between intake and
	// draining the whole backlog.
	if strings.TrimSpace(c.Assignee) == "" && len(c.Labels) == 0 {
		return fmt.Errorf("trackerIntake: assignee or labels are required when enabled")
	}
	return nil
}
