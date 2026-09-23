// Package trackerintake implements the opt-in issue-intake observer. It polls a
// project's configured tracker for eligible issues and starts one worker session
// per issue, leaving PR/lifecycle handling to the existing observers.
package trackerintake

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// quarantineWindow and quarantineAttempts bound the failure-loop guard: this
	// many sessions that ended without a PR inside this window take the card out
	// of intake until a human looks at it.
	quarantineWindow   = time.Hour
	quarantineAttempts = 3

	// DefaultTickInterval is intentionally slower than runtime liveness checks:
	// intake is a backlog sweep, not an interactive status surface.
	DefaultTickInterval = time.Minute
	// DefaultFailureBackoff suppresses repeated polls for a project after an
	// intake failure. The observer retries automatically after this window.
	DefaultFailureBackoff = 5 * time.Minute
	// maxIntakePromptLen mirrors the session HTTP prompt limit. Intake uses the
	// session service directly, so it must enforce the same boundary itself.
	maxIntakePromptLen = 4096

	intakePromptTruncationNotice = "\n\n[Issue content truncated to fit the session prompt limit. Open the linked issue for the full details.]\n"
	intakePromptFooter           = "\nImplement the requested change in this repository, run the relevant checks, and open or update a pull request when ready."
)

// Store is the durable read surface the observer needs.
type Store interface {
	ListProjects(ctx context.Context) ([]domain.ProjectRecord, error)
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
	// ListPRsBySession backs the "waiting on a human" test below: a session that
	// already has an open PR is not doing work any more.
	ListPRsBySession(ctx context.Context, sessionID domain.SessionID) ([]domain.PullRequest, error)
}

// Spawner is the session creation surface used by intake.
type Spawner interface {
	Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.Session, int, int, error)
}

// TrackerResolver picks the tracker adapter for a project's intake config. It
// takes the whole config, not just the provider, because a board-backed
// provider is only addressable together with its board id and ready column.
type TrackerResolver interface {
	Resolve(cfg domain.TrackerIntakeConfig) (ports.Tracker, error)
}

// SingleTrackerResolver returns the same tracker for one specific provider and
// refuses every other provider. It exists so single-provider deployments don't
// need to construct a map.
type SingleTrackerResolver struct {
	Provider domain.TrackerProvider
	Adapter  ports.Tracker
}

// Resolve returns the wrapped adapter when the requested provider matches, or
// when the resolver was constructed without a provider pin.
func (s SingleTrackerResolver) Resolve(cfg domain.TrackerIntakeConfig) (ports.Tracker, error) {
	provider := cfg.Provider
	if s.Adapter == nil {
		return nil, fmt.Errorf("tracker intake: no adapter for provider %q", provider)
	}
	if s.Provider == "" || provider == "" || provider == s.Provider {
		return s.Adapter, nil
	}
	return nil, fmt.Errorf("tracker intake: no adapter for provider %q", provider)
}

// Gate is the intake pause switch. It exists so a human ("/pause" from chat) and
// the agent provider's usage limits can both stop new work from being claimed
// without tearing down the sessions already running.
//
// The zero value is usable and un-paused.
type Gate struct{ paused atomic.Bool }

// Pause stops new cards from being claimed. Live sessions are untouched.
func (g *Gate) Pause() { g.paused.Store(true) }

// Resume re-enables claiming.
func (g *Gate) Resume() { g.paused.Store(false) }

// Paused reports whether claiming is currently suspended.
func (g *Gate) Paused() bool { return g != nil && g.paused.Load() }

// Announcer receives human-facing conveyor events. It is optional: without a
// notifier configured, intake stays silent.
type Announcer interface {
	Announce(text string)
}

// Config holds optional observer knobs. Zero values use production defaults.
// DefaultIdleRelease is how long a session may show NO activity before it
// stops holding a concurrency slot.
//
// The cap exists to bound what agents spend — tokens and parallel work. A
// session that has shown nothing for an hour spends nothing, but under the
// old rule it held the queue forever. Measured 2026-09-22: a conveyor restart
// restored the panes, the agents came back sitting at an empty prompt (status
// `no_signal` — AO cannot tell working from stuck), and two claimed cards
// waited indefinitely behind two sessions that were doing nothing at all.
//
// An hour is far longer than any healthy gap between hook callbacks and far
// shorter than the days a forgotten session can linger.
const DefaultIdleRelease = time.Hour

