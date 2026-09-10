package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// AcceptanceSnapshot is the complete read-only observation one agent report is
// admitted against: the durable run and invocation, the frozen specification
// packet, the report bytes, and the worktree the invocation produced. Gather
// builds it once, so no later phase reads a store, a file, or a repository.
type AcceptanceSnapshot struct {
	// Run is the durable run the invocation belongs to.
	Run store.Run
	// Invocation is the durable invocation whose report is being accepted.
	Invocation store.Invocation
	// Role is the declared definition of the invocation's role.
	Role workflow.RoleDefinition
	// Packet is the decoded frozen specification packet of the run.
	Packet SpecificationPacket
	// PacketError is retained by gather so admission can preserve the original
	// checkpoint-before-packet rejection order without decoding the packet again.
	PacketError error
	// Report is the structured proposal read from the invocation directory.
	Report report.Report
	// Worktree is the inspected state of the run worktree.
	Worktree gitadapter.WorktreeState
	// ObservedChanges are the changed paths attributed to this invocation. An
	// open objection cycle attributes only what the revision itself changed.
	ObservedChanges []string
	// ObservedProtectedTestPaths are the gathered content identities for the run's
	// protected test paths.
	ObservedProtectedTestPaths []store.ProtectedTestPath
	// ProtectedTestPathsError is a deferred read failure for a protected path.
	// Admission returns it at the same point as the former live filesystem read.
	ProtectedTestPathsError error
	// ObjectionBasePaths are the gathered content identities frozen when an
	// implementation report opens a test objection.
	ObjectionBasePaths []store.ProtectedTestPath
	// ObjectionBasePathsError is a deferred read failure for objection context.
	// Projection returns it without touching the filesystem.
	ObjectionBasePathsError error
	// AutomatedObjection is the read-only pilot decision gathered for an
	// implementation objection before admission begins.
	AutomatedObjection bool
	// ObjectionGateReason explains why the gathered pilot decision refused
	// automated objection handling.
	ObjectionGateReason string
}

// TestInvocation reports whether the independent test role owns this report.
func (snapshot AcceptanceSnapshot) TestInvocation() bool {
	return snapshot.Role.Kind == workflow.RoleKindTest
}

// ReviewInvocation reports whether an isolated reviewer owns this report.
func (snapshot AcceptanceSnapshot) ReviewInvocation() bool {
	return snapshot.Role.Kind == workflow.RoleKindReview
}

// TestRevisionActive reports whether this report answers an open
// implementation objection rather than opening the test stage.
func (snapshot AcceptanceSnapshot) TestRevisionActive() bool {
	return snapshot.TestInvocation() && snapshot.Run.TestObjection != nil
}

// AcceptanceOutcome identifies the exit admission selected for one report. It
// is resolved once, so projection, commitment, and dispatch read the same
// decision instead of recomputing the role and objection predicates.
type AcceptanceOutcome string

const (
	// AcceptanceOutcomeHandoff means the report completes its stage and the
	// declared workflow transition owns the next run projection.
	AcceptanceOutcomeHandoff AcceptanceOutcome = "handoff"
	// AcceptanceOutcomeTestStage means the independent test stage policy owns
	// the continuation of this report.
	AcceptanceOutcomeTestStage AcceptanceOutcome = "test-stage"
	// AcceptanceOutcomeReview means an isolated review round owns it.
	AcceptanceOutcomeReview AcceptanceOutcome = "review"
	// AcceptanceOutcomeTestObjection means an accepted implementation report
	// disputes a protected test and redirects the run to the test role.
	AcceptanceOutcomeTestObjection AcceptanceOutcome = "test-objection"
	// AcceptanceOutcomeUnverifiable means a test report failed schema
	// validation in the one shape the coordinator parks for a person instead
	// of refusing.
	AcceptanceOutcomeUnverifiable AcceptanceOutcome = "unverifiable"
)

// acceptanceEvaluationRecorder is the content-free evaluation projection one
// accepted report writes. The module receives it explicitly instead of
// discovering the coordinator's broader store capability itself.
type acceptanceEvaluationRecorder interface {
	EnsureEvaluationSummary(context.Context, store.Run) error
	RecordEvaluationExemption(context.Context, string, store.EvaluationExemption) error
	RecordEvaluationEscalation(context.Context, string, store.EvaluationEscalationCategory) error
	RecordEvaluationBlocker(context.Context, string, store.EvaluationBlockerCategory) error
	RecordEvaluationBudgetExhaustion(context.Context, string) error
	RecordEvaluationUsage(context.Context, string, string, store.EvaluationUsage) error
}

// acceptanceJournal is the durable-effect seam report acceptance owns. Result
// acceptance crosses a restart boundary, so both operations are journaled.
type acceptanceJournal interface {
	AcceptResult(context.Context, effectkernel.RunStore, effectkernel.InvocationStore, effectkernel.ResultAcceptance) (store.Invocation, store.Run, error)
	Replay(context.Context, effectkernel.RunStore, store.PendingEffect) (store.Run, error)
}

// acceptanceLifecycle is the invocation-lifecycle seam commitment needs to
// finish a native harness session and release its worker.
type acceptanceLifecycle interface {
	HarnessRuntime(socketPath, harnessName string) (harness.Runtime, error)
	StopWorker(ctx context.Context, workerID string) error
}

