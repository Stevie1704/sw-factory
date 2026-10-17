package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// Session-role failure kinds recorded on an ended invocation. They reuse the
// harness failure vocabulary and add the boundaries only a session has.
const (
	// SessionFailureAuthenticationExpired means the harness credential was
	// refused. An operator refreshes it; the user then resubmits the turn.
	SessionFailureAuthenticationExpired = "authentication_expired"
	// SessionFailureRateLimited means the harness reported a capacity limit.
	SessionFailureRateLimited = "rate_limited"
	// SessionFailureUnexpectedExit means the process ended without a report.
	SessionFailureUnexpectedExit = "unexpected_exit"
	// SessionFailureNativeSessionMismatch means a resume revealed a native
	// session other than the recorded one.
	SessionFailureNativeSessionMismatch = "native_session_mismatch"
	// SessionFailureCredentials means the coordinator could not project the
	// harness credential into the worker.
	SessionFailureCredentials = "credentials_unavailable"
	// SessionFailureLaunch means the worker or harness could not start.
	SessionFailureLaunch = "launch_failed"
	// SessionFailureLaunchInterrupted means a coordinator stopped during the
	// launch and no process exists.
	SessionFailureLaunchInterrupted = "launch_interrupted"
	// SessionFailureReportRejected means the role's report failed validation.
	SessionFailureReportRejected = "report_rejected"
	// SessionFailurePlanRejected means plan validation refused the draft.
	SessionFailurePlanRejected = "plan_rejected"
	// SessionFailureCancelled means the coordinator cancelled the role.
	SessionFailureCancelled = "cancelled"
	// SessionFailureSessionMoved means the session left the pinned revision
	// while the role was in flight.
	SessionFailureSessionMoved = "session_moved"
	// SessionFailureNativeSessionLost means a resume of the recorded native
	// session ended before the harness revealed it, for example after its
	// role volume was removed. The next launch of the role starts a fresh
	// native session with the full packet.
	SessionFailureNativeSessionLost = "native_session_lost"
)

// sessionPacketVersion is the schema version of the frozen session packet.
const sessionPacketVersion = 1

// sessionPacketFileName is the packet file name below the packet directory.
const sessionPacketFileName = "session.json"

// maxSessionPacketTurnBytes bounds the conversation copied into one packet.
// The newest turns are kept; the packet counts the omitted older turns.
const maxSessionPacketTurnBytes = 512 << 10

// ErrSessionRoleNotStartable reports a session whose state, turns, or brief do
// not allow the requested role.
var ErrSessionRoleNotStartable = errors.New("session role cannot start")

// ErrSessionRoleNotActive reports a cancellation for a session with no active
// role invocation.
var ErrSessionRoleNotActive = errors.New("session has no active role invocation")

// SessionRoleStore is the operational-store seam of session-role invocations.
type SessionRoleStore interface {
	IdeationSession(context.Context, string) (store.IdeationSession, error)
	IdeationHistory(context.Context, string) (store.IdeationHistory, error)
	StartSessionInvocation(context.Context, store.IdeationWrite, store.SessionInvocation) (store.IdeationSession, store.SessionInvocation, error)
	RecordSessionInvocationNativeSession(context.Context, string, string) (store.SessionInvocation, error)
	SessionInvocation(context.Context, string) (store.SessionInvocation, error)
	ActiveSessionInvocations(context.Context) ([]store.SessionInvocation, error)
	UnreleasedSessionInvocations(context.Context) ([]store.SessionInvocation, error)
	MarkSessionInvocationResourcesReleased(context.Context, string, time.Time) (store.SessionInvocation, error)
	SessionInvocations(context.Context, string) ([]store.SessionInvocation, error)
	AcceptSessionRoleResult(context.Context, store.IdeationWrite, store.SessionRoleResult) (store.IdeationSession, store.SessionInvocation, error)
	ReleaseSessionInvocation(context.Context, store.IdeationWrite, store.SessionInvocationEnd) (store.IdeationSession, store.SessionInvocation, error)
	CloseSessionInvocation(context.Context, store.SessionInvocationEnd) (store.SessionInvocation, error)
}

var _ SessionRoleStore = (*store.Store)(nil)

// SessionRoleDependencies are the adapters a session-role coordinator uses. It
// has no tracker or code-host dependency: session roles have no publication
// authority.
type SessionRoleDependencies struct {
	// Store persists sessions and invocations.
	Store SessionRoleStore
	// Harnesses are the headless adapters keyed by harness.
	Harnesses map[config.Harness]harness.HeadlessRuntime
	// Workers starts and stops session-role workers.
	Workers worker.WorkerRuntime
	// Snapshots creates the read-only repository context.
	Snapshots gitadapter.ContextSnapshotter
	// Registration identifies the repository and its host credential sources.
	Registration config.RepositoryRegistration
	// Policy is the repository policy used to pin a new invocation. A running
	// invocation keeps its pinned values.
	Policy config.RepositoryConfig
	// Now supplies the coordinator time.
	Now Clock
	// NewInvocationID supplies a new invocation identity.
	NewInvocationID func() (string, error)
	// ValidatePlan optionally checks a draft plan graph before acceptance.
	// Plan validation owns graph policy; nil accepts what the store accepts.
	ValidatePlan func([]store.PlanNode) error
}

