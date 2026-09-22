package trackerintake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestPollSpawnsWorkerForEligibleIssue(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
				Enabled:  true,
				Assignee: "alice",
			}},
		}},
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title:     "Fix login",
		Body:      "The login form submits twice.",
		State:     domain.IssueOpen,
		URL:       "https://github.com/acme/demo/issues/12",
		Labels:    []string{"agent-ready"},
		Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(spawner.calls))
	}
	call := spawner.calls[0]
	if call.ProjectID != "demo" || call.Kind != domain.KindWorker {
		t.Fatalf("spawn config = %+v", call)
	}
	if call.IssueID != "github:acme/demo#12" {
		t.Fatalf("IssueID = %q, want canonical github id", call.IssueID)
	}
	if !strings.Contains(call.Prompt, "Fix login") || !strings.Contains(call.Prompt, "The login form submits twice.") {
		t.Fatalf("prompt missing issue context:\n%s", call.Prompt)
	}
	if len(tracker.filters) != 1 {
		t.Fatalf("tracker filters = %d, want 1", len(tracker.filters))
	}
	if got := tracker.filters[0]; got.State != domain.ListOpen || got.Assignee != "alice" || len(got.Labels) != 0 {
		t.Fatalf("tracker filter = %+v", got)
	}
}

func TestPollSkipsExistingIssueSessionsAfterRestart(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
		}},
		sessions: []domain.SessionRecord{{ID: "demo-1", ProjectID: "demo", IssueID: "github:acme/demo#12"}},
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title:     "Already running",
		State:     domain.IssueOpen,
		Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0", len(spawner.calls))
	}
}

func TestPollRespawnsIssueAfterTerminatedSession(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
		}},
		sessions: []domain.SessionRecord{{ID: "demo-1", ProjectID: "demo", IssueID: "github:acme/demo#12", IsTerminated: true}},
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title:     "Killed session should respawn",
		State:     domain.IssueOpen,
		Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].IssueID != "github:acme/demo#12" {
		t.Fatalf("spawn calls = %+v, want one spawn for issue #12 (terminated session should not block respawn)", spawner.calls)
	}
}

func TestIssueIsQuarantinedAfterRepeatedFailedAttempts(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	attempts := make([]domain.SessionRecord, 0, quarantineAttempts)
	for i := range quarantineAttempts {
		attempts = append(attempts, domain.SessionRecord{
			ID:           domain.SessionID(fmt.Sprintf("demo-%d", i+1)),
			ProjectID:    "demo",
			IssueID:      "github:acme/demo#12",
			IsTerminated: true,
			CreatedAt:    now.Add(-time.Duration(i+1) * time.Minute),
		})
	}
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
		}},
		sessions: attempts,
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title:     "A card whose sessions keep dying",
		State:     domain.IssueOpen,
		Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}

	observer := New(singleResolver(tracker), store, spawner, Config{
		Clock:  func() time.Time { return now },
		Logger: discardLogger(),
	})
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawn calls = %+v, want none: three attempts died without a PR", spawner.calls)
	}
}

func TestAttemptThatOpenedAPRDoesNotCountTowardsQuarantine(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	sessions := make([]domain.SessionRecord, 0, quarantineAttempts)
	prs := map[domain.SessionID][]domain.PullRequest{}
	for i := range quarantineAttempts {
		id := domain.SessionID(fmt.Sprintf("demo-%d", i+1))
		sessions = append(sessions, domain.SessionRecord{
			ID:           id,
			ProjectID:    "demo",
			IssueID:      "github:acme/demo#12",
			IsTerminated: true,
			CreatedAt:    now.Add(-time.Duration(i+1) * time.Minute),
		})
		prs[id] = []domain.PullRequest{{Number: 100 + i, Merged: true}}
	}
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "demo",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
		}},
		sessions: sessions,
		prs:      prs,
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#12"},
		Title:     "Reopened after each attempt shipped a PR",
		State:     domain.IssueOpen,
		Assignees: []string{"alice"},
	}}}
	spawner := &fakeSpawner{}

	observer := New(singleResolver(tracker), store, spawner, Config{
		Clock:  func() time.Time { return now },
		Logger: discardLogger(),
	})
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawn calls = %+v, want one: attempts that shipped a PR are not failures", spawner.calls)
	}
}

