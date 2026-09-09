package factory

import (
	"context"
	"errors"
	"fmt"
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
	// Report is the structured proposal read from the invocation directory.
	Report report.Report
	// Worktree is the inspected state of the run worktree.
	Worktree gitadapter.WorktreeState
	// ObservedChanges are the changed paths attributed to this invocation. An
	// open objection cycle attributes only what the revision itself changed.
	ObservedChanges []string
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

// AcceptanceAdmission is the decision pure admission reaches. It carries no
// projection: it says only whether the report may be accepted, and which of
// the two non-rejecting exits the sequencer must take.
type AcceptanceAdmission struct {
	// Unverifiable marks a test report that failed schema validation in the
	// one shape the coordinator parks for a person instead of refusing.
	Unverifiable bool
	// ImplementationObjection marks an accepted implementation report that
	// disputes a protected test and redirects the run back to the test role.
	ImplementationObjection bool
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
	objectionGate             func(context.Context, config.RepositoryRegistration, SpecificationPacket) (bool, string)
	persistRun                func(context.Context, config.RepositoryRegistration, RunStore, store.Run, store.Run) error
}

// reportAcceptance owns the report-acceptance path. Its adapters are explicit
// constructor arguments, so the module never reaches the coordinator's
// dependency set.
type reportAcceptance struct {
	journal   acceptanceJournal
	lifecycle acceptanceLifecycle
	worktree  gitadapter.WorktreeInspector
	clock     Clock
	hooks     reportAcceptanceHooks
}

// newReportAcceptance binds the module to one adapter set.
func newReportAcceptance(journal acceptanceJournal, lifecycle acceptanceLifecycle, worktree gitadapter.WorktreeInspector, clock Clock, hooks reportAcceptanceHooks) *reportAcceptance {
	return &reportAcceptance{journal: journal, lifecycle: lifecycle, worktree: worktree, clock: clock, hooks: hooks}
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
	// EvaluationRecorder is the optional content-free evaluation projection.
	EvaluationRecorder evaluationRecorder
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
	snapshot, err := a.gather(ctx, *request.Run, *invocation, roleDefinition)
	if err != nil {
		return AgentResult{}, err
	}
	admission, err := AdmitReport(snapshot)
	if err != nil {
		return AgentResult{}, err
	}
	if admission.Unverifiable {
		if snapshot.TestRevisionActive() {
			return a.hooks.pauseUnverifiableRevision(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report)
		}
		return a.hooks.pauseUnverifiableTest(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report)
	}
	objection := acceptanceObjection{Present: admission.ImplementationObjection}
	if objection.Present {
		objection.Automated, objection.Reason = a.hooks.objectionGate(ctx, request.Registration, snapshot.Packet)
	}
	if err := recordAcceptanceEvaluation(ctx, request.EvaluationRecorder, snapshot, admission); err != nil {
		return AgentResult{}, err
	}
	return a.commit(ctx, request, invocationStore, invocation, snapshot, objection)
}

// identify resolves the invocation the request names and the role definition
// that owns its stage. It is the only store read outside gather.
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
func (a *reportAcceptance) gather(ctx context.Context, run store.Run, invocation store.Invocation, roleDefinition workflow.RoleDefinition) (AcceptanceSnapshot, error) {
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
		return AcceptanceSnapshot{}, fmt.Errorf("decode specification packet for agent report: %w", err)
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

// AdmitReport decides whether one gathered report may be accepted. It is pure
// over the snapshot: it takes no adapter and no context, so a rejection can be
// reproduced from a run, an invocation, a report, and a worktree state alone.
func AdmitReport(snapshot AcceptanceSnapshot) (AcceptanceAdmission, error) {
	run, invocation := snapshot.Run, snapshot.Invocation
	if run.CheckpointSHA != "" && snapshot.Worktree.HeadSHA != run.CheckpointSHA {
		return AcceptanceAdmission{}, fmt.Errorf("agent report worktree HEAD %q does not match checkpoint %q", snapshot.Worktree.HeadSHA, run.CheckpointSHA)
	}
	if snapshot.ReviewInvocation() && len(snapshot.Worktree.ChangedPaths) != 0 {
		return AcceptanceAdmission{}, fmt.Errorf("%s changed the immutable checkpoint worktree", invocation.Role)
	}
	if snapshot.TestInvocation() && !independentTestStageDeclared(snapshot.Packet) {
		return AcceptanceAdmission{}, errors.New("test-stage reports are unavailable in advisory mode without a selected route; implementation owns TDD")
	}
	if err := validateImplementationOwnedSignals(snapshot.Report, invocation, snapshot.Packet); err != nil {
		return AcceptanceAdmission{}, err
	}
	if err := validateReviewRepairTestOwnerRouting(snapshot.Report, invocation, run, snapshot.Packet); err != nil {
		return AcceptanceAdmission{}, err
	}
	if snapshot.ReviewInvocation() && snapshot.Report.ReviewHandoff != nil && snapshot.Report.ReviewHandoff.ReviewedSHA != run.CheckpointSHA {
		return AcceptanceAdmission{}, &ReviewCheckpointMismatchError{
			Role:         invocation.Role,
			InvocationID: invocation.ID,
			Expected:     run.CheckpointSHA,
			Observed:     snapshot.Report.ReviewHandoff.ReviewedSHA,
		}
	}
	if err := report.Validate(snapshot.Report, acceptanceValidationContext(snapshot)); err != nil {
		if isUnverifiableTestReport(snapshot.Report, invocation) {
			return AcceptanceAdmission{Unverifiable: true}, nil
		}
		return AcceptanceAdmission{}, err
	}
	if snapshot.TestInvocation() {
		if err := validateTestStageReportPolicy(snapshot.Report, snapshot.Packet.RepositoryConfig.TestPolicy); err != nil {
			return AcceptanceAdmission{}, err
		}
	}
	objection := implementationTestObjection(snapshot.Report, invocation)
	if snapshot.Report.TestObjectionResponse != nil && !snapshot.TestRevisionActive() {
		return AcceptanceAdmission{}, errors.New("test objection response requires an active implementation objection")
	}
	if objection {
		if err := validateImplementationTestObjection(snapshot.Report, invocation, run, snapshot.Packet); err != nil {
			return AcceptanceAdmission{}, err
		}
	}
	if err := admitAcceptancePathOwnership(snapshot); err != nil {
		return AcceptanceAdmission{}, err
	}
	if snapshot.Report.Outcome == report.OutcomeNeedsClarification {
		if err := validateClarificationQuestions(snapshot.Packet, run.PendingQuestions, snapshot.Report.Questions); err != nil {
			return AcceptanceAdmission{}, err
		}
	}
	return AcceptanceAdmission{ImplementationObjection: objection}, nil
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
	return validateProtectedTestPaths(snapshot.Run.Worktree, snapshot.Worktree, snapshot.Run.ProtectedTestPaths)
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

// acceptanceObjection is the resolved implementation objection, including the
// measured-pilot decision that says whether it may run automatically.
type acceptanceObjection struct {
	// Present marks an admitted objection against a protected test.
	Present bool
	// Automated reports whether the authorized pilot decision permits an
	// automated revision cycle.
	Automated bool
	// Reason explains a refused automation to the operator.
	Reason string
}

// recordAcceptanceEvaluation writes the content-free evaluation projection of
// one accepted report. A store without the projection records nothing.
func recordAcceptanceEvaluation(ctx context.Context, recorder evaluationRecorder, snapshot AcceptanceSnapshot, admission AcceptanceAdmission) error {
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
	if admission.ImplementationObjection || (snapshot.TestRevisionActive() && value.TestObjectionResponse != nil) {
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
	// Next is the projected run without the persistence strategy's own edits.
	Next store.Run
	// StopWorker reports whether acceptance ends this invocation's worker.
	StopWorker bool
}

// projectAcceptance builds the accepted invocation and the next run from an
// admitted report. Both persistence strategies share it; only the durability
// mechanism, and the restart-safety edits that mechanism requires, differ.
func projectAcceptance(snapshot AcceptanceSnapshot, objection acceptanceObjection, nativeSessionID string, now time.Time) (acceptanceProjection, error) {
	accepted := snapshot.Invocation
	accepted.NativeSessionID = nativeSessionID
	accepted.Status = acceptedInvocationStatus(snapshot.Report.Outcome)
	accepted.UpdatedAt = now
	previous := snapshot.Run
	next := previous
	releaseActiveInvocation(&next, snapshot.Invocation.ID)
	switch {
	case objection.Present:
		projected, err := projectImplementationTestObjection(previous, snapshot.Report, snapshot.Invocation, snapshot.Packet, snapshot.Worktree, objection.Automated, objection.Reason)
		if err != nil {
			return acceptanceProjection{}, err
		}
		next = projected
	case snapshot.TestInvocation() || snapshot.ReviewInvocation():
		// The test and review stage policies own their own run transition.
	default:
		next = agentReportRunProjection(previous, snapshot.Invocation.Stage, snapshot.Report)
	}
	stopWorker := snapshot.Report.Outcome == report.OutcomeNeedsClarification || snapshot.ReviewInvocation() || objection.Present
	return acceptanceProjection{Invocation: accepted, Previous: previous, Next: next, StopWorker: stopWorker}, nil
}

// commit makes the projection durable and hands the run to the stage policy
// that owns it. The journaled and legacy stores share the projection and the
// dispatch; only the durability mechanism differs.
func (a *reportAcceptance) commit(ctx context.Context, request ReportAcceptanceRequest, invocationStore InvocationStore, invocation *store.Invocation, snapshot AcceptanceSnapshot, objection acceptanceObjection) (AgentResult, error) {
	harnessRuntime, err := a.lifecycle.HarnessRuntime(request.Registration.Cmux.SocketPath, invocation.Harness)
	if err != nil {
		return AgentResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	nativeSessionID, err := reconcileNativeSessionID(*invocation, snapshot.Report)
	if err != nil {
		return AgentResult{}, err
	}
	projection, err := projectAcceptance(snapshot, objection, nativeSessionID, a.clock().UTC())
	if err != nil {
		return AgentResult{}, err
	}
	if _, journaled := request.RunStore.(PendingEffectStore); journaled {
		err = a.commitJournaled(ctx, request, invocationStore, invocation, snapshot, harnessRuntime, projection)
	} else {
		err = a.commitDirect(ctx, request, invocationStore, invocation, snapshot, harnessRuntime, objection, projection)
	}
	if err != nil {
		return AgentResult{}, err
	}
	return a.dispatch(ctx, request, invocation, snapshot, objection)
}

// commitJournaled persists the acceptance as one durable effect. The effect
// payload has to survive a restart on its own, so the review result and the
// revision bump are applied before it is reserved rather than by the stage
// policy that runs after it.
func (a *reportAcceptance) commitJournaled(ctx context.Context, request ReportAcceptanceRequest, invocationStore InvocationStore, invocation *store.Invocation, snapshot AcceptanceSnapshot, harnessRuntime harness.Runtime, projection acceptanceProjection) error {
	next := projection.Next
	if snapshot.ReviewInvocation() {
		if err := applyReviewResultProjection(&next, invocation.Role, snapshot.Report); err != nil {
			return err
		}
	}
	next.Revision = projection.Previous.Revision + 1
	if snapshot.TestInvocation() || snapshot.ReviewInvocation() {
		next.Revision = projection.Previous.Revision
	}
	next.UpdatedAt = a.clock().UTC()
	accepted, committed, err := a.journal.AcceptResult(ctx, request.RunStore, invocationStore, effectkernel.ResultAcceptance{
		Repository: commandRepository(request.Registration),
		SocketPath: request.Registration.Cmux.SocketPath,
		WorkerID:   workerIDForInvocation(projection.Invocation),
		Harness:    harnessRuntime,
		Session: harness.Session{
			InvocationID:    projection.Invocation.ID,
			NativeSessionID: projection.Invocation.NativeSessionID,
			Surface:         invocationSurface(projection.Invocation),
		},
		Invocation: projection.Invocation,
		Previous:   projection.Previous,
		Next:       next,
		StopWorker: projection.StopWorker,
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
func (a *reportAcceptance) commitDirect(ctx context.Context, request ReportAcceptanceRequest, invocationStore InvocationStore, invocation *store.Invocation, snapshot AcceptanceSnapshot, harnessRuntime harness.Runtime, objection acceptanceObjection, projection acceptanceProjection) error {
	if err := harnessRuntime.Finish(ctx, harness.Session{InvocationID: invocation.ID, NativeSessionID: projection.Invocation.NativeSessionID, Surface: invocationSurface(*invocation)}); err != nil {
		return fmt.Errorf("finish accepted harness session: %w", err)
	}
	if snapshot.Report.Outcome == report.OutcomeNeedsClarification || snapshot.ReviewInvocation() {
		if err := a.lifecycle.StopWorker(ctx, workerIDForInvocation(*invocation)); err != nil {
			return err
		}
	}
	*invocation = projection.Invocation
	if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
		return fmt.Errorf("persist accepted invocation: %w", err)
	}
	next := projection.Next
	switch {
	case objection.Present:
		next.Revision = projection.Previous.Revision + 1
		next.UpdatedAt = a.clock().UTC()
		*request.Run = next
		if err := a.hooks.persistRun(ctx, request.Registration, request.RunStore, projection.Previous, next); err != nil {
			return fmt.Errorf("persist implementation test objection: %w", err)
		}
	case snapshot.TestInvocation() || snapshot.ReviewInvocation():
		// The stage policy persists its own transition.
		*request.Run = next
	default:
		next.UpdatedAt = a.clock().UTC()
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
func (a *reportAcceptance) dispatch(ctx context.Context, request ReportAcceptanceRequest, invocation *store.Invocation, snapshot AcceptanceSnapshot, objection acceptanceObjection) (AgentResult, error) {
	switch {
	case objection.Present:
		return a.hooks.finishObjection(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report)
	case snapshot.TestInvocation():
		return a.hooks.acceptTestStage(ctx, request.Registration, request.RunStore, request.Run, invocation, snapshot.Report, snapshot.Worktree)
	case snapshot.ReviewInvocation():
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
func acceptanceEvaluationRecorderForRunStore(runStore RunStore) evaluationRecorder {
	recorder, _ := runStore.(evaluationRecorder)
	return recorder
}