type Config struct {
	Tick           time.Duration
	FailureBackoff time.Duration
	// IdleRelease overrides DefaultIdleRelease. Negative disables the release
	// entirely — every unterminated session holds its slot, as before.
	IdleRelease time.Duration
	Clock       func() time.Time
	Logger      *slog.Logger
	// Gate, when set, suspends claiming while paused.
	Gate *Gate
	// Announcer, when set, is told about each claimed card.
	Announcer Announcer
}

// Observer polls configured projects and starts sessions for eligible issues.
type Observer struct {
	resolver       TrackerResolver
	store          Store
	spawner        Spawner
	tick           time.Duration
	failureBackoff time.Duration
	clock          func() time.Time
	logger         *slog.Logger
	idleRelease    time.Duration
	backoffUntil   map[string]time.Time
	// quarantined remembers issues intake stopped claiming, so the chat hears
	// about each one once instead of on every tick.
	quarantined map[domain.IssueID]bool
	gate        *Gate
	announcer   Announcer
}

// New constructs an Observer with safe defaults.
func New(resolver TrackerResolver, store Store, spawner Spawner, cfg Config) *Observer {
	o := &Observer{resolver: resolver, store: store, spawner: spawner, tick: cfg.Tick, failureBackoff: cfg.FailureBackoff, clock: cfg.Clock, logger: cfg.Logger, idleRelease: cfg.IdleRelease, backoffUntil: map[string]time.Time{}, quarantined: map[domain.IssueID]bool{}, gate: cfg.Gate, announcer: cfg.Announcer}
	if o.tick <= 0 {
		o.tick = DefaultTickInterval
	}
	if o.failureBackoff <= 0 {
		o.failureBackoff = DefaultFailureBackoff
	}
	if o.idleRelease == 0 {
		o.idleRelease = DefaultIdleRelease
	}
	if o.clock == nil {
		o.clock = time.Now
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	return o
}

// Start launches the observer loop. The first poll runs immediately inside the
// goroutine, keeping daemon startup non-blocking.
func (o *Observer) Start(ctx context.Context) <-chan struct{} {
	return observe.StartPollLoop(ctx, o.tick, o.Poll, o.logger, "tracker intake")
}

// Poll runs one synchronous intake pass. Store discovery failures are returned
// because they prevent the pass from knowing the current world; provider and
// spawn failures are logged and skipped so one bad issue/project does not block
// the rest of the daemon.
func (o *Observer) Poll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.resolver == nil || o.store == nil || o.spawner == nil {
		return nil
	}
	if o.gate.Paused() {
		o.logger.Debug("tracker intake: paused, not claiming new cards")
		return nil
	}
	now := o.clock().UTC()
	projects, err := o.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	enabledProjects := make([]domain.ProjectRecord, 0, len(projects))
	for _, project := range projects {
		if project.Config.TrackerIntake.Enabled {
			enabledProjects = append(enabledProjects, project)
		}
	}
	if len(enabledProjects) == 0 {
		return nil
	}
	sessions, err := o.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	seen := seenIssueIDs(sessions)
	live := o.liveIntakeSessions(ctx, sessions)
	o.quarantineFailedIssues(ctx, sessions, now)
	for _, project := range enabledProjects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if until, ok := o.backoffUntil[project.ID]; ok && now.Before(until) {
			o.logger.Debug("tracker intake: project in failure backoff", "project", project.ID, "until", until)
			continue
		}
		if failed := o.pollProject(ctx, project, seen, live); failed {
			o.backoffUntil[project.ID] = now.Add(o.failureBackoff)
		} else {
			delete(o.backoffUntil, project.ID)
		}
	}
	return nil
}