func TestSeenIssueIDsExcludesTerminatedSessions(t *testing.T) {
	sessions := []domain.SessionRecord{
		{ID: "demo-1", IssueID: "github:acme/demo#12", IsTerminated: true},
		{ID: "demo-2", IssueID: "github:acme/demo#12", IsTerminated: false},
	}
	seen := seenIssueIDs(sessions)
	if !seen["github:acme/demo#12"] {
		t.Fatal("issue with a live session alongside a terminated one should still be seen")
	}
	if len(seen) != 1 {
		t.Fatalf("seen = %+v, want exactly one issue", seen)
	}
}

func TestSeenIssueIDsIgnoresOnlyTerminatedSession(t *testing.T) {
	sessions := []domain.SessionRecord{
		{ID: "demo-1", IssueID: "github:acme/demo#12", IsTerminated: true},
	}
	seen := seenIssueIDs(sessions)
	if seen["github:acme/demo#12"] {
		t.Fatal("issue with only a terminated session should not be marked as seen")
	}
}

func TestPollSkipsSessionScanWhenIntakeDisabled(t *testing.T) {
	store := &fakeStore{
		projects:    []domain.ProjectRecord{{ID: "demo"}},
		sessionsErr: errors.New("session scan should not run"),
	}

	if err := New(singleResolver(&fakeTracker{}), store, &fakeSpawner{}, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v, want nil", err)
	}
}

func TestPollSkipsIneligibleAndInvalidProjects(t *testing.T) {
	store := &fakeStore{
		projects: []domain.ProjectRecord{
			{ID: "off", RepoOriginURL: "https://github.com/acme/off.git"},
			{ID: "broad", RepoOriginURL: "https://github.com/acme/broad.git", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true}}},
			{ID: "missing-origin", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}}},
		},
	}
	tracker := &fakeTracker{issues: []domain.Issue{{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/off#1"},
		Title: "ignored",
		State: domain.IssueOpen,
	}}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(tracker.repos) != 0 {
		t.Fatalf("tracker was called for invalid/off projects: %+v", tracker.repos)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0", len(spawner.calls))
	}
}

func TestPollContinuesAfterTrackerAndSpawnFailures(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{
		{ID: "bad", RepoOriginURL: "https://github.com/acme/bad.git", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}}},
		{ID: "good", RepoOriginURL: "https://github.com/acme/good.git", Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}}},
	}}
	tracker := &fakeTracker{
		failRepos: map[string]error{"acme/bad": errors.New("rate limited")},
		issuesByRepo: map[string][]domain.Issue{
			"acme/good": {
				{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/good#1"}, Title: "first", State: domain.IssueOpen, Assignees: []string{"alice"}},
				{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/good#2"}, Title: "second", State: domain.IssueOpen, Assignees: []string{"alice"}},
			},
		},
	}
	spawner := &fakeSpawner{failIssue: domain.IssueID("github:acme/good#1")}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 2 {
		t.Fatalf("spawn attempts = %d, want 2", len(spawner.calls))
	}
	if spawner.calls[1].IssueID != "github:acme/good#2" {
		t.Fatalf("second spawn issue = %q", spawner.calls[1].IssueID)
	}
}

func TestPollBacksOffProjectAfterFailure(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "demo",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{failRepos: map[string]error{"acme/demo": errors.New("rate limited")}}
	observer := New(singleResolver(tracker), store, &fakeSpawner{}, Config{
		Clock:          func() time.Time { return now },
		FailureBackoff: time.Minute,
		Logger:         discardLogger(),
	})

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("first Poll() error = %v", err)
	}
	if len(tracker.repos) != 1 {
		t.Fatalf("tracker calls after first poll = %d, want 1", len(tracker.repos))
	}

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("second Poll() error = %v", err)
	}
	if len(tracker.repos) != 1 {
		t.Fatalf("tracker calls during backoff = %d, want still 1", len(tracker.repos))
	}

	now = now.Add(time.Minute + time.Nanosecond)
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatalf("third Poll() error = %v", err)
	}
	if len(tracker.repos) != 2 {
		t.Fatalf("tracker calls after backoff = %d, want 2", len(tracker.repos))
	}
}