// reportAcceptanceHooks are the stage policies acceptance dispatches to once a
// report is committed. Each policy stays in the file that owns its stage; the
// module only decides which one a committed report belongs to.
type reportAcceptanceHooks struct {
	acceptTestStage           func(context.Context, config.RepositoryRegistration, RunStore, *store.Run, *store.Invocation, report.Report, gitadapter.WorktreeState) (AgentResult, error)
	acceptReview              func(context.Context, config.RepositoryRegistration, RunStore, *store.Run, *store.Invocation, report.Report) (AgentResult, error)
	finishObjection           func(context.Context, config.RepositoryRegistration, RunStore, *store.Run, *store.Invocation, report.Report) (AgentResult, error)
	pauseUnverifiableTest     func(context.Context, config.RepositoryRegistration, RunStore, *store.Run, *store.Invocation, report.Report) (AgentResult, error)
	pauseUnverifiableRevision func(context.Context, config.RepositoryRegistration, RunStore, *store.Run, *store.Invocation, report.Report) (AgentResult, error)
	publishClarification      func(context.Context, config.RepositoryRegistration, RunStore, store.Run) (store.Run, error)
	persistRun                func(context.Context, config.RepositoryRegistration, RunStore, store.Run, store.Run) error
}

// reportAcceptance owns the report-acceptance path. Its adapters are explicit
// constructor arguments, so the module never reaches the coordinator's
// dependency set.
type reportAcceptance struct {
	journal            acceptanceJournal
	lifecycle          acceptanceLifecycle
	worktree           gitadapter.WorktreeInspector
	clock              Clock
	evaluationRecorder acceptanceEvaluationRecorder
	objectionGate      func(context.Context, config.RepositoryRegistration, SpecificationPacket) (bool, string)
	hooks              reportAcceptanceHooks
}

// newReportAcceptance binds the module to one adapter set.
func newReportAcceptance(journal acceptanceJournal, lifecycle acceptanceLifecycle, worktree gitadapter.WorktreeInspector, clock Clock, evaluationRecorder acceptanceEvaluationRecorder, objectionGate func(context.Context, config.RepositoryRegistration, SpecificationPacket) (bool, string), hooks reportAcceptanceHooks) *reportAcceptance {
	return &reportAcceptance{
		journal:            journal,
		lifecycle:          lifecycle,
		worktree:           worktree,
		clock:              clock,
		evaluationRecorder: evaluationRecorder,
		objectionGate:      objectionGate,
		hooks:              hooks,
	}
}

// ReportAcceptanceRequest is one acceptance of one invocation report against
// an already opened operational store.
type ReportAcceptanceRequest struct {
	// Registration is the tracked repository of the run.
	Registration config.RepositoryRegistration
	// RunStore is the opened operational store, owned by the caller.
	RunStore RunStore
	// Run is the durable run, updated in place as phases commit.
	Run *store.Run
	// Request selects the invocation and repeats the path policy.
	Request AgentReportRequest
}

// Accept sequences the four phases of report acceptance. Repeated acceptance
// of an already terminal invocation is crash recovery, so it is resolved
// before admission rather than as a branch inside it.
func (a *reportAcceptance) Accept(ctx context.Context, request ReportAcceptanceRequest) (AgentResult, error) {
	invocationStore, ok := request.RunStore.(InvocationStore)
	if !ok {
		return AgentResult{}, errors.New("operational store does not support visible invocations")
	}
	invocation, roleDefinition, err := a.identify(ctx, invocationStore, request)
	if err != nil {
		return AgentResult{}, err
	}
	if invocation.Status != store.InvocationStatusActive {
		return a.recoverRepeatedAcceptance(ctx, invocationStore, request, invocation)
	}
	if err := admitAcceptanceIdentity(*request.Run, *invocation, roleDefinition, request.Request); err != nil {
		return AgentResult{}, err
	}
	snapshot, err := a.gather(ctx, request.Registration, *request.Run, *invocation, roleDefinition)
	if err != nil {
		return AgentResult{}, err
	}
	outcome, err := AdmitReport(snapshot)
	if err != nil {
		return AgentResult{}, err
	}
	if outcome == AcceptanceOutcomeUnverifiable {
		if snapshot.TestRevisionActive() {
			return a.hooks.pauseUnverifiableRevision(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report)
		}
		return a.hooks.pauseUnverifiableTest(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report)
	}
	projection := projectAcceptance(snapshot, outcome, a.clock().UTC())
	return a.commit(ctx, request, invocationStore, invocation, snapshot, outcome, projection)
}

// identify resolves the invocation the request names and the role definition
// that owns its stage. It is the one read the sequencer needs before it can
// tell a first acceptance from a repeated one.
func (a *reportAcceptance) identify(ctx context.Context, invocationStore InvocationStore, request ReportAcceptanceRequest) (*store.Invocation, workflow.RoleDefinition, error) {
	run := request.Run
	if run == nil {
		return nil, workflow.RoleDefinition{}, errors.New("no active run")
	}
	if request.Request.RunID != "" && request.Request.RunID != run.ID {
		return nil, workflow.RoleDefinition{}, fmt.Errorf("active run is %s, not %s", run.ID, request.Request.RunID)
	}
	if run.CheckRepairPendingAttempt != 0 {
		return nil, workflow.RoleDefinition{}, fmt.Errorf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)
	}
	invocation, err := invocationStore.Invocation(ctx, run.ID, request.Request.InvocationID)
	if err != nil {
		return nil, workflow.RoleDefinition{}, err
	}
	if invocation == nil {
		return nil, workflow.RoleDefinition{}, fmt.Errorf("invocation %q does not belong to run %q", request.Request.InvocationID, run.ID)
	}
	roleDefinition, roleDeclared := workflow.DefaultRegistry().Role(invocation.Role)
	if !roleDeclared || roleDefinition.Stage != invocation.Stage {
		return nil, workflow.RoleDefinition{}, fmt.Errorf("invocation role %q does not own stage %q", invocation.Role, invocation.Stage)
	}
	if invocation.AttachRequired {
		return nil, workflow.RoleDefinition{}, fmt.Errorf("invocation %q requires `factory attach` before report acceptance", invocation.ID)
	}
	return invocation, roleDefinition, nil
}