// SessionRoles launches, resumes, cancels, recovers, and accepts the
// ideation and planning roles of ideation sessions through HeadlessRuntime.
// The coordinator calls it from its single supervisor loop; it is the only
// component that applies a session-role result.
type SessionRoles struct {
	deps      SessionRoleDependencies
	lifecycle workflow.SessionLifecycle
}

// NewSessionRoles returns a session-role coordinator over deps.
func NewSessionRoles(deps SessionRoleDependencies) *SessionRoles {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &SessionRoles{deps: deps, lifecycle: workflow.DefaultSessionLifecycle()}
}

// sessionLaunch is the role-specific input of one launch.
type sessionLaunch struct {
	definition    workflow.SessionRoleDefinition
	history       store.IdeationHistory
	triggerID     string
	briefRevision int64
	brief         *store.Brief
	turns         []store.IdeationTurn
	resume        *store.SessionInvocation
}

// StartIdeationTurn launches the ideation role for turnID, which must be the
// newest turn of the session and a user turn. A later turn continues the
// native session of the last accepted ideation invocation.
func (r *SessionRoles) StartIdeationTurn(ctx context.Context, sessionID, turnID string) (store.SessionInvocation, error) {
	definition, _ := workflow.SessionRole(workflow.RoleIdeation)
	history, err := r.startableHistory(ctx, definition, sessionID)
	if err != nil {
		return store.SessionInvocation{}, err
	}
	turns := history.Turns
	if len(turns) == 0 || turns[len(turns)-1].ID != turnID || turns[len(turns)-1].Author != store.IdeationTurnAuthorUser {
		return store.SessionInvocation{}, fmt.Errorf("%w: turn %q is not the newest user turn of session %q", ErrSessionRoleNotStartable, turnID, sessionID)
	}
	launch := sessionLaunch{definition: definition, history: history, triggerID: turnID, turns: turns, briefRevision: history.Session.BriefRevision}
	if len(history.Briefs) != 0 {
		brief := history.Briefs[len(history.Briefs)-1].Brief
		launch.brief = &brief
	}
	if launch.resume, err = r.resumableInvocation(ctx, sessionID, definition.Name); err != nil {
		return store.SessionInvocation{}, err
	}
	return r.launch(ctx, launch)
}