func TestPollSkipsNonOpenIssueStates(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "demo",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#1"}, Title: "already active", State: domain.IssueInProgress, Assignees: []string{"alice"}},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#2"}, Title: "ready", State: domain.IssueOpen, Assignees: []string{"alice"}},
	}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].IssueID != "github:acme/demo#2" {
		t.Fatalf("spawn calls = %+v, want only open issue #2", spawner.calls)
	}
}

func TestPollAppliesLocalEligibilityFilter(t *testing.T) {
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "demo",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config:        domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{Enabled: true, Assignee: "alice"}},
	}}}
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#1"}, Title: "unassigned", State: domain.IssueOpen},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#2"}, Title: "wrong assignee", State: domain.IssueOpen, Assignees: []string{"bob"}},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#3"}, Title: "eligible", State: domain.IssueOpen, Labels: []string{"Agent-Ready"}, Assignees: []string{"Alice"}},
	}}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(spawner.calls) != 1 || spawner.calls[0].IssueID != "github:acme/demo#3" {
		t.Fatalf("spawn calls = %+v, want only eligible issue #3", spawner.calls)
	}
}

func TestIssueMatchesConfigAssigneeSpecialValues(t *testing.T) {
	assigned := domain.Issue{Assignees: []string{"alice"}}
	unassigned := domain.Issue{}
	if !issueMatchesConfig(assigned, domain.TrackerIntakeConfig{Assignee: "*"}) {
		t.Fatal("assigned issue should match assignee=*")
	}
	if issueMatchesConfig(unassigned, domain.TrackerIntakeConfig{Assignee: "*"}) {
		t.Fatal("unassigned issue should not match assignee=*")
	}
	if !issueMatchesConfig(unassigned, domain.TrackerIntakeConfig{Assignee: "none"}) {
		t.Fatal("unassigned issue should match assignee=none")
	}
	if issueMatchesConfig(assigned, domain.TrackerIntakeConfig{Assignee: "none"}) {
		t.Fatal("assigned issue should not match assignee=none")
	}
}

func TestIssueMatchesConfigLabels(t *testing.T) {
	// A label is the other way to mark an issue ready, and the only one open to
	// tooling that has no repository account: an App's bot cannot be an
	// assignee, and GitHub drops an unknown login silently.
	ready := domain.Issue{Labels: []string{"type:bug", "Ready-For-Agent"}}
	plain := domain.Issue{Labels: []string{"type:bug"}}
	cfg := domain.TrackerIntakeConfig{Labels: []string{"ready-for-agent"}}

	if !issueMatchesConfig(ready, cfg) {
		t.Fatal("labelled issue should match; comparison is case-insensitive")
	}
	if issueMatchesConfig(plain, cfg) {
		t.Fatal("issue without the label should not match")
	}
	// Every configured label must be present: two markers mean two conditions.
	both := domain.TrackerIntakeConfig{Labels: []string{"ready-for-agent", "P1"}}
	if issueMatchesConfig(ready, both) {
		t.Fatal("issue missing one of the labels should not match")
	}
	// The label check runs on top of the assignee rule, not instead of it.
	withAssignee := domain.TrackerIntakeConfig{Labels: []string{"ready-for-agent"}, Assignee: "alice"}
	if issueMatchesConfig(ready, withAssignee) {
		t.Fatal("labelled issue with no assignee should not match an assignee rule")
	}
}

func TestTrackerIntakeConfigAcceptsLabelsWithoutAssignee(t *testing.T) {
	cfg := domain.TrackerIntakeConfig{
		Enabled:  true,
		Provider: domain.TrackerProviderGitHub,
		Repo:     "acme/demo",
		Labels:   []string{"ready-for-agent"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil: a label narrows intake as well as an assignee", err)
	}
	bare := domain.TrackerIntakeConfig{Enabled: true, Provider: domain.TrackerProviderGitHub, Repo: "acme/demo"}
	if err := bare.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error: intake without any narrowing rule drains the backlog")
	}
	board := domain.TrackerIntakeConfig{
		Enabled:   true,
		Provider:  domain.TrackerProviderGitHubProjects,
		ProjectID: "PVT_x",
		Labels:    []string{"ready-for-agent"},
	}
	if err := board.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error: the board provider has a column, labels are not its rule")
	}
}