// recoverRepeatedAcceptance completes an acceptance whose durable effect
// survived a crash. A journaled store replays the pending effect, refreshes
// the invocation, and resumes the stage projection the crash interrupted. A
// store without the journal has no such proof and refuses the repeat.
func (a *reportAcceptance) recoverRepeatedAcceptance(ctx context.Context, invocationStore InvocationStore, request ReportAcceptanceRequest, invocation *store.Invocation) (AgentResult, error) {
	journal, journaled := request.RunStore.(PendingEffectStore)
	if !journaled {
		return AgentResult{}, fmt.Errorf("invocation %q is already %s", invocation.ID, invocation.Status)
	}
	pending, pendingErr := journal.PendingEffect(ctx, request.Run.ID)
	if pendingErr != nil {
		return AgentResult{}, fmt.Errorf("read pending effect for repeated report acceptance: %w", pendingErr)
	}
	// Capture the accepted report from the pending effect before replay clears it.
	var acceptedReport report.Report
	var haveAcceptedReport bool
	if pending != nil && pending.Kind == store.PendingEffectKindResultAcceptance {
		if extractedReport, extractErr := readAcceptedReportFromEffect(*pending); extractErr == nil {
			acceptedReport = extractedReport
			haveAcceptedReport = true
		}
	}
	if pending != nil {
		updatedRun, replayErr := a.journal.Replay(ctx, request.RunStore, *pending)
		if replayErr != nil {
			return AgentResult{}, fmt.Errorf("replay pending effect before repeated report acceptance: %w", replayErr)
		}
		*request.Run = updatedRun
	}
	refreshed, refreshErr := invocationStore.Invocation(ctx, request.Run.ID, request.Request.InvocationID)
	if refreshErr != nil {
		return AgentResult{}, fmt.Errorf("refresh repeated report invocation: %w", refreshErr)
	}
	if refreshed != nil {
		invocation = refreshed
	}
	// Prefer the immutable snapshot the effect carried; a legacy payload
	// without one falls back to the artifact retained on disk.
	value := acceptedReport
	var readErr error
	if !haveAcceptedReport {
		value, readErr = readAcceptedAgentReport(*invocation)
	}
	if !haveAcceptedReport && readErr != nil {
		return AgentResult{}, fmt.Errorf("invocation %q is already %s", invocation.ID, invocation.Status)
	}
	resumed, projected, projectionErr := a.resumeAcceptedStageProjection(ctx, request, invocation, value)
	if projected {
		return resumed, projectionErr
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// resumeAcceptedStageProjection completes the test- or review-specific run
// transition when result acceptance made the invocation terminal but a crash
// left the durable run active at the same stage. A run that already moved away
// from the invocation stage is the durable proof that no continuation remains.
func (a *reportAcceptance) resumeAcceptedStageProjection(ctx context.Context, request ReportAcceptanceRequest, invocation *store.Invocation, value report.Report) (AgentResult, bool, error) {
	run := request.Run
	reviewInvocation := roleIsKind(*invocation, workflow.RoleKindReview)
	if run == nil || (run.Status != store.StatusActive && !(reviewInvocation && run.Stage == store.StageReview && run.Status == store.StatusWaitingForHuman)) || (run.Stage != invocation.Stage && !(reviewInvocation && run.Stage == store.StageReview)) {
		return AgentResult{}, false, nil
	}
	if roleIsKind(*invocation, workflow.RoleKindTest) {
		if a.worktree == nil {
			return AgentResult{}, true, errors.New("worktree inspector is required to resume accepted test projection")
		}
		state, err := a.worktree.Inspect(ctx, run.Worktree)
		if err != nil {
			return AgentResult{}, true, fmt.Errorf("inspect worktree to resume accepted test projection: %w", err)
		}
		result, err := a.hooks.acceptTestStage(ctx, request.Registration, request.RunStore, run, invocation, value, state)
		return result, true, err
	}
	if reviewInvocation {
		result, err := a.hooks.acceptReview(ctx, request.Registration, request.RunStore, run, invocation, value)
		return result, true, err
	}
	return AgentResult{}, false, nil
}

// gather performs every read report acceptance needs: the report bytes, the
// worktree the invocation produced, and the frozen specification packet. It
// takes read-only seams and mutates nothing.
func (a *reportAcceptance) gather(ctx context.Context, registration config.RepositoryRegistration, run store.Run, invocation store.Invocation, roleDefinition workflow.RoleDefinition) (AcceptanceSnapshot, error) {
	snapshot := AcceptanceSnapshot{Run: run, Invocation: invocation, Role: roleDefinition}
	path := reportPath(invocation)
	var err error
	if snapshot.TestInvocation() {
		snapshot.Report, err = report.ReadEnvelope(path)
	} else {
		snapshot.Report, err = report.Read(path)
	}
	if err != nil {
		return AcceptanceSnapshot{}, err
	}
	if a.worktree == nil {
		return AcceptanceSnapshot{}, errors.New("worktree runtime does not support report inspection")
	}
	snapshot.Worktree, err = a.worktree.Inspect(ctx, run.Worktree)
	if err != nil {
		return AcceptanceSnapshot{}, fmt.Errorf("inspect worktree for agent report: %w", err)
	}
	snapshot.ObservedChanges = append([]string(nil), snapshot.Worktree.ChangedPaths...)
	if snapshot.TestRevisionActive() {
		snapshot.ObservedChanges, err = testRevisionChangedPaths(snapshot.Worktree, run.TestRevisionBaseChangedPaths)
		if err != nil {
			return AcceptanceSnapshot{}, fmt.Errorf("compute test revision worktree changes: %w", err)
		}
	}
	snapshot.Packet, err = decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		snapshot.PacketError = fmt.Errorf("decode specification packet for agent report: %w", err)
	}
	if snapshot.PacketError == nil && !snapshot.TestRevisionActive() {
		snapshot.ObservedProtectedTestPaths, snapshot.ProtectedTestPathsError = observeProtectedTestPaths(run.Worktree, run.ProtectedTestPaths)
	}
	if snapshot.PacketError == nil && implementationTestObjection(snapshot.Report, snapshot.Invocation) {
		if a.objectionGate != nil {
			snapshot.AutomatedObjection, snapshot.ObjectionGateReason = a.objectionGate(ctx, registration, snapshot.Packet)
		}
		snapshot.ObjectionBasePaths, snapshot.ObjectionBasePathsError = protectedTestPathsForCheckpoint(run.Worktree, snapshot.Worktree.ChangedPaths)
	}
	return snapshot, nil
}

// admitAcceptanceIdentity enforces the run-state, stage-ownership, and
// path-policy preconditions that hold before the report itself is read.
func admitAcceptanceIdentity(run store.Run, invocation store.Invocation, roleDefinition workflow.RoleDefinition, request AgentReportRequest) error {
	reviewInvocation := roleDefinition.Kind == workflow.RoleKindReview
	if err := validateAgentRunState(run); err != nil && !reviewCanBeAcceptedWhileWaiting(run, invocation) {
		return err
	}
	if run.Stage != invocation.Stage && !(reviewInvocation && run.Stage == store.StageReview) {
		return fmt.Errorf("invocation stage %q does not match active run stage %q", invocation.Stage, run.Stage)
	}
	if len(request.PermittedPaths) == 0 {
		return nil
	}
	if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
		return err
	}
	if !sameStrings(request.PermittedPaths, invocation.PermittedPaths) {
		return errors.New("agent report permitted paths do not match the invocation policy")
	}
	return nil
}