// resumableInvocation returns the newest accepted invocation of role whose
// native session a new turn continues, or nil. A newer invocation of role
// that lost its native session ends the search, so the next turn starts a
// fresh native session instead of resuming the lost one again.
func (r *SessionRoles) resumableInvocation(ctx context.Context, sessionID, role string) (*store.SessionInvocation, error) {
	invocations, err := r.deps.Store.SessionInvocations(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for index := len(invocations) - 1; index >= 0; index-- {
		invocation := invocations[index]
		switch {
		case invocation.Role != role:
		case invocation.Status == store.SessionInvocationFailed && invocation.FailureKind == SessionFailureNativeSessionLost:
			return nil, nil
		case invocation.Status == store.SessionInvocationAccepted && invocation.NativeSessionID != "":
			return &invocation, nil
		}
	}
	return nil, nil
}

// StartPlanning launches the planning role for the current brief revision.
// The role receives the frozen brief, not the conversation. requestID is the
// handoff or regeneration request the result answers.
func (r *SessionRoles) StartPlanning(ctx context.Context, sessionID, requestID string) (store.SessionInvocation, error) {
	definition, _ := workflow.SessionRole(workflow.RolePlanning)
	history, err := r.startableHistory(ctx, definition, sessionID)
	if err != nil {
		return store.SessionInvocation{}, err
	}
	if len(history.Briefs) == 0 {
		return store.SessionInvocation{}, fmt.Errorf("%w: session %q has no brief to plan from", ErrSessionRoleNotStartable, sessionID)
	}
	brief := history.Briefs[len(history.Briefs)-1].Brief
	return r.launch(ctx, sessionLaunch{definition: definition, history: history, triggerID: requestID, briefRevision: history.Session.BriefRevision, brief: &brief})
}

// startableHistory reads the session history and checks that definition may
// start from the current session state.
func (r *SessionRoles) startableHistory(ctx context.Context, definition workflow.SessionRoleDefinition, sessionID string) (store.IdeationHistory, error) {
	history, err := r.deps.Store.IdeationHistory(ctx, sessionID)
	if err != nil {
		return store.IdeationHistory{}, err
	}
	if !definition.CanStartFrom(history.Session.State) {
		return store.IdeationHistory{}, fmt.Errorf("%w: %s cannot start while session %q is %s", ErrSessionRoleNotStartable, definition.Name, sessionID, history.Session.State)
	}
	return history, nil
}

// launch pins and persists one invocation, then starts its worker and native
// session. A failure after the invocation is persisted releases the session
// to the user with the typed cause.
func (r *SessionRoles) launch(ctx context.Context, launch sessionLaunch) (store.SessionInvocation, error) {
	role := launch.definition.Name
	policy, err := resolveAgentPolicy(r.deps.Policy, AgentRequest{Role: role})
	if err != nil {
		return store.SessionInvocation{}, err
	}
	adapter := r.deps.Harnesses[policy.Harness]
	if adapter == nil {
		return store.SessionInvocation{}, fmt.Errorf("no headless adapter for harness %q", policy.Harness)
	}
	if r.deps.Workers == nil || r.deps.Snapshots == nil || r.deps.NewInvocationID == nil {
		return store.SessionInvocation{}, errors.New("session roles need a worker runtime, a snapshot adapter, and an invocation identity source")
	}
	seed, credentialStoreID, err := harnessCredentialSeeding(r.deps.Workers, r.deps.Registration, policy.Harness, "")
	if err != nil {
		return store.SessionInvocation{}, err
	}
	if launch.resume != nil && launch.resume.Harness != string(policy.Harness) {
		// A native session belongs to the harness that created it (ADR 0002),
		// so a changed harness starts a fresh session with the full packet.
		launch.resume = nil
	}
	id, err := r.deps.NewInvocationID()
	if err != nil {
		return store.SessionInvocation{}, fmt.Errorf("create session invocation id: %w", err)
	}
	session := launch.history.Session
	snapshot, err := r.deps.Snapshots.CreateContextSnapshot(ctx, r.deps.Registration.Path, r.deps.Policy.TargetBranch, id)
	if err != nil {
		return store.SessionInvocation{}, fmt.Errorf("create repository context snapshot: %w", err)
	}
	invocation := store.SessionInvocation{
		ID: id, Role: role, TriggerID: launch.triggerID, BriefRevision: launch.briefRevision,
		ContextRevision: snapshot.Revision, Harness: string(policy.Harness), Model: policy.Model,
		ReasoningEffort: policy.ReasoningEffort, Image: r.deps.Policy.WorkerBuild.Image,
		ImageDigest: r.deps.Policy.WorkerBuild.Digest, CredentialStoreID: credentialStoreID,
		WorkerID: sessionWorkerID(session.ID, role), SnapshotPath: snapshot.Worktree,
	}
	resumeSessionID := ""
	if launch.resume != nil {
		// The native session is recorded only after the harness revealed it,
		// so a restart during the launch verifies the resume.
		invocation.ResumedFrom = launch.resume.ID
		resumeSessionID = launch.resume.NativeSessionID
	}
	root := filepath.Join(filepath.Dir(snapshot.Worktree), ".factory-sessions", session.ID, id)
	invocation.InvocationDirectory = filepath.Join(root, "packet")
	invocation.ResultDirectory = filepath.Join(root, "results")
	pinnedRevision := session.Revision + 1
	promptText, identity, err := prompt.BuildSession(prompt.SessionRequest{
		InvocationID: id, SessionID: session.ID, SessionRevision: pinnedRevision, Role: role,
		ContextRevision: snapshot.Revision, Continuation: launch.resume != nil,
	})
	if err == nil {
		invocation.PromptVersion, invocation.PromptSHA256 = identity.Version, identity.SHA256
		err = writeSessionPacket(invocation, launch, pinnedRevision)
	}
	if err != nil {
		_ = r.deps.Snapshots.RemoveContextSnapshot(context.WithoutCancel(ctx), r.deps.Registration.Path, snapshot)
		return store.SessionInvocation{}, err
	}
	write, err := r.lifecycle.Write(session, workflow.SessionEventRoleStarted, "", r.deps.Now().UTC())
	if err == nil {
		_, invocation, err = r.deps.Store.StartSessionInvocation(ctx, write, invocation)
	}
	if err != nil {
		_ = r.deps.Snapshots.RemoveContextSnapshot(context.WithoutCancel(ctx), r.deps.Registration.Path, snapshot)
		return store.SessionInvocation{}, err
	}
	nativeSessionID, err := r.startProcess(ctx, invocation, adapter, promptText, seed, resumeSessionID)
	if err != nil {
		classified := harness.ClassifyError(err, invocation.Harness)
		ended, releaseErr := r.release(ctx, invocation, store.SessionInvocationFailed, invocationFailureKind(invocation, err), fmt.Sprintf("%s role could not start: %v", role, classified))
		return ended, errors.Join(classified, releaseErr)
	}
	return r.deps.Store.RecordSessionInvocationNativeSession(ctx, invocation.ID, nativeSessionID)
}

// startProcess starts the read-only worker, projects the harness credential,
// and starts or resumes the native session. Each invocation pins the image
// again, so the session role worker is recreated from a changed image; its
// role volume, which holds the native session, stays. A non-empty
// resumeSessionID resumes that native session.
func (r *SessionRoles) startProcess(ctx context.Context, invocation store.SessionInvocation, adapter harness.HeadlessRuntime, promptText string, seed func(context.Context, string, string) error, resumeSessionID string) (string, error) {
	owner := sessionWorkerOwner(invocation.SessionID)
	gitMetadata, err := prepareGitMetadataProjection(invocation.ID, r.deps.Registration.Path, invocation.SnapshotPath)
	if err != nil {
		return "", fmt.Errorf("prepare snapshot Git metadata: %w", err)
	}
	if err := r.deps.Workers.Start(ctx, worker.StartRequest{
		RunID: owner, WorkerID: invocation.WorkerID, WorktreeReadOnly: true, WorktreePath: invocation.SnapshotPath,
		GitMetadataPath: gitMetadata, Image: invocation.Image, ImageDigest: invocation.ImageDigest,
		InvocationPath: invocation.InvocationDirectory, ResultPath: invocation.ResultDirectory,
		CredentialStoreID: invocation.CredentialStoreID, Role: invocation.Role,
		Limits: config.EffectiveWorkerLimits(r.deps.Registration.WorkerLimits), ReplaceImage: true,
	}); err != nil {
		return "", fmt.Errorf("start session worker: %w", err)
	}
	if seed != nil {
		if err := seed(ctx, owner, invocation.WorkerID); err != nil {
			return "", newCredentialProjectionError(invocation.Harness, err)
		}
	}
	request := harness.HeadlessStartRequest{
		InvocationID: invocation.ID, RunID: owner, WorkerID: invocation.WorkerID,
		SessionID: invocation.SessionID, SessionRevision: invocation.SessionRevision,
		Role: invocation.Role, Stage: invocation.Role, Prompt: promptText,
		Model: invocation.Model, ReasoningEffort: invocation.ReasoningEffort,
	}
	var session harness.HeadlessSession
	if resumeSessionID != "" {
		request.ResumeSessionID = resumeSessionID
		session, err = adapter.ResumeHeadless(ctx, request)
	} else {
		session, err = adapter.StartHeadless(ctx, request)
	}
	if err != nil {
		return "", err
	}
	return session.NativeSessionID, nil
}

// CancelRole cancels the active role invocation of a session and returns the
// session to the user with reason. Its report, if one arrives later, is
// ignored.
func (r *SessionRoles) CancelRole(ctx context.Context, sessionID, reason string) error {
	active, err := r.deps.Store.ActiveSessionInvocations(ctx)
	if err != nil {
		return err
	}
	for _, invocation := range active {
		if invocation.SessionID == sessionID {
			_, err := r.release(ctx, invocation, store.SessionInvocationCancelled, SessionFailureCancelled, reason)
			return err
		}
	}
	return fmt.Errorf("%w: %q", ErrSessionRoleNotActive, sessionID)
}

// Reconcile first retries the outstanding resource cleanup of ended
// invocations, then observes every active session invocation once. It
// accepts a written report, records a native session revealed after an
// interrupted launch, releases a failed process with its typed cause, and
// cancels an invocation whose session moved on. It needs no browser and no
// attached terminal, so it also recovers the invocations of a stopped
// coordinator.
func (r *SessionRoles) Reconcile(ctx context.Context) error {
	active, err := r.deps.Store.ActiveSessionInvocations(ctx)
	if err != nil {
		return err
	}
	failures := []error{r.retryResourceRelease(ctx, active)}
	for _, invocation := range active {
		if err := r.reconcileInvocation(ctx, invocation); err != nil {
			failures = append(failures, fmt.Errorf("reconcile session invocation %q: %w", invocation.ID, err))
		}
	}
	return errors.Join(failures...)
}

// retryResourceRelease repeats the cleanup of every ended invocation whose
// resources are not yet released. A worker that an active invocation of the
// same session role now uses is left running; only the ended invocation's
// snapshot and Git projection are removed.
func (r *SessionRoles) retryResourceRelease(ctx context.Context, active []store.SessionInvocation) error {
	unreleased, err := r.deps.Store.UnreleasedSessionInvocations(ctx)
	if err != nil {
		return err
	}
	inUse := map[string]bool{}
	for _, invocation := range active {
		inUse[invocation.WorkerID] = true
	}
	var failures []error
	for _, invocation := range unreleased {
		if err := r.releaseResources(ctx, invocation, !inUse[invocation.WorkerID]); err != nil {
			failures = append(failures, fmt.Errorf("release resources of session invocation %q: %w", invocation.ID, err))
		}
	}
	return errors.Join(failures...)
}

// reconcileInvocation applies one observation to one active invocation.
func (r *SessionRoles) reconcileInvocation(ctx context.Context, invocation store.SessionInvocation) error {
	session, err := r.deps.Store.IdeationSession(ctx, invocation.SessionID)
	if err != nil {
		return err
	}
	if session.State != store.IdeationStateRoleActive || session.Revision != invocation.SessionRevision {
		ended, err := r.deps.Store.CloseSessionInvocation(ctx, store.SessionInvocationEnd{
			InvocationID: invocation.ID, Status: store.SessionInvocationCancelled, FailureKind: SessionFailureSessionMoved,
			Reason: fmt.Sprintf("session moved to %s at revision %d", session.State, session.Revision), At: r.deps.Now().UTC(),
		})
		if err != nil {
			return err
		}
		return r.releaseResources(ctx, ended, true)
	}
	if reportWritten(invocation) {
		return r.acceptReport(ctx, session, invocation)
	}
	adapter := r.deps.Harnesses[config.Harness(invocation.Harness)]
	if adapter == nil {
		return fmt.Errorf("no headless adapter for harness %q", invocation.Harness)
	}
	inspection, err := adapter.InspectHeadless(ctx, r.inspectionRequest(invocation))
	if err != nil {
		return err
	}
	switch inspection.Status {
	case worker.HeadlessStatusStarting, worker.HeadlessStatusRunning:
		if invocation.NativeSessionID == "" {
			return r.adoptNativeSession(ctx, adapter, invocation)
		}
		return nil
	case worker.HeadlessStatusMissing:
		if invocation.NativeSessionID == "" {
			_, err := r.release(ctx, invocation, store.SessionInvocationFailed, SessionFailureLaunchInterrupted, invocation.Role+" role launch was interrupted before the process started")
			return err
		}
	}
	if reportWritten(invocation) {
		return r.acceptReport(ctx, session, invocation)
	}
	cause := error(harness.NewUnexpectedExitError(invocation.Harness))
	if inspector, ok := adapter.(harness.HeadlessFailureInspector); ok {
		if failure := inspector.HeadlessFailureFor(ctx, r.inspectionRequest(invocation)); failure != nil {
			cause = failure
		}
	}
	classified := harness.ClassifyError(cause, invocation.Harness)
	_, err = r.release(ctx, invocation, store.SessionInvocationFailed, invocationFailureKind(invocation, cause), fmt.Sprintf("%s role stopped without a report: %v", invocation.Role, classified))
	return err
}

// adoptNativeSession records the native session of a process that a stopped
// coordinator launched but did not record. A resumed launch must reveal the
// native session it resumed; another one ends the invocation as a mismatch.
func (r *SessionRoles) adoptNativeSession(ctx context.Context, adapter harness.HeadlessRuntime, invocation store.SessionInvocation) error {
	inspector, ok := adapter.(harness.NativeSessionInspector)
	if !ok {
		return nil
	}
	nativeSessionID, err := inspector.NativeSessionID(ctx, harness.NativeSessionRequest{
		RunID: sessionWorkerOwner(invocation.SessionID), InvocationID: invocation.ID, WorkerID: invocation.WorkerID, Harness: invocation.Harness,
	})
	if err != nil || nativeSessionID == "" {
		// The process has not revealed its identity yet; observe it again.
		return nil
	}
	if invocation.ResumedFrom != "" {
		resumed, err := r.deps.Store.SessionInvocation(ctx, invocation.ResumedFrom)
		if err != nil {
			return err
		}
		if resumed.NativeSessionID != nativeSessionID {
			cause := harness.NewNativeSessionMismatchError(invocation.Harness)
			_, err := r.release(ctx, invocation, store.SessionInvocationFailed, SessionFailureNativeSessionMismatch,
				fmt.Sprintf("%s role resume revealed another native session: %v", invocation.Role, harness.ClassifyError(cause, invocation.Harness)))
			return err
		}
	}
	_, err = r.deps.Store.RecordSessionInvocationNativeSession(ctx, invocation.ID, nativeSessionID)
	return err
}

// acceptReport validates the written report and applies it through one store
// write pinned to the invocation's session revision. An invalid report or a
// refused plan ends the invocation as rejected.
func (r *SessionRoles) acceptReport(ctx context.Context, session store.IdeationSession, invocation store.SessionInvocation) error {
	definition, ok := workflow.SessionRole(invocation.Role)
	if !ok {
		return fmt.Errorf("session role %q is not declared", invocation.Role)
	}
	value, err := report.ReadEnvelope(filepath.Join(invocation.ResultDirectory, report.ReportFileName))
	if err == nil {
		err = report.Validate(value, report.ValidationContext{
			InvocationID: invocation.ID, SessionID: invocation.SessionID, SessionRevision: invocation.SessionRevision,
			Harness: invocation.Harness, Role: invocation.Role, Stage: definition.Stage, RoleKind: string(definition.Kind),
		})
	}
	if err != nil {
		return r.reject(ctx, invocation, SessionFailureReportRejected, fmt.Sprintf("%s report rejected: %v", invocation.Role, err))
	}
	result, event, reason, err := r.roleResult(invocation, session, value)
	if err != nil {
		return r.reject(ctx, invocation, SessionFailurePlanRejected, fmt.Sprintf("planning draft rejected: %v", err))
	}
	write, err := r.lifecycle.Write(session, event, reason, r.deps.Now().UTC())
	if err != nil {
		return err
	}
	_, accepted, err := r.deps.Store.AcceptSessionRoleResult(ctx, write, result)
	if err != nil {
		if errors.Is(err, store.ErrIdeationRevisionConflict) || errors.Is(err, store.ErrSessionInvocationNotActive) {
			return err
		}
		return r.reject(ctx, invocation, SessionFailureReportRejected, fmt.Sprintf("%s result refused: %v", invocation.Role, err))
	}
	return r.releaseResources(ctx, accepted, true)
}

// roleResult converts a validated report into the store result, the session
// event, and the state reason it applies.
func (r *SessionRoles) roleResult(invocation store.SessionInvocation, session store.IdeationSession, value report.Report) (store.SessionRoleResult, workflow.SessionEvent, string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return store.SessionRoleResult{}, "", "", fmt.Errorf("encode role result: %w", err)
	}
	result := store.SessionRoleResult{InvocationID: invocation.ID, Payload: payload}
	turn := func(body string) *store.IdeationTurn {
		return &store.IdeationTurn{ID: "role-" + invocation.ID, Author: store.IdeationTurnAuthorRole, Role: invocation.Role, InvocationID: invocation.ID, Body: boundedTurnBody(body)}
	}
	switch {
	case value.Outcome == report.OutcomeCompleted && value.Ideation != nil:
		result.Turn = turn(value.Ideation.Message)
		if value.Ideation.Brief != nil {
			brief := briefFromReport(*value.Ideation.Brief)
			result.Brief = &brief
		}
		return result, workflow.SessionEventRoleAwaitsUser, "", nil
	case value.Outcome == report.OutcomeCompleted && value.PlanDraft != nil:
		nodes := planNodesFromReport(value.PlanDraft.Nodes)
		if r.deps.ValidatePlan != nil {
			if err := r.deps.ValidatePlan(nodes); err != nil {
				return store.SessionRoleResult{}, "", "", err
			}
		}
		result.Turn = turn(value.Summary)
		result.Plan = &store.PlanRevision{SourceBriefRevision: invocation.BriefRevision, ContextRevision: invocation.ContextRevision, Nodes: nodes}
		return result, workflow.SessionEventPlanDrafted, "", nil
	case value.Outcome == report.OutcomeNeedsClarification:
		lines := []string{value.Summary}
		for _, question := range value.Questions {
			lines = append(lines, "- "+question.Prompt)
		}
		result.Turn = turn(strings.Join(lines, "\n"))
		return result, releaseEvent(invocation, session), invocation.Role + " role asked for clarification", nil
	default:
		lines := []string{value.Summary}
		for _, evidence := range value.Evidence {
			lines = append(lines, "- "+evidence.Kind+": "+evidence.Detail)
		}
		result.Turn = turn(strings.Join(lines, "\n"))
		return result, releaseEvent(invocation, session), invocation.Role + " role cannot proceed", nil
	}
}