func TestBuildIssuePromptCapsLargeIssueBody(t *testing.T) {
	prompt := BuildIssuePrompt(domain.Issue{
		ID:    domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#99"},
		Title: "Large issue",
		URL:   "https://github.com/acme/demo/issues/99",
		Body:  strings.Repeat("body ", 2000),
	})
	if len(prompt) > maxIntakePromptLen {
		t.Fatalf("prompt length = %d, want <= %d", len(prompt), maxIntakePromptLen)
	}
	if !strings.Contains(prompt, "Issue content truncated") {
		t.Fatalf("prompt missing truncation notice:\n%s", prompt)
	}
	if !strings.Contains(prompt, "https://github.com/acme/demo/issues/99") {
		t.Fatalf("prompt missing issue URL:\n%s", prompt)
	}
	if !strings.HasSuffix(prompt, intakePromptFooter) {
		t.Fatalf("prompt missing footer:\n%s", prompt)
	}
}

func TestTrackerRepoUsesConfiguredRepo(t *testing.T) {
	project := domain.ProjectRecord{
		ID:            "demo",
		RepoOriginURL: "https://github.com/wrong/repo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
			Enabled:  true,
			Repo:     "acme/demo",
			Assignee: "alice",
		}},
	}
	repo, ok := trackerRepo(project, project.Config.TrackerIntake.WithDefaults())
	if !ok {
		t.Fatal("trackerRepo ok = false")
	}
	if repo.Native != "acme/demo" {
		t.Fatalf("repo.Native = %q, want acme/demo", repo.Native)
	}
}

func singleResolver(tracker ports.Tracker) TrackerResolver {
	return SingleTrackerResolver{Provider: domain.TrackerProviderGitHub, Adapter: tracker}
}

type fakeStore struct {
	projects    []domain.ProjectRecord
	sessions    []domain.SessionRecord
	sessionsErr error
	// prs maps a session id to the PRs the store reports for it.
	prs    map[domain.SessionID][]domain.PullRequest
	prsErr error
}

func (f *fakeStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return append([]domain.ProjectRecord(nil), f.projects...), nil
}

func (f *fakeStore) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return append([]domain.SessionRecord(nil), f.sessions...), f.sessionsErr
}

func (f *fakeStore) ListPRsBySession(_ context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	if f.prsErr != nil {
		return nil, f.prsErr
	}
	return append([]domain.PullRequest(nil), f.prs[id]...), nil
}

type fakeTracker struct {
	issues       []domain.Issue
	issuesByRepo map[string][]domain.Issue
	failRepos    map[string]error
	repos        []domain.TrackerRepo
	filters      []domain.ListFilter
}

func (f *fakeTracker) Get(_ context.Context, id domain.TrackerID) (domain.Issue, error) {
	for _, issue := range f.issues {
		if strings.EqualFold(issue.ID.Native, id.Native) {
			return issue, nil
		}
	}
	for _, issues := range f.issuesByRepo {
		for _, issue := range issues {
			if strings.EqualFold(issue.ID.Native, id.Native) {
				return issue, nil
			}
		}
	}
	return domain.Issue{}, nil
}

func (f *fakeTracker) List(_ context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error) {
	f.repos = append(f.repos, repo)
	f.filters = append(f.filters, filter)
	if err := f.failRepos[repo.Native]; err != nil {
		return nil, err
	}
	if f.issuesByRepo != nil {
		return append([]domain.Issue(nil), f.issuesByRepo[repo.Native]...), nil
	}
	return append([]domain.Issue(nil), f.issues...), nil
}

func (f *fakeTracker) Preflight(context.Context) error { return nil }

type fakeSpawner struct {
	calls     []ports.SpawnConfig
	failIssue domain.IssueID
}

func (f *fakeSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.Session, int, int, error) {
	f.calls = append(f.calls, cfg)
	if cfg.IssueID == f.failIssue {
		return domain.Session{}, 0, 0, errors.New("spawn failed")
	}
	return domain.Session{SessionRecord: domain.SessionRecord{ID: domain.SessionID(string(cfg.ProjectID) + "-1"), ProjectID: cfg.ProjectID, IssueID: cfg.IssueID, Kind: cfg.Kind}}, len(cfg.Prompt), 0, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIntakeRespectsMaxConcurrent(t *testing.T) {
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#1"}, State: domain.IssueOpen},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#2"}, State: domain.IssueOpen},
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#3"}, State: domain.IssueOpen},
	}}
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "proj",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
				Enabled:       true,
				Provider:      domain.TrackerProviderGitHub,
				Assignee:      "*",
				MaxConcurrent: 2,
			}},
		}},
	}
	for i := range tracker.issues {
		tracker.issues[i].Assignees = []string{"octocat"}
	}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.calls) != 2 {
		t.Fatalf("spawned %d sessions, want the 2 allowed by maxConcurrent", len(spawner.calls))
	}
}