// AdmitReport decides whether one gathered report may be accepted and which
// exit it takes. It is pure over the snapshot: it takes no adapter and no
// context, so a rejection can be reproduced from a run, an invocation, a
// report, and a worktree state alone.
func AdmitReport(snapshot AcceptanceSnapshot) (AcceptanceOutcome, error) {
	if err := admitAcceptanceCheckpoint(snapshot); err != nil {
		return "", err
	}
	if snapshot.PacketError != nil {
		return "", snapshot.PacketError
	}
	if err := admitAcceptanceRouting(snapshot); err != nil {
		return "", err
	}
	if err := report.Validate(snapshot.Report, acceptanceValidationContext(snapshot)); err != nil {
		if isUnverifiableTestReport(snapshot.Report, snapshot.Invocation) {
			return AcceptanceOutcomeUnverifiable, nil
		}
		return "", err
	}
	objection, err := admitAcceptanceObjection(snapshot)
	if err != nil {
		return "", err
	}
	if err := admitAcceptancePathOwnership(snapshot); err != nil {
		return "", err
	}
	if snapshot.Report.Outcome == report.OutcomeNeedsClarification {
		if err := validateClarificationQuestions(snapshot.Packet, snapshot.Run.PendingQuestions, snapshot.Report.Questions); err != nil {
			return "", err
		}
	}
	switch {
	case objection:
		return AcceptanceOutcomeTestObjection, nil
	case snapshot.TestInvocation():
		return AcceptanceOutcomeTestStage, nil
	case snapshot.ReviewInvocation():
		return AcceptanceOutcomeReview, nil
	}
	return AcceptanceOutcomeHandoff, nil
}

// admitAcceptanceCheckpoint enforces that the observed worktree still holds
// the immutable checkpoint the invocation was launched against. A reviewer
// additionally owns no write to that checkpoint at all.
func admitAcceptanceCheckpoint(snapshot AcceptanceSnapshot) error {
	if snapshot.Run.CheckpointSHA != "" && snapshot.Worktree.HeadSHA != snapshot.Run.CheckpointSHA {
		return fmt.Errorf("agent report worktree HEAD %q does not match checkpoint %q", snapshot.Worktree.HeadSHA, snapshot.Run.CheckpointSHA)
	}
	if snapshot.ReviewInvocation() && len(snapshot.Worktree.ChangedPaths) != 0 {
		return fmt.Errorf("%s changed the immutable checkpoint worktree", snapshot.Invocation.Role)
	}
	return nil
}