// reject ends an invocation whose report or draft was refused and releases the
// session with the reason.
func (r *SessionRoles) reject(ctx context.Context, invocation store.SessionInvocation, kind, reason string) error {
	_, err := r.release(ctx, invocation, store.SessionInvocationRejected, kind, reason)
	return err
}

// release ends an active invocation without a result, returns the session to
// where the role started from, and removes the role's resources.
func (r *SessionRoles) release(ctx context.Context, invocation store.SessionInvocation, status store.SessionInvocationStatus, kind, reason string) (store.SessionInvocation, error) {
	reason = boundedReason(reason)
	end := store.SessionInvocationEnd{InvocationID: invocation.ID, Status: status, FailureKind: kind, Reason: reason, At: r.deps.Now().UTC()}
	session, err := r.deps.Store.IdeationSession(ctx, invocation.SessionID)
	if err != nil {
		return invocation, err
	}
	var ended store.SessionInvocation
	if session.State == store.IdeationStateRoleActive && session.Revision == invocation.SessionRevision {
		write, writeErr := r.lifecycle.Write(session, releaseEvent(invocation, session), reason, end.At)
		if writeErr != nil {
			return invocation, writeErr
		}
		_, ended, err = r.deps.Store.ReleaseSessionInvocation(ctx, write, end)
	} else {
		ended, err = r.deps.Store.CloseSessionInvocation(ctx, end)
	}
	if err != nil {
		return invocation, err
	}
	return ended, r.releaseResources(ctx, ended, true)
}