// pollProject returns failed=true for conditions that should be retried after a
// backoff window rather than logged on every poll.
func (o *Observer) pollProject(ctx context.Context, project domain.ProjectRecord, seen map[domain.IssueID]bool, live map[domain.ProjectID]int) (failed bool) {
	cfg := project.Config.TrackerIntake.WithDefaults()
	if !cfg.Enabled {
		return false
	}
	if err := cfg.Validate(); err != nil {
		o.logger.Warn("tracker intake: skipping project with invalid config", "project", project.ID, "err", err)
		return true
	}
	repo, ok := trackerRepo(project, cfg)
	if !ok {
		o.logger.Warn("tracker intake: skipping project without tracker scope", "project", project.ID, "provider", cfg.Provider, "origin", project.RepoOriginURL)
		return true
	}
	tracker, err := o.resolver.Resolve(cfg)
	if err != nil {
		o.logger.Warn("tracker intake: no adapter for provider", "project", project.ID, "provider", cfg.Provider, "err", err)
		return true
	}
	issues, err := tracker.List(ctx, repo, domain.ListFilter{
		State:    domain.ListOpen,
		Assignee: cfg.Assignee,
		Labels:   cfg.Labels,
	})
	if err != nil {
		o.logger.Error("tracker intake: list issues failed", "project", project.ID, "repo", repo.Native, "err", err)
		return true
	}
	// MaxConcurrent throttles a triaged backlog against the agent provider's
	// rate limits: without it, a board with a full ready column spawns one
	// session per card on the first tick.
	projectID := domain.ProjectID(project.ID)
	budget := -1
	if cfg.MaxConcurrent > 0 {
		budget = cfg.MaxConcurrent - live[projectID]
		if budget <= 0 {
			o.logger.Debug("tracker intake: project at its concurrency cap", "project", project.ID, "live", live[projectID], "max", cfg.MaxConcurrent)
			return false
		}
	}
	var spawnFailed bool
	for _, issue := range issues {
		if budget == 0 {
			o.logger.Info("tracker intake: concurrency cap reached, remaining cards stay queued", "project", project.ID, "max", cfg.MaxConcurrent)
			break
		}
		if ctx.Err() != nil {
			return true
		}
		if issue.State != domain.IssueOpen {
			continue
		}
		if !issueMatchesConfig(issue, cfg) {
			continue
		}
		issueID := CanonicalIssueID(issue.ID)
		if issueID == "" || seen[issueID] {
			continue
		}
		if o.quarantined[issueID] {
			continue
		}
		session, _, _, err := o.spawner.Spawn(ctx, ports.SpawnConfig{
			ProjectID: projectID,
			IssueID:   issueID,
			Kind:      domain.KindWorker,
			Prompt:    BuildIssuePrompt(issue),
			// A merged PR means the task is done: tear the session down instead
			// of leaving its worktree and agent process behind.
			TerminateOnPRMerge: true,
		})
		if err != nil {
			o.logger.Error("tracker intake: spawn issue session failed", "project", project.ID, "issue", issueID, "err", err)
			spawnFailed = true
			continue
		}
		if o.announcer != nil {
			o.announcer.Announce(fmt.Sprintf("🤖 взял в работу %s\n%s\n\nсессия: %s · проект: %s", issue.ID.Native, strings.TrimSpace(issue.Title), session.ID, project.ID))
		}
		seen[issueID] = true
		live[projectID]++
		if budget > 0 {
			budget--
		}
	}
	return spawnFailed
}