// admitAcceptanceRouting enforces the frozen workflow route: a test report
// needs a declared independent test stage, neither an implementation signal
// nor a review-repair handoff may cross the role that owns it, and a review
// result must name the exact checkpoint its round was opened against.
func admitAcceptanceRouting(snapshot AcceptanceSnapshot) error {
	if snapshot.TestInvocation() && !independentTestStageDeclared(snapshot.Packet) {
		return errors.New("test-stage reports are unavailable in advisory mode without a selected route; implementation owns TDD")
	}
	if err := validateImplementationOwnedSignals(snapshot.Report, snapshot.Invocation, snapshot.Packet); err != nil {
		return err
	}
	if err := validateReviewRepairTestOwnerRouting(snapshot.Report, snapshot.Invocation, snapshot.Run, snapshot.Packet); err != nil {
		return err
	}
	if !snapshot.ReviewInvocation() || snapshot.Report.ReviewHandoff == nil || snapshot.Report.ReviewHandoff.ReviewedSHA == snapshot.Run.CheckpointSHA {
		return nil
	}
	return &ReviewCheckpointMismatchError{
		Role:         snapshot.Invocation.Role,
		InvocationID: snapshot.Invocation.ID,
		Expected:     snapshot.Run.CheckpointSHA,
		Observed:     snapshot.Report.ReviewHandoff.ReviewedSHA,
	}
}

// admitAcceptanceObjection reports whether an accepted implementation report
// opens a bounded test objection cycle, and refuses a response to an objection
// that is not open.
func admitAcceptanceObjection(snapshot AcceptanceSnapshot) (bool, error) {
	if snapshot.TestInvocation() {
		if err := validateTestStageReportPolicy(snapshot.Report, snapshot.Packet.RepositoryConfig.TestPolicy); err != nil {
			return false, err
		}
	}
	objection := implementationTestObjection(snapshot.Report, snapshot.Invocation)
	if snapshot.Report.TestObjectionResponse != nil && !snapshot.TestRevisionActive() {
		return false, errors.New("test objection response requires an active implementation objection")
	}
	if !objection {
		return false, nil
	}
	return true, validateImplementationTestObjection(snapshot.Report, snapshot.Invocation, snapshot.Run, snapshot.Packet)
}

// admitAcceptancePathOwnership enforces the path split between the test role
// and implementation. An open objection cycle owns test paths exclusively; an
// ordinary report must leave the protected test checkpoint untouched.
func admitAcceptancePathOwnership(snapshot AcceptanceSnapshot) error {
	testPolicy := snapshot.Packet.RepositoryConfig.TestPolicy
	if snapshot.TestRevisionActive() {
		if err := validateTestChangedPaths(snapshot.ObservedChanges, testPolicy); err != nil {
			return fmt.Errorf("test revision path ownership: %w", err)
		}
		return nil
	}
	if snapshot.Invocation.Role == workflow.RoleImplementation && independentTestStageDeclared(snapshot.Packet) && !snapshot.Run.TestStageSkipped {
		if err := validateImplementationTestPaths(snapshot.Worktree.ChangedPaths, testPolicy); err != nil {
			return err
		}
	}
	if snapshot.ProtectedTestPathsError != nil {
		return snapshot.ProtectedTestPathsError
	}
	return validateProtectedTestPathObservations(snapshot.Worktree, snapshot.Run.ProtectedTestPaths, snapshot.ObservedProtectedTestPaths)
}

// acceptanceValidationContext renders the report-schema validation inputs the
// coordinator owns for one gathered snapshot.
func acceptanceValidationContext(snapshot AcceptanceSnapshot) report.ValidationContext {
	invocation, run := snapshot.Invocation, snapshot.Run
	return report.ValidationContext{
		InvocationID:            invocation.ID,
		RunID:                   invocation.RunID,
		Harness:                 invocation.Harness,
		Role:                    invocation.Role,
		Stage:                   string(invocation.Stage),
		PromptVersion:           invocation.PromptVersion,
		RoleKind:                string(snapshot.Role.Kind),
		WorktreePath:            run.Worktree,
		PermittedPaths:          invocation.PermittedPaths,
		CheckpointSHA:           run.CheckpointSHA,
		ObservedChanges:         snapshot.ObservedChanges,
		WorktreeObserved:        true,
		RepositoryGuidancePaths: append([]string(nil), snapshot.Packet.RepositoryGuidancePaths...),
		RepositoryGuidanceBound: invocation.Role == workflow.RoleStandardsReview && invocation.PromptVersion != "standards-review-v1",
		TestPaths:               snapshot.Packet.RepositoryConfig.TestPolicy.TestPaths,
		TestInfrastructurePaths: snapshot.Packet.RepositoryConfig.TestPolicy.InfrastructurePaths,
	}
}