// releaseEvent returns the session event that ends a role without a new
// draft: a planning role keeps an earlier draft under review; every other
// role returns the session to the user.
func releaseEvent(invocation store.SessionInvocation, session store.IdeationSession) workflow.SessionEvent {
	if invocation.Role == workflow.RolePlanning && session.PlanRevision > 0 {
		return workflow.SessionEventDraftKept
	}
	return workflow.SessionEventRoleAwaitsUser
}

// releaseResources stops the process of an ended invocation, removes its
// resources, and records the release. The store marks the release only after
// the cleanup succeeded, so a failure or a coordinator stop leaves it for the
// next reconcile pass. ownsWorker is false when another active invocation
// uses the same worker; that worker and its process then stay.
func (r *SessionRoles) releaseResources(ctx context.Context, invocation store.SessionInvocation, ownsWorker bool) error {
	if ownsWorker {
		r.stopProcess(ctx, invocation, invocation.Status == store.SessionInvocationAccepted)
	}
	if err := r.cleanup(ctx, invocation, ownsWorker); err != nil {
		return err
	}
	_, err := r.deps.Store.MarkSessionInvocationResourcesReleased(ctx, invocation.ID, r.deps.Now().UTC())
	return err
}

// stopProcess finishes an accepted process or cancels any other one. Both
// operations are idempotent; a failure is left for the next observation.
func (r *SessionRoles) stopProcess(ctx context.Context, invocation store.SessionInvocation, accepted bool) {
	adapter := r.deps.Harnesses[config.Harness(invocation.Harness)]
	if adapter == nil {
		return
	}
	session := harness.HeadlessSession{
		InvocationID: invocation.ID, RunID: sessionWorkerOwner(invocation.SessionID), WorkerID: invocation.WorkerID,
		Role: invocation.Role, NativeSessionID: invocation.NativeSessionID,
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if accepted {
		_ = adapter.FinishHeadless(cleanupContext, session)
		return
	}
	_ = adapter.CancelHeadless(cleanupContext, session)
}

// cleanup stops the session-role worker when stopWorker is set and removes
// the snapshot and its Git metadata projection. The role volume stays, so a
// later turn can resume the native session.
func (r *SessionRoles) cleanup(ctx context.Context, invocation store.SessionInvocation, stopWorker bool) error {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var failures []error
	if stopWorker && r.deps.Workers != nil {
		if err := r.deps.Workers.Stop(cleanupContext, invocation.WorkerID); err != nil {
			failures = append(failures, fmt.Errorf("stop session worker: %w", err))
		}
	}
	if invocation.SnapshotPath != "" && r.deps.Snapshots != nil {
		if err := r.deps.Snapshots.RemoveContextSnapshot(cleanupContext, r.deps.Registration.Path, gitadapter.ContextSnapshot{Revision: invocation.ContextRevision, Worktree: invocation.SnapshotPath}); err != nil {
			failures = append(failures, err)
		}
		if err := os.RemoveAll(gitMetadataProjectionPath(invocation.ID, invocation.SnapshotPath)); err != nil {
			failures = append(failures, fmt.Errorf("remove snapshot Git metadata: %w", err))
		}
	}
	return errors.Join(failures...)
}

// inspectionRequest returns the process selector of one invocation.
func (r *SessionRoles) inspectionRequest(invocation store.SessionInvocation) harness.HeadlessInspectionRequest {
	return harness.HeadlessInspectionRequest{InvocationID: invocation.ID, RunID: sessionWorkerOwner(invocation.SessionID), WorkerID: invocation.WorkerID, Role: invocation.Role}
}

// reportWritten reports whether factory-report published a report for the
// invocation.
func reportWritten(invocation store.SessionInvocation) bool {
	if invocation.ResultDirectory == "" {
		return false
	}
	_, err := os.Lstat(filepath.Join(invocation.ResultDirectory, report.ReportFileName))
	return err == nil
}

// sessionFailureKind maps a launch or process error to the recorded failure
// kind. It reads only typed errors, never native output.
func sessionFailureKind(err error) string {
	var projection *credentialProjectionError
	switch {
	case harness.IsNativeSessionMismatch(err):
		return SessionFailureNativeSessionMismatch
	case harness.IsAuthenticationExpired(err):
		return SessionFailureAuthenticationExpired
	case harness.IsRateLimited(err):
		return SessionFailureRateLimited
	case harness.IsUnexpectedExit(err):
		return SessionFailureUnexpectedExit
	case errors.Is(err, context.Canceled):
		return SessionFailureCancelled
	case errors.As(err, &projection):
		return SessionFailureCredentials
	default:
		return SessionFailureLaunch
	}
}

// invocationFailureKind returns the failure kind of err for invocation. A
// resume that exited before the harness revealed a native session lost that
// session, so the next launch starts a fresh one.
func invocationFailureKind(invocation store.SessionInvocation, err error) string {
	kind := sessionFailureKind(err)
	if kind == SessionFailureUnexpectedExit && invocation.ResumedFrom != "" && invocation.NativeSessionID == "" {
		return SessionFailureNativeSessionLost
	}
	return kind
}

// sessionWorkerOwner returns the worker owner key of a session. Worker
// resources are named from it; it is not a run identity.
func sessionWorkerOwner(sessionID string) string {
	return "session-" + sessionID
}

// sessionWorkerID returns the worker of one session role. Its role volume
// keeps the native session between turns.
func sessionWorkerID(sessionID, role string) string {
	return sessionWorkerOwner(sessionID) + "-" + role
}

// boundedReason keeps a state reason inside the store bound at a rune
// boundary.
func boundedReason(reason string) string {
	return store.TruncateUTF8(reason, store.MaxIdeationReasonBytes)
}

// boundedTurnBody keeps a role turn inside the store bound at a rune boundary.
func boundedTurnBody(body string) string {
	if strings.TrimSpace(body) == "" {
		body = "(no message)"
	}
	return store.TruncateUTF8(body, store.MaxIdeationTurnBytes)
}

// sessionInvocationPacket is the frozen input of one session-role invocation,
// mounted read-only at prompt.WorkerSessionPacketPath.
type sessionInvocationPacket struct {
	SchemaVersion   int                 `json:"schema_version"`
	InvocationID    string              `json:"invocation_id"`
	SessionID       string              `json:"session_id"`
	SessionRevision int64               `json:"session_revision"`
	Role            string              `json:"role"`
	ContextRevision string              `json:"context_revision"`
	PromptVersion   string              `json:"prompt_version"`
	PromptSHA256    string              `json:"prompt_sha256"`
	Continuation    bool                `json:"continuation"`
	BriefRevision   int64               `json:"brief_revision,omitempty"`
	Brief           *report.Brief       `json:"brief,omitempty"`
	Turns           []sessionPacketTurn `json:"turns,omitempty"`
	OmittedTurns    int                 `json:"omitted_turns,omitempty"`
}

// sessionPacketTurn is one conversation turn in a session packet.
type sessionPacketTurn struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
	Author   string `json:"author"`
	Role     string `json:"role,omitempty"`
	Body     string `json:"body"`
	// Corrects names the earlier user turn whose answer this turn changes.
	Corrects string `json:"corrects,omitempty"`
}