// liveIntakeSessions counts the sessions that still occupy a concurrency slot:
// started from an issue, not yet terminated, not parked on an open pull
// request, and showing at least some sign of life.
//
// The PR test is what keeps the conveyor moving. A session that opened its PR
// is waiting on a human review-and-merge, which can take hours or days; counting
// it as busy meant a cap of N stalled the whole board after N cards, with every
// agent idle. Merged and closed PRs do not park a session: there the agent is
// either done (and about to be torn down) or back at work on the same issue.
//
// The idle test is the other half of the same problem, and it cost a day to
// find. A session that has gone quiet — no hook callback, no activity — is not
// spending the tokens the cap protects, yet it held the queue forever: nothing
// terminates such a session on its own. Measured 2026-09-22: restarting the
// conveyor restored every pane, the agents came back sitting at an EMPTY
// PROMPT (status `no_signal`, which means «AO cannot tell working from stuck»),
// and two claimed cards waited behind them indefinitely while both agents did
// nothing at all. A session with no activity stamp yet is NOT idle — it has
// just spawned, and that is exactly when it is about to work.
func (o *Observer) liveIntakeSessions(ctx context.Context, sessions []domain.SessionRecord) map[domain.ProjectID]int {
	live := map[domain.ProjectID]int{}
	now := o.clock()
	for _, session := range sessions {
		if session.IsTerminated || session.IssueID == "" {
			continue
		}
		if o.parkedOnOpenPR(ctx, session.ID) {
			continue
		}
		if o.idleRelease > 0 && !session.Activity.LastActivityAt.IsZero() &&
			now.Sub(session.Activity.LastActivityAt) >= o.idleRelease {
			o.logger.Debug("tracker intake: session idle, slot released",
				"session", session.ID, "idle", now.Sub(session.Activity.LastActivityAt).Round(time.Minute))
			continue
		}
		live[session.ProjectID]++
	}
	return live
}

// parkedOnOpenPR reports whether the session has a pull request still open. A
// read failure counts the session as busy: over-counting only delays a claim,
// while under-counting would spawn past the cap.
func (o *Observer) parkedOnOpenPR(ctx context.Context, id domain.SessionID) bool {
	prs, err := o.store.ListPRsBySession(ctx, id)
	if err != nil {
		o.logger.Warn("tracker intake: could not read session PRs, counting it against the cap", "session", id, "err", err)
		return false
	}
	for _, pr := range prs {
		if !pr.Merged && !pr.Closed {
			return true
		}
	}
	return false
}

func issueMatchesConfig(issue domain.Issue, cfg domain.TrackerIntakeConfig) bool {
	// Labels are re-checked locally for the same reason the assignee is: the
	// provider filter is a query hint, and a stale list must not start work on
	// an issue whose marker has already been removed.
	for _, label := range cfg.Labels {
		if !containsFold(issue.Labels, strings.TrimSpace(label)) {
			return false
		}
	}
	assignee := strings.TrimSpace(cfg.Assignee)
	switch {
	case assignee == "":
		return true
	case assignee == "*":
		return len(issue.Assignees) > 0
	case strings.EqualFold(assignee, "none"):
		return len(issue.Assignees) == 0
	default:
		return containsFold(issue.Assignees, assignee)
	}
}

func containsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), needle) {
			return true
		}
	}
	return false
}

// quarantineFailedIssues stops intake from re-claiming a card whose sessions
// keep dying before they produce anything.
//
// A card is claimed, its session ends without ever opening a PR, the card is
// still sitting in the ready column, so intake claims it again — a loop that
// burns an agent run a minute and is invisible until someone counts the
// sessions. Three abandoned attempts inside the window is the signal that the
// task is not going to succeed by itself; the card is left alone and the chat
// is told once, so a human decides what to do with it.
//
// An attempt that opened a PR does not count: that session did its job, and the
// usual reopen-the-issue flow must keep working.
func (o *Observer) quarantineFailedIssues(ctx context.Context, sessions []domain.SessionRecord, now time.Time) {
	attempts := map[domain.IssueID]int{}
	titles := map[domain.IssueID]string{}
	for _, sess := range sessions {
		if sess.IssueID == "" || !sess.IsTerminated {
			continue
		}
		if now.Sub(sess.CreatedAt.UTC()) > quarantineWindow {
			continue
		}
		prs, err := o.store.ListPRsBySession(ctx, sess.ID)
		if err != nil {
			o.logger.Warn("tracker intake: read session PRs failed", "session", sess.ID, "err", err)
			continue
		}
		if len(prs) > 0 {
			continue
		}
		attempts[sess.IssueID]++
		titles[sess.IssueID] = string(sess.ID)
	}
	for issueID, count := range attempts {
		if count < quarantineAttempts || o.quarantined[issueID] {
			continue
		}
		o.quarantined[issueID] = true
		o.logger.Warn("tracker intake: issue quarantined after repeated failed attempts",
			"issue", issueID, "attempts", count, "window", quarantineWindow, "last", titles[issueID])
		if o.announcer != nil {
			o.announcer.Announce(fmt.Sprintf("⛔️ %s снята с конвейера: %d сессии подряд закончились без PR. Карточку разбирает человек — конвейер её больше не берёт.", issueID, count))
		}
	}
	// A card that stopped failing (its sessions now open PRs, or the old
	// attempts aged out of the window) becomes claimable again on its own.
	for issueID := range o.quarantined {
		if attempts[issueID] < quarantineAttempts {
			delete(o.quarantined, issueID)
		}
	}
}