// recordAcceptanceEvaluation writes the content-free evaluation projection of
// one accepted report. A store without the projection records nothing.
func recordAcceptanceEvaluation(ctx context.Context, recorder acceptanceEvaluationRecorder, snapshot AcceptanceSnapshot, outcome AcceptanceOutcome) error {
	if recorder == nil {
		return nil
	}
	run, invocation, value := snapshot.Run, snapshot.Invocation, snapshot.Report
	if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
		return fmt.Errorf("ensure local evaluation summary: %w", err)
	}
	if value.BudgetExhausted {
		if err := recorder.RecordEvaluationBudgetExhaustion(ctx, run.ID); err != nil {
			return fmt.Errorf("record local evaluation budget exhaustion: %w", err)
		}
	}
	for _, exemption := range value.Exemptions {
		if err := recorder.RecordEvaluationExemption(ctx, run.ID, store.EvaluationExemption(exemption)); err != nil {
			return fmt.Errorf("record local evaluation exemption: %w", err)
		}
	}
	for _, escalation := range value.Escalations {
		if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationCategory(escalation)); err != nil {
			return fmt.Errorf("record local evaluation escalation: %w", err)
		}
	}
	for _, blocker := range value.Blockers {
		if err := recorder.RecordEvaluationBlocker(ctx, run.ID, store.EvaluationBlockerCategory(blocker)); err != nil {
			return fmt.Errorf("record local evaluation blocker: %w", err)
		}
	}
	if value.Usage != nil {
		if err := recorder.RecordEvaluationUsage(ctx, run.ID, invocation.ID, store.EvaluationUsage{
			Available:    value.Usage.Available,
			InputTokens:  value.Usage.InputTokens,
			OutputTokens: value.Usage.OutputTokens,
			TotalTokens:  value.Usage.TotalTokens,
			CostReported: value.Usage.CostReported,
			CostMicros:   value.Usage.CostMicros,
			Currency:     value.Usage.Currency,
		}); err != nil {
			return fmt.Errorf("record local evaluation usage: %w", err)
		}
	}
	if outcome == AcceptanceOutcomeTestObjection || (snapshot.TestRevisionActive() && value.TestObjectionResponse != nil) {
		if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationTestDispute); err != nil {
			return fmt.Errorf("record test objection escalation: %w", err)
		}
	}
	switch value.Outcome {
	case report.OutcomeNeedsClarification:
		if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationClarification); err != nil {
			return fmt.Errorf("record local evaluation clarification: %w", err)
		}
	case report.OutcomeCannotProceed:
		if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationBlocked); err != nil {
			return fmt.Errorf("record local evaluation blocker escalation: %w", err)
		}
	}
	return nil
}

// reconcileNativeSessionID resolves the one native session identity the
// accepted invocation keeps. A report may supply the identity a launch never
// observed, but it may never rename one the coordinator already persisted.
func reconcileNativeSessionID(invocation store.Invocation, value report.Report) (string, error) {
	nativeSessionID := invocation.NativeSessionID
	if invocation.NativeSessionID != "" {
		if value.NativeSessionID != "" && value.NativeSessionID != invocation.NativeSessionID {
			return "", errors.New("agent report native session identifier does not match persisted invocation")
		}
	} else {
		nativeSessionID = value.NativeSessionID
	}
	if nativeSessionID == "" {
		return "", errors.New("accepted agent report must retain a native session identifier")
	}
	return nativeSessionID, nil
}

// acceptanceProjection is the durable state one admitted report produces
// before any persistence strategy runs. It is pure over the snapshot.
type acceptanceProjection struct {
	// Invocation is the accepted invocation with its terminal status.
	Invocation store.Invocation
	// Previous is the run as it was persisted before this acceptance.
	Previous store.Run
	// Next is the complete run projection for direct persistence.
	Next store.Run
	// JournaledNext is the same logical projection with the revision and
	// timestamp required in a restart-safe result-acceptance effect.
	JournaledNext store.Run
	// Error is a pure projection refusal. Commitment returns it after the
	// evaluation and native-session checks that historically preceded projection.
	Error error
}

// projectAcceptance builds the accepted invocation and the next run from an
// admitted report. Both persistence strategies share it; only the durability
// mechanism, and the restart-safety edits that mechanism requires, differ.
func projectAcceptance(snapshot AcceptanceSnapshot, outcome AcceptanceOutcome, now time.Time) acceptanceProjection {
	accepted := snapshot.Invocation
	accepted.Status = acceptedInvocationStatus(snapshot.Report.Outcome)
	accepted.UpdatedAt = now
	previous := snapshot.Run
	projection := acceptanceProjection{Invocation: accepted, Previous: previous}
	next := previous
	releaseActiveInvocation(&next, snapshot.Invocation.ID)
	switch outcome {
	case AcceptanceOutcomeTestObjection:
		if snapshot.ObjectionBasePathsError != nil {
			projection.Error = fmt.Errorf("record test objection worktree context: %w", snapshot.ObjectionBasePathsError)
			return projection
		}
		projected, err := projectImplementationTestObjection(previous, snapshot.Report, snapshot.Invocation, snapshot.Packet, snapshot.ObjectionBasePaths, snapshot.AutomatedObjection, snapshot.ObjectionGateReason)
		if err != nil {
			projection.Error = err
			return projection
		}
		next = projected
		next.Revision = previous.Revision + 1
		next.UpdatedAt = now
	case AcceptanceOutcomeReview:
		if err := applyReviewResultProjection(&next, snapshot.Invocation.Role, snapshot.Report); err != nil {
			projection.Error = err
			return projection
		}
	case AcceptanceOutcomeTestStage:
		// The test-stage policy owns its run transition after commitment.
	default:
		next = agentReportRunProjection(previous, snapshot.Invocation.Stage, snapshot.Report)
		next.UpdatedAt = now
	}
	journaledNext := next
	journaledNext.Revision = previous.Revision + 1
	if outcome == AcceptanceOutcomeTestStage || outcome == AcceptanceOutcomeReview {
		journaledNext.Revision = previous.Revision
	}
	journaledNext.UpdatedAt = now
	projection.Next = next
	projection.JournaledNext = journaledNext
	return projection
}