// writeSessionPacket writes the frozen packet and creates the result
// directory of invocation.
func writeSessionPacket(invocation store.SessionInvocation, launch sessionLaunch, pinnedRevision int64) error {
	for _, directory := range []string{invocation.InvocationDirectory, invocation.ResultDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create session invocation directory: %w", err)
		}
	}
	packet := sessionInvocationPacket{
		SchemaVersion: sessionPacketVersion, InvocationID: invocation.ID, SessionID: launch.history.Session.ID,
		SessionRevision: pinnedRevision, Role: invocation.Role, ContextRevision: invocation.ContextRevision,
		PromptVersion: invocation.PromptVersion, PromptSHA256: invocation.PromptSHA256,
		Continuation: invocation.ResumedFrom != "", BriefRevision: launch.briefRevision,
	}
	if launch.brief != nil {
		brief := briefToReport(*launch.brief)
		packet.Brief = &brief
	}
	packet.Turns, packet.OmittedTurns = boundedPacketTurns(launch.turns)
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session packet: %w", err)
	}
	if err := os.WriteFile(filepath.Join(invocation.InvocationDirectory, sessionPacketFileName), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write session packet: %w", err)
	}
	return nil
}

// boundedPacketTurns keeps the newest turns within maxSessionPacketTurnBytes
// and returns how many older turns it left out.
func boundedPacketTurns(turns []store.IdeationTurn) ([]sessionPacketTurn, int) {
	size, first := 0, len(turns)
	for first > 0 && size+len(turns[first-1].Body) <= maxSessionPacketTurnBytes {
		first--
		size += len(turns[first].Body)
	}
	kept := make([]sessionPacketTurn, 0, len(turns)-first)
	for _, turn := range turns[first:] {
		kept = append(kept, sessionPacketTurn{
			ID: turn.ID, Sequence: turn.Sequence, Author: string(turn.Author), Role: turn.Role, Body: turn.Body, Corrects: turn.Corrects,
		})
	}
	return kept, first
}