func TestIntakeCountsExistingLiveSessionsAgainstTheCap(t *testing.T) {
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#7"}, State: domain.IssueOpen, Assignees: []string{"octocat"}},
	}}
	store := &fakeStore{
		projects: []domain.ProjectRecord{{
			ID:            "proj",
			RepoOriginURL: "https://github.com/acme/demo.git",
			Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
				Enabled:       true,
				Provider:      domain.TrackerProviderGitHub,
				Assignee:      "*",
				MaxConcurrent: 1,
			}},
		}},
		sessions: []domain.SessionRecord{{
			ID:        "proj-1",
			ProjectID: "proj",
			IssueID:   "github:acme/demo#5",
		}},
	}
	spawner := &fakeSpawner{}

	if err := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger()}).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawned %d sessions, want none: the single slot is already taken by a live session", len(spawner.calls))
	}
}

func TestTrackerRepoScopesBoardProviderToTheProjectRepo(t *testing.T) {
	project := domain.ProjectRecord{ID: "proj", RepoOriginURL: "https://github.com/acme/demo.git"}
	cfg := domain.TrackerIntakeConfig{
		Enabled:   true,
		Provider:  domain.TrackerProviderGitHubProjects,
		ProjectID: "PVT_board",
	}

	repo, ok := trackerRepo(project, cfg)
	if !ok {
		t.Fatal("board-backed intake must still resolve a repo scope: a board can span several repositories")
	}
	if repo.Native != "acme/demo" {
		t.Fatalf("repo.Native = %q, want acme/demo", repo.Native)
	}
	if repo.Provider != domain.TrackerProviderGitHubProjects {
		t.Fatalf("repo.Provider = %q, want github-projects", repo.Provider)
	}
}

func TestPausedGateStopsClaiming(t *testing.T) {
	tracker := &fakeTracker{issues: []domain.Issue{
		{ID: domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#1"}, State: domain.IssueOpen, Assignees: []string{"octocat"}},
	}}
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "proj",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
			Enabled: true, Provider: domain.TrackerProviderGitHub, Assignee: "*",
		}},
	}}}
	spawner := &fakeSpawner{}
	gate := &Gate{}
	gate.Pause()

	observer := New(singleResolver(tracker), store, spawner, Config{Logger: discardLogger(), Gate: gate})
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("a paused gate must not claim cards, spawned %d", len(spawner.calls))
	}

	gate.Resume()
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("after resume the card must be claimed, spawned %d", len(spawner.calls))
	}
}

type recordingAnnouncer struct{ messages []string }

func (r *recordingAnnouncer) Announce(text string) { r.messages = append(r.messages, text) }

func TestAnnouncerHearsAboutClaimedCards(t *testing.T) {
	tracker := &fakeTracker{issues: []domain.Issue{
		{
			ID:        domain.TrackerID{Provider: domain.TrackerProviderGitHub, Native: "acme/demo#16"},
			Title:     "агент уходит в луп",
			State:     domain.IssueOpen,
			Assignees: []string{"octocat"},
		},
	}}
	store := &fakeStore{projects: []domain.ProjectRecord{{
		ID:            "proj",
		RepoOriginURL: "https://github.com/acme/demo.git",
		Config: domain.ProjectConfig{TrackerIntake: domain.TrackerIntakeConfig{
			Enabled: true, Provider: domain.TrackerProviderGitHub, Assignee: "*",
		}},
	}}}
	announcer := &recordingAnnouncer{}

	if err := New(singleResolver(tracker), store, &fakeSpawner{}, Config{Logger: discardLogger(), Announcer: announcer}).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(announcer.messages) != 1 {
		t.Fatalf("got %d announcements, want 1", len(announcer.messages))
	}
	for _, want := range []string{"acme/demo#16", "агент уходит в луп", "proj-1"} {
		if !strings.Contains(announcer.messages[0], want) {
			t.Errorf("announcement missing %q:\n%s", want, announcer.messages[0])
		}
	}
}