// commit makes the projection durable and hands the run to the stage policy
// that owns it. The journaled and legacy stores share the projection and the
// dispatch; only the durability mechanism differs.
func (a *reportAcceptance) commit(ctx context.Context, request ReportAcceptanceRequest, invocationStore InvocationStore, invocation *store.Invocation, snapshot AcceptanceSnapshot, outcome AcceptanceOutcome, projection acceptanceProjection) (AgentResult, error) {
	if err := recordAcceptanceEvaluation(ctx, a.evaluationRecorder, snapshot, outcome); err != nil {
		return AgentResult{}, err
	}
	harnessRuntime, err := a.lifecycle.HarnessRuntime(request.Registration.Cmux.SocketPath, invocation.Harness)
	if err != nil {
		return AgentResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	nativeSessionID, err := reconcileNativeSessionID(*invocation, snapshot.Report)
	if err != nil {
		return AgentResult{}, err
	}
	projection.Invocation.NativeSessionID = nativeSessionID
	if projection.Error != nil {
		return AgentResult{}, projection.Error
	}
	if _, journaled := request.RunStore.(PendingEffectStore); journaled {
		err = a.commitJournaled(ctx, request, invocationStore, invocation, snapshot, harnessRuntime, outcome, projection)
	} else {
		err = a.commitDirect(ctx, request, invocationStore, invocation, snapshot, harnessRuntime, outcome, projection)
	}
	if err != nil {
		return AgentResult{}, err
	}
	return a.dispatch(ctx, request, invocation, snapshot, outcome)
}

// commitJournaled persists the acceptance as one durable effect. The effect
// payload has to survive a restart on its own, so the review result and the
// revision bump are applied before it is reserved rather than by the stage
// policy that runs after it.
func (a *reportAcceptance) commitJournaled(ctx context.Context, request ReportAcceptanceRequest, invocationStore InvocationStore, invocation *store.Invocation, snapshot AcceptanceSnapshot, harnessRuntime harness.Runtime, outcome AcceptanceOutcome, projection acceptanceProjection) error {
	next := projection.JournaledNext
	accepted, committed, err := a.journal.AcceptResult(ctx, request.RunStore, invocationStore, effectkernel.ResultAcceptance{
		Repository: commandRepository(request.Registration),
		SocketPath: request.Registration.Cmux.SocketPath,
		WorkerID:   workerIDForInvocation(projection.Invocation),
		Harness:    harnessRuntime,
		Session: harness.Session{
			InvocationID:    projection.Invocation.ID,
			RunID:           projection.Invocation.RunID,
			WorkerID:        workerIDForInvocation(projection.Invocation),
			NativeSessionID: projection.Invocation.NativeSessionID,
			Surface:         invocationSurface(projection.Invocation),
		},
		Invocation: projection.Invocation,
		Previous:   projection.Previous,
		Next:       next,
		StopWorker: snapshot.Report.Outcome == report.OutcomeNeedsClarification || outcome == AcceptanceOutcomeReview || outcome == AcceptanceOutcomeTestObjection,
		Report:     snapshot.Report,
	})
	if err != nil {
		return err
	}
	*invocation = accepted
	*request.Run = committed
	return nil
}

// commitDirect persists the acceptance against a legacy store that has no
// journal. The harness session is finished first, so a failure leaves the
// invocation active and the report re-acceptable.
func (a *reportAcceptance) commitDirect(ctx context.Context, request ReportAcceptanceRequest, invocationStore InvocationStore, invocation *store.Invocation, snapshot AcceptanceSnapshot, harnessRuntime harness.Runtime, outcome AcceptanceOutcome, projection acceptanceProjection) error {
	if err := harnessRuntime.Finish(ctx, harness.Session{InvocationID: invocation.ID, RunID: invocation.RunID, WorkerID: workerIDForInvocation(*invocation), NativeSessionID: projection.Invocation.NativeSessionID, Surface: invocationSurface(*invocation)}); err != nil {
		return fmt.Errorf("finish accepted harness session: %w", err)
	}
	if snapshot.Report.Outcome == report.OutcomeNeedsClarification || outcome == AcceptanceOutcomeReview {
		if err := a.lifecycle.StopWorker(ctx, workerIDForInvocation(*invocation)); err != nil {
			return err
		}
	}
	*invocation = projection.Invocation
	if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
		return fmt.Errorf("persist accepted invocation: %w", err)
	}
	next := projection.Next
	switch outcome {
	case AcceptanceOutcomeTestObjection:
		*request.Run = next
		if err := a.hooks.persistRun(ctx, request.Registration, request.RunStore, projection.Previous, next); err != nil {
			return fmt.Errorf("persist implementation test objection: %w", err)
		}
	case AcceptanceOutcomeTestStage, AcceptanceOutcomeReview:
		// The stage policy persists its own transition.
		*request.Run = next
	default:
		*request.Run = next
		if err := a.hooks.persistRun(ctx, request.Registration, request.RunStore, projection.Previous, next); err != nil {
			return fmt.Errorf("persist accepted agent state: %w", err)
		}
	}
	return nil
}

// dispatch hands the committed run to the stage policy that owns its
// continuation. Test, review, objection, and clarification remain separate
// policies in their own files.
func (a *reportAcceptance) dispatch(ctx context.Context, request ReportAcceptanceRequest, invocation *store.Invocation, snapshot AcceptanceSnapshot, outcome AcceptanceOutcome) (AgentResult, error) {
	switch outcome {
	case AcceptanceOutcomeTestObjection:
		return a.hooks.finishObjection(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report)
	case AcceptanceOutcomeTestStage:
		return a.hooks.acceptTestStage(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report, snapshot.Worktree)
	case AcceptanceOutcomeReview:
		return a.hooks.acceptReview(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report)
	}
	if snapshot.Report.Outcome == report.OutcomeNeedsClarification {
		published, err := a.hooks.publishClarification(ctx, request.Registration, request.RunStore, *request.Run)
		if err != nil {
			return AgentResult{}, err
		}
		*request.Run = published
	}
	return AgentResult{Invocation: *invocation, Report: snapshot.Report}, nil
}