// briefFromReport converts a reported brief into the stored brief.
func briefFromReport(value report.Brief) store.Brief {
	items := func(values []report.BriefItem) []store.BriefItem {
		converted := make([]store.BriefItem, 0, len(values))
		for _, item := range values {
			converted = append(converted, store.BriefItem{ID: item.ID, Text: item.Text})
		}
		return converted
	}
	decisions := make([]store.BriefDecision, 0, len(value.Decisions))
	for _, decision := range value.Decisions {
		decisions = append(decisions, store.BriefDecision{ID: decision.ID, Decision: decision.Decision, Rationale: decision.Rationale})
	}
	return store.Brief{
		Problem: value.Problem, Audience: value.Audience, SuccessCriteria: items(value.SuccessCriteria),
		Scope: items(value.Scope), NonGoals: items(value.NonGoals), Decisions: decisions,
		Assumptions: items(value.Assumptions), Evidence: items(value.Evidence), UnresolvedQuestions: items(value.UnresolvedQuestions),
	}
}

// briefToReport converts a stored brief into the packet representation.
func briefToReport(value store.Brief) report.Brief {
	items := func(values []store.BriefItem) []report.BriefItem {
		converted := make([]report.BriefItem, 0, len(values))
		for _, item := range values {
			converted = append(converted, report.BriefItem{ID: item.ID, Text: item.Text})
		}
		return converted
	}
	decisions := make([]report.BriefDecision, 0, len(value.Decisions))
	for _, decision := range value.Decisions {
		decisions = append(decisions, report.BriefDecision{ID: decision.ID, Decision: decision.Decision, Rationale: decision.Rationale})
	}
	return report.Brief{
		Problem: value.Problem, Audience: value.Audience, SuccessCriteria: items(value.SuccessCriteria),
		Scope: items(value.Scope), NonGoals: items(value.NonGoals), Decisions: decisions,
		Assumptions: items(value.Assumptions), Evidence: items(value.Evidence), UnresolvedQuestions: items(value.UnresolvedQuestions),
	}
}

// planNodesFromReport converts reported plan nodes into stored plan nodes.
func planNodesFromReport(values []report.PlanNode) []store.PlanNode {
	nodes := make([]store.PlanNode, 0, len(values))
	for _, node := range values {
		nodes = append(nodes, store.PlanNode{
			ID: node.ID, Kind: node.Kind, Title: node.Title, Body: node.Body,
			AcceptanceCriteria: append([]string(nil), node.AcceptanceCriteria...), ParentID: node.ParentID,
			DependsOn: append([]string(nil), node.DependsOn...), BriefRefs: append([]string(nil), node.BriefRefs...),
		})
	}
	return nodes
}