func seenIssueIDs(sessions []domain.SessionRecord) map[domain.IssueID]bool {
	seen := make(map[domain.IssueID]bool, len(sessions))
	for _, sess := range sessions {
		if sess.IssueID != "" && !sess.IsTerminated {
			seen[sess.IssueID] = true
		}
	}
	return seen
}

// CanonicalIssueID stores tracker issue ids in sessions.issue_id with the
// provider included, so future providers cannot collide on native ids.
func CanonicalIssueID(id domain.TrackerID) domain.IssueID {
	provider := id.Provider
	if provider == "" {
		provider = domain.TrackerProviderGitHub
	}
	native := strings.TrimSpace(id.Native)
	if native == "" {
		return ""
	}
	return domain.IssueID(string(provider) + ":" + native)
}

// BuildIssuePrompt turns normalized issue facts into the worker's initial task.
func BuildIssuePrompt(issue domain.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Work on tracker issue %s.\n\n", CanonicalIssueID(issue.ID))
	if issue.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", issue.Title)
	}
	if issue.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", issue.URL)
	}
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(issue.Labels, ", "))
	}
	if len(issue.Assignees) > 0 {
		fmt.Fprintf(&b, "Assignees: %s\n", strings.Join(issue.Assignees, ", "))
	}
	body := strings.TrimSpace(issue.Body)
	if body != "" {
		fmt.Fprintf(&b, "\nBody:\n%s\n", body)
	}
	b.WriteString(intakePromptFooter)
	return capIntakePrompt(b.String())
}

func capIntakePrompt(prompt string) string {
	if len(prompt) <= maxIntakePromptLen {
		return prompt
	}
	prefix := strings.TrimSuffix(prompt, intakePromptFooter)
	prefixBudget := maxIntakePromptLen - len(intakePromptTruncationNotice) - len(intakePromptFooter)
	if prefixBudget <= 0 {
		return truncateUTF8(prompt, maxIntakePromptLen)
	}
	return truncateUTF8(prefix, prefixBudget) + intakePromptTruncationNotice + intakePromptFooter
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		cut = i
	}
	return s[:cut]
}

func trackerRepo(project domain.ProjectRecord, cfg domain.TrackerIntakeConfig) (domain.TrackerRepo, bool) {
	provider := cfg.Provider
	if provider == "" {
		provider = domain.TrackerProviderGitHub
	}
	// Both GitHub-backed providers scope by repository. The board provider uses
	// it to pick its own cards off a board that may span several repositories,
	// so the repo is a filter there rather than the query target.
	if provider != domain.TrackerProviderGitHub && provider != domain.TrackerProviderGitHubProjects {
		return domain.TrackerRepo{}, false
	}
	native := strings.TrimSpace(cfg.Repo)
	if native == "" {
		native = parseGitHubRepoNative(project.RepoOriginURL)
	}
	if native == "" {
		return domain.TrackerRepo{}, false
	}
	return domain.TrackerRepo{Provider: provider, Native: native}, true
}

func parseGitHubRepoNative(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if strings.HasPrefix(remote, "git@") {
		if _, rest, ok := strings.Cut(remote, ":"); ok {
			return cleanRepoPath(rest)
		}
	}
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
		if host == "github.com" || strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".ghe.io") {
			return cleanRepoPath(u.Path)
		}
		return ""
	}
	return cleanRepoPath(remote)
}

func cleanRepoPath(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	owner := strings.TrimSpace(parts[len(parts)-2])
	repo := strings.TrimSpace(parts[len(parts)-1])
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}