// acceptanceEvaluationRecorderForRunStore narrows a coordinator-owned store to
// the content-free evaluation projection acceptance records into.
func acceptanceEvaluationRecorderForRunStore(runStore RunStore) acceptanceEvaluationRecorder {
	recorder, _ := runStore.(acceptanceEvaluationRecorder)
	return recorder
}

// reviewCanBeAcceptedWhileWaiting permits the serialized event loop to apply
// a second review result after the first reviewer has already put the round in
// a human-waiting state. Non-review work remains blocked by that state.
func reviewCanBeAcceptedWhileWaiting(run store.Run, invocation store.Invocation) bool {
	if run.Stage != store.StageReview || run.Status != store.StatusWaitingForHuman || !roleIsKind(invocation, workflow.RoleKindReview) {
		return false
	}
	if containsString(run.ActiveInvocationIDs, invocation.ID) {
		return true
	}
	return false
}

// acceptedInvocationStatus maps a validated report outcome to its durable
// invocation lifecycle state, keeping result acceptance consistent across all
// visible roles.
func acceptedInvocationStatus(outcome report.Outcome) store.InvocationStatus {
	switch outcome {
	case report.OutcomeCompleted:
		return store.InvocationStatusCompleted
	case report.OutcomeNeedsClarification:
		return store.InvocationStatusWaitingForHuman
	case report.OutcomeCannotProceed:
		return store.InvocationStatusCannotProceed
	default:
		return store.InvocationStatusCannotProceed
	}
}

// agentReportRunProjection applies the declared workflow transition for a
// validated generic handoff in both journaled and legacy paths. An unreadable
// frozen packet or an undeclared transition fails the run at its invocation
// stage instead of guessing a route that could skip a selected stage.
func agentReportRunProjection(previous store.Run, invocationStage store.Stage, value report.Report) store.Run {
	next := previous
	// An accepted report ends the run's delegation to its invocation, so the
	// status projection stops implying that a harness is executing.
	clearActiveInvocations(&next)
	route, readable := routeForRun(previous)
	if !readable {
		next.Stage = invocationStage
		next.Status = store.StatusFailed
		next.PendingQuestions = nil
		return next
	}
	transition, err := workflow.DefaultRegistry().ResolveRouteReportTransition(route, invocationStage, value.Outcome)
	if err != nil {
		next.Stage = invocationStage
		next.Status = store.StatusFailed
		next.PendingQuestions = nil
		return next
	}
	next.Stage = transition.Stage
	next.Status = transition.Status
	switch value.Outcome {
	case report.OutcomeNeedsClarification:
		next.PendingQuestions = pendingQuestionsFromReport(value.Questions)
	case report.OutcomeCannotProceed:
		next.PendingQuestions = nil
	default:
		if value.Handoff != nil && len(value.Handoff.ProductionFilesChanged) != 0 {
			next.RoleHandoff = roleHandoffFromReport(*value.Handoff)
		}
		next.PendingQuestions = nil
	}
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	return next
}

// readAcceptedAgentReport returns the immutable report belonging to a terminal
// invocation. It makes repeated acceptance a read-only idempotent operation for
// the durable journal store without re-finishing its harness session. This
// function reads from the filesystem and should only be used as a fallback when
// the pending effect payload is unavailable.
func readAcceptedAgentReport(invocation store.Invocation) (report.Report, error) {
	path := reportPath(invocation)
	if roleIsKind(invocation, workflow.RoleKindTest) {
		return report.ReadEnvelope(path)
	}
	return report.Read(path)
}

// readAcceptedReportFromEffect extracts the accepted report from a result
// acceptance pending effect payload. This provides the immutable snapshot that
// was validated during acceptance, avoiding re-reading mutable report.json.
func readAcceptedReportFromEffect(pending store.PendingEffect) (report.Report, error) {
	payload, err := effectkernel.ReadResultAcceptance(pending)
	if err != nil {
		return report.Report{}, err
	}
	if strings.TrimSpace(payload.AcceptedReport) == "" {
		return report.Report{}, errors.New("result acceptance payload has no accepted report")
	}
	var value report.Report
	if err := json.Unmarshal([]byte(payload.AcceptedReport), &value); err != nil {
		return report.Report{}, fmt.Errorf("decode accepted report from effect: %w", err)
	}
	return value, nil
}

// readAcceptedReviewReportFromEffect extracts the immutable review invocation
// and report snapshot from a result-acceptance effect. Legacy payloads without
// the snapshot fall back to the invocation artifact retained on disk.
func readAcceptedReviewReportFromEffect(pending store.PendingEffect) (store.Invocation, report.Report, bool, error) {
	payload, err := effectkernel.ReadResultAcceptance(pending)
	if err != nil {
		return store.Invocation{}, report.Report{}, false, err
	}
	if !roleIsKind(payload.Invocation, workflow.RoleKindReview) {
		return payload.Invocation, report.Report{}, false, nil
	}
	if strings.TrimSpace(payload.AcceptedReport) == "" {
		value, readErr := readAcceptedAgentReport(payload.Invocation)
		if readErr != nil {
			return store.Invocation{}, report.Report{}, true, fmt.Errorf("read accepted review report from invocation artifact: %w", readErr)
		}
		return payload.Invocation, value, true, nil
	}
	var value report.Report
	if err := json.Unmarshal([]byte(payload.AcceptedReport), &value); err != nil {
		return store.Invocation{}, report.Report{}, true, fmt.Errorf("decode accepted review report from effect: %w", err)
	}
	return payload.Invocation, value, true, nil
}
