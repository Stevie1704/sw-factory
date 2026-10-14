package cli

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// Outcomes of an operation document. Only a refused operation was not
// attempted; every other outcome may have changed state.
const (
	operationCompleted = "completed"
	operationPending   = "pending"
	operationBlocked   = "blocked"
	operationRefused   = "refused"
	operationError     = "error"
)

// operationDocument is the --json result of one finite mutation command.
// Result holds the command-specific fields; it never holds a whole service
// or store struct.
type operationDocument struct {
	SchemaVersion int                `json:"schema_version"`
	Command       string             `json:"command"`
	Outcome       string             `json:"outcome"`
	Attempted     bool               `json:"attempted"`
	Reason        *reasonDocument    `json:"reason,omitempty"`
	Identities    identitiesDocument `json:"identities"`
	Result        any                `json:"result,omitempty"`
	Recovery      *recoveryDocument  `json:"recovery,omitempty"`
	Blockers      []operationBlocker `json:"blockers,omitempty"`
	Verification  *actionDocument    `json:"verification,omitempty"`
	Error         *errorDocument     `json:"error,omitempty"`
}

// reasonDocument is the stable code of a refused, blocked, or pending outcome.
type reasonDocument struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// identitiesDocument names the state an operation produced or observed.
type identitiesDocument struct {
	RunID             string `json:"run_id,omitempty"`
	Revision          int64  `json:"revision,omitempty"`
	IssueNumber       int    `json:"issue_number,omitempty"`
	CheckpointSHA     string `json:"checkpoint_sha,omitempty"`
	InvocationID      string `json:"invocation_id,omitempty"`
	EffectID          string `json:"effect_id,omitempty"`
	PullRequestNumber int    `json:"pull_request_number,omitempty"`
}

// operationBlocker is one condition that stopped an operation.
type operationBlocker struct {
	RunID  string `json:"run_id,omitempty"`
	Reason string `json:"reason"`
	Action string `json:"action,omitempty"`
}

// Fixed messages of the classified outcomes. Error text can quote external
// observations, so a document never copies it.
const (
	messageRecoveryBlocked  = "the run waits for human reconciliation; read recovery.discrepancies and recovery.actions"
	messageWorkflowFailure  = "the durable workflow state is invalid; read recovery.discrepancies"
	messageOutcomeUnknown   = "an external call did not answer; its outcome is unknown; verify the state before you run the command again"
	messageReplayReserved   = "an external call did not answer during the replay; the effect stays reserved and the next reconciliation replays it"
	messageOperationalError = "the command failed; standard error has the detail"
)

// newOperationDocument classifies the error of one command into its outcome.
func newOperationDocument(command string, err error) operationDocument {
	document := operationDocument{SchemaVersion: diagnosticsSchemaVersion, Command: command, Outcome: operationCompleted, Attempted: true}
	if err == nil {
		return document
	}
	var (
		refused          *factory.OperationRefusedError
		infrastructure   *factory.InfrastructureDiscrepancyError
		recoveryRequired *factory.RecoveryRequiredError
		workflow         *factory.WorkflowFailureError
		timeout          *hostcmd.TimeoutError
		cleanupConfirm   *factory.CleanupConfirmationRequiredError
		cleanupChanged   *factory.CleanupPlanChangedError
		cleanupBlocked   *factory.CleanupBlockedError
		resetConfirm     *factory.ResetConfirmationRequiredError
		resetBlocked     *factory.ResetBlockedError
		resetIncomplete  *factory.ResetIncompleteError
	)
	refusal := func(code, message string) {
		document.Outcome, document.Attempted = operationRefused, false
		document.Reason = &reasonDocument{Code: code, Message: message}
	}
	blocking := func(code, message string) {
		document.Outcome = operationBlocked
		document.Reason = &reasonDocument{Code: code, Message: message}
	}
	switch {
	case errors.As(err, &refused):
		refusal(string(refused.Code), boundedMessage(refused.Message))
	case errors.As(err, &cleanupConfirm):
		refusal("cleanup.confirmation_required", cleanupConfirm.Error())
	case errors.As(err, &cleanupChanged):
		refusal("cleanup.plan_changed", cleanupChanged.Error())
	case errors.As(err, &resetConfirm):
		refusal("reset.confirmation_required", resetConfirm.Error())
	// A blocked cleanup or reset refused before any deletion, so it is a
	// refusal. An incomplete reset deleted targets, so it is blocked.
	case errors.As(err, &cleanupBlocked):
		refusal("cleanup.blocked", cleanupBlocked.Error())
		for _, run := range cleanupBlocked.Runs {
			document.Blockers = append(document.Blockers, operationBlocker{RunID: run.RunID, Reason: run.Reason})
		}
	case errors.As(err, &resetBlocked):
		refusal("reset.blocked", resetBlocked.Error())
		for _, blocker := range resetBlocked.Blockers {
			document.Blockers = append(document.Blockers, operationBlocker{RunID: blocker.RunID, Reason: blocker.Reason, Action: blocker.Action})
		}
	case errors.As(err, &resetIncomplete):
		blocking("reset.incomplete", resetIncomplete.Error())
	case errors.As(err, &workflow):
		blocking("recovery.workflow_failure", messageWorkflowFailure)
	case errors.As(err, &infrastructure):
		blocking("recovery.waiting_for_human", messageRecoveryBlocked)
		document.Recovery = newRecoveryDocument(&infrastructure.Diagnosis)
	case errors.As(err, &recoveryRequired):
		blocking("recovery.waiting_for_human", messageRecoveryBlocked)
		document.Recovery = newRecoveryDocument(&recoveryRequired.Diagnosis)
	case errors.As(err, &timeout):
		document.Outcome = operationPending
		document.Reason = &reasonDocument{Code: "external.outcome_unknown", Message: messageOutcomeUnknown}
	default:
		document.Outcome = operationError
		document.Error = newErrorDocument(command+".operational_failure", doctor.OriginUnknown, errors.New(messageOperationalError))
	}
	return document
}

// boundedMessage bounds one factory-authored message in a document.
func boundedMessage(message string) string {
	if len(message) > maxErrorMessageBytes {
		return message[:maxErrorMessageBytes]
	}
	return message
}

// writeOperationJSON writes one operation document. An operational error also
// writes its detail to errorsOutput, because the document never copies error
// text. It returns 0 for a completed operation and 1 otherwise.
func writeOperationJSON(output, errorsOutput io.Writer, configPath string, document operationDocument, err error) int {
	bindConfiguration(document.Verification, configPath)
	document.Recovery.bindConfiguration(configPath)
	if document.Outcome == operationError {
		writeError(errorsOutput, err)
	}
	if !writeJSONDocument(output, errorsOutput, document) || document.Outcome != operationCompleted {
		return 1
	}
	return 0
}

// verifyStatus is the inspection that confirms the effect of a run command.
func verifyStatus(summary string) *actionDocument {
	return &actionDocument{
		ID: string(factory.ActionInspectStatus), Kind: factory.ActionKindInspection, Summary: summary,
		Effect: doctor.EffectInspection, Preconditions: []string{}, Authorization: factory.AuthorizationNone,
		Command: []string{"factory", "status", "--json"}, Argv: []string{"factory", "status", "--json"},
	}
}

// verifyDoctor is the inspection that confirms an installation command.
func verifyDoctor(summary string) *actionDocument {
	return &actionDocument{
		ID: string(factory.ActionInspectDoctor), Kind: factory.ActionKindInspection, Summary: summary,
		Effect: doctor.EffectInspection, Preconditions: []string{}, Authorization: factory.AuthorizationNone,
		Command: []string{"factory", "doctor", "--json"}, Argv: []string{"factory", "doctor", "--json"},
	}
}

// runIdentities names the durable run an operation produced or observed.
func runIdentities(run store.Run) identitiesDocument {
	return identitiesDocument{RunID: run.ID, Revision: run.Revision, IssueNumber: run.IssueNumber, CheckpointSHA: run.CheckpointSHA}
}

// runStateDocument is the stage and status of a run after an operation.
type runStateDocument struct {
	Stage  store.Stage  `json:"stage,omitempty"`
	Status store.Status `json:"status,omitempty"`
}

// newRunStateDocument projects the stage and status of run.
func newRunStateDocument(run store.Run) runStateDocument {
	return runStateDocument{Stage: run.Stage, Status: run.Status}
}

// resumeResultDocument is the result of `factory resume --json`.
type resumeResultDocument struct {
	runStateDocument
	Continuation string `json:"continuation,omitempty"`
}

// newResumeDocument projects one resume. A resume without an invocation
// re-evaluated the checks of the same checkpoint.
func newResumeDocument(result factory.ResumeResult, err error) operationDocument {
	document := newOperationDocument("resume", err)
	document.Identities = runIdentities(result.Run)
	document.Identities.InvocationID = result.Invocation.ID
	if result.Run.ID != "" {
		continuation := "checks"
		if result.Invocation.ID != "" {
			continuation = "agent"
		}
		document.Result = resumeResultDocument{runStateDocument: newRunStateDocument(result.Run), Continuation: continuation}
	}
	document.Verification = verifyStatus("confirm the run stage, status, and checkpoint with `factory status --json`")
	return document
}

// reconcileResultDocument is the result of `factory reconcile --json`.
type reconcileResultDocument struct {
	runStateDocument
	Reconciliation factory.RecoveryOutcome `json:"reconciliation"`
}

// newReconcileDocument projects one reconciliation or effect abandonment. A
// replay that timed out is pending: the effect stays reserved.
func newReconcileDocument(result factory.RecoveryResult, err error) operationDocument {
	document := newOperationDocument("reconcile", err)
	if result.Outcome == factory.RecoveryOutcomeRetryable && err != nil {
		document.Outcome, document.Error = operationPending, nil
		document.Reason = &reasonDocument{Code: "external.outcome_unknown", Message: messageReplayReserved}
	}
	if result.Run == nil {
		if err == nil {
			document.Result = reconcileResultDocument{Reconciliation: result.Outcome}
		}
		return document
	}
	document.Identities = runIdentities(*result.Run)
	if pending := result.Diagnosis.PendingEffect; pending != nil {
		document.Identities.EffectID = pending.ID
	}
	document.Result = reconcileResultDocument{runStateDocument: newRunStateDocument(*result.Run), Reconciliation: result.Outcome}
	if document.Recovery == nil && result.Diagnosis.RunID != "" {
		document.Recovery = newRecoveryDocument(&result.Diagnosis)
	}
	document.Verification = verifyStatus("confirm the reconciled run and any pending effect with `factory status --json`")
	return document
}

// authRefreshResultDocument is the result of `factory auth refresh --json`.
type authRefreshResultDocument struct {
	Harness string `json:"harness"`
	Resumed bool   `json:"resumed"`
}

// newAuthRefreshDocument projects one credential refresh.
func newAuthRefreshDocument(result factory.AuthRefreshResult, err error) operationDocument {
	document := newOperationDocument("auth-refresh", err)
	document.Identities = runIdentities(result.Run)
	document.Identities.InvocationID = result.Invocation.ID
	if err == nil {
		document.Result = authRefreshResultDocument{Harness: string(result.Harness), Resumed: result.Resumed}
	}
	document.Verification = verifyStatus("confirm the run status with `factory status --json`")
	return document
}

// stopResultDocument is the result of `factory stop --json`.
type stopResultDocument struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

// newStopDocument projects one stop request. A requested stop completes
// asynchronously, so its verification reads the supervisor state.
func newStopDocument(result factory.StopResult, err error) operationDocument {
	document := newOperationDocument("stop", err)
	if err == nil {
		document.Result = stopResultDocument{Running: result.Running, PID: result.PID}
	}
	document.Verification = verifyStatus("confirm that supervisor.state is no longer live with `factory status --json`")
	return document
}

// initResultDocument is the result of `factory init --json`.
type initResultDocument struct {
	ConfigPath string `json:"config_path"`
}

// newInitDocument projects one host configuration creation.
func newInitDocument(result factory.InitResult, err error) operationDocument {
	document := newOperationDocument("init", err)
	if err == nil {
		document.Result = initResultDocument{ConfigPath: result.ConfigPath}
	}
	document.Verification = verifyDoctor("confirm the host configuration with `factory doctor --json`")
	return document
}

// inferredValueDocument is one registration value inferred from the host.
type inferredValueDocument struct {
	Flag  string `json:"flag"`
	Value string `json:"value"`
}

// registerResultDocument is the result of `factory register --json`.
type registerResultDocument struct {
	RepositoryPath      string                  `json:"repository_path"`
	OperationalDataPath string                  `json:"operational_data_path"`
	Updated             bool                    `json:"updated"`
	Inferred            []inferredValueDocument `json:"inferred"`
}

// newRegisterDocument projects one repository registration.
func newRegisterDocument(result factory.RegisterResult, err error) operationDocument {
	document := newOperationDocument("register", err)
	if err == nil {
		registered := registerResultDocument{RepositoryPath: result.RepositoryPath, OperationalDataPath: result.OperationalDataPath, Updated: result.Updated, Inferred: []inferredValueDocument{}}
		for _, value := range result.Inferred {
			registered.Inferred = append(registered.Inferred, inferredValueDocument{Flag: value.Flag, Value: value.Value})
		}
		document.Result = registered
	}
	document.Verification = verifyDoctor("confirm the registration with `factory doctor --json`")
	return document
}

// labelsResultDocument is the result of `factory bootstrap-labels --json`.
type labelsResultDocument struct {
	Repository string   `json:"repository"`
	Labels     []string `json:"labels"`
}

// newBootstrapLabelsDocument projects one label bootstrap.
func newBootstrapLabelsDocument(result factory.BootstrapLabelsResult, err error) operationDocument {
	document := newOperationDocument("bootstrap-labels", err)
	if err == nil {
		document.Result = labelsResultDocument{Repository: result.Repository.String(), Labels: append([]string{}, result.Labels...)}
	}
	document.Verification = verifyDoctor("confirm the factory labels with `factory doctor --json`")
	return document
}

// issueResultDocument is the result of `factory issue --json`.
type issueResultDocument struct {
	runStateDocument
	Branch        string `json:"branch,omitempty"`
	Worktree      string `json:"worktree,omitempty"`
	TestPolicy    string `json:"test_policy,omitempty"`
	Route         string `json:"route,omitempty"`
	BaselineGates int    `json:"baseline_gates"`
}

// newIssueDocument projects one claim and its baseline. A baseline failure
// after the claim keeps the claimed run identity in the document.
func newIssueDocument(claimed factory.IssueResult, baseline factory.BaselineResult, err error) operationDocument {
	document := newOperationDocument("issue", err)
	run := claimed.Run
	if baseline.Run.ID != "" {
		run = baseline.Run
	}
	document.Identities = runIdentities(run)
	if err == nil {
		document.Result = issueResultDocument{
			runStateDocument: newRunStateDocument(run),
			Branch:           run.Branch, Worktree: run.Worktree,
			TestPolicy:    factory.TestPolicyDescription(claimed.Packet.RepositoryConfig.TestPolicy.Mode),
			Route:         claimed.Packet.Route.Description(),
			BaselineGates: len(baseline.Gates),
		}
	}
	document.Verification = verifyStatus("confirm the claimed run with `factory status --json`")
	return document
}

// agentResultDocument is the result of `factory agent --json`.
type agentResultDocument struct {
	runStateDocument
	Role            string `json:"role,omitempty"`
	NativeSessionID string `json:"native_session_id,omitempty"`
	TestPolicy      string `json:"test_policy,omitempty"`
	Route           string `json:"route,omitempty"`
}

// newAgentDocument projects one agent launch without its prompt.
func newAgentDocument(launch factory.AgentLaunchResult, err error) operationDocument {
	document := newOperationDocument("agent", err)
	document.Identities = identitiesDocument{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID}
	if err == nil {
		document.Result = agentResultDocument{
			runStateDocument: runStateDocument{Stage: launch.Invocation.Stage},
			Role:             launch.Invocation.Role, NativeSessionID: launch.Invocation.NativeSessionID,
			TestPolicy: factory.TestPolicyDescription(launch.TestPolicyMode), Route: launch.Route.Description(),
		}
	}
	document.Verification = verifyStatus("confirm the active invocation with `factory status --json`")
	return document
}

// agentReportResultDocument is the result of `factory agent-report --json`.
type agentReportResultDocument struct {
	ReportOutcome    string                 `json:"report_outcome"`
	InvocationStatus store.InvocationStatus `json:"invocation_status"`
}

// newAgentReportDocument projects one accepted report without its content.
func newAgentReportDocument(result factory.AgentResult, err error) operationDocument {
	document := newOperationDocument("agent-report", err)
	document.Identities = identitiesDocument{RunID: result.Invocation.RunID, InvocationID: result.Invocation.ID}
	if err == nil {
		document.Result = agentReportResultDocument{ReportOutcome: string(result.Report.Outcome), InvocationStatus: result.Invocation.Status}
	}
	document.Verification = verifyStatus("confirm the run stage with `factory status --json`")
	return document
}

// gateStateDocument is the commit status of one gate.
type gateStateDocument struct {
	Gate  string `json:"gate"`
	State string `json:"state"`
}

// checkRepairDocument is a check repair that a draft-pr request started or
// paused instead of opening the pull request.
type checkRepairDocument struct {
	Outcome   factory.CheckRepairOutcome `json:"outcome"`
	Attempt   int                        `json:"attempt"`
	Budget    int                        `json:"budget"`
	Remaining int                        `json:"remaining"`
}

// draftPullRequestResultDocument is the result of `factory draft-pr --json`.
type draftPullRequestResultDocument struct {
	runStateDocument
	PullRequestURL string               `json:"pull_request_url,omitempty"`
	Gates          []gateStateDocument  `json:"gates"`
	Repair         *checkRepairDocument `json:"repair,omitempty"`
}

// newDraftPullRequestDocument projects one draft pull request request.
func newDraftPullRequestDocument(result factory.DraftPullRequestResult, err error) operationDocument {
	document := newOperationDocument("draft-pr", err)
	run := result.Run
	if result.Repair != nil {
		run = result.Repair.Run
	}
	document.Identities = runIdentities(run)
	document.Identities.PullRequestNumber = result.PullRequest.Number
	if err == nil {
		draft := draftPullRequestResultDocument{runStateDocument: newRunStateDocument(run), Gates: []gateStateDocument{}}
		if result.Repair == nil {
			draft.PullRequestURL = result.PullRequest.URL
		} else {
			repair := result.Repair
			draft.Repair = &checkRepairDocument{Outcome: repair.Outcome, Attempt: repair.Attempt, Budget: repair.Budget, Remaining: repair.Remaining}
			document.Identities.InvocationID = repair.Invocation.ID
		}
		for _, gateResult := range result.Gates {
			draft.Gates = append(draft.Gates, gateStateDocument{Gate: gateResult.GateName, State: string(gateResult.Status.State)})
		}
		document.Result = draft
	}
	document.Verification = verifyStatus("confirm the run stage and failed gates with `factory status --json`")
	return document
}

// lifecycleDocument is a lifecycle transition that a poll applied.
type lifecycleDocument struct {
	Outcome factory.LifecycleOutcome `json:"outcome"`
	Status  store.Status             `json:"status"`
}

// polledCommandDocument is one processed comment command without its text.
type polledCommandDocument struct {
	Kind          string                 `json:"kind"`
	Outcome       factory.CommandOutcome `json:"outcome"`
	RejectionCode string                 `json:"rejection_code,omitempty"`
}

// pollResultDocument is the result of `factory poll --json`.
type pollResultDocument struct {
	Lifecycle *lifecycleDocument      `json:"lifecycle,omitempty"`
	Commands  []polledCommandDocument `json:"commands"`
}

// newPollDocument projects one poll: a lifecycle transition, or the commands
// processed when the lifecycle was unchanged.
func newPollDocument(lifecycle factory.LifecycleResult, commands []factory.CommandResult, err error) operationDocument {
	document := newOperationDocument("poll", err)
	document.Identities = identitiesDocument{RunID: lifecycle.Run.ID}
	if err == nil {
		polled := pollResultDocument{Commands: []polledCommandDocument{}}
		if lifecycle.Outcome != factory.LifecycleUnchanged {
			polled.Lifecycle = &lifecycleDocument{Outcome: lifecycle.Outcome, Status: lifecycle.Run.Status}
		}
		for _, command := range commands {
			kind := string(command.Command.Kind)
			if kind == "" {
				kind = "malformed"
			}
			entry := polledCommandDocument{Kind: kind, Outcome: command.Outcome}
			if command.Rejection != nil {
				entry.RejectionCode = string(command.Rejection.Code)
			}
			polled.Commands = append(polled.Commands, entry)
		}
		document.Result = polled
	}
	document.Verification = verifyStatus("confirm the run status with `factory status --json`")
	return document
}

// cleanupRunDocument is one run of a cleanup plan.
type cleanupRunDocument struct {
	RunID         string       `json:"run_id"`
	Status        store.Status `json:"status"`
	EligibleAt    time.Time    `json:"eligible_at"`
	Worktree      string       `json:"worktree"`
	Branch        string       `json:"branch"`
	StoredOutputs []string     `json:"stored_outputs"`
}

// skippedRunDocument is one run a plan withholds, with its reason.
type skippedRunDocument struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
}

// cleanupResultDocument is the result of `factory cleanup --json`.
type cleanupResultDocument struct {
	Before  time.Time            `json:"before"`
	Runs    []cleanupRunDocument `json:"runs"`
	Skipped []skippedRunDocument `json:"skipped"`
	Deleted []string             `json:"deleted"`
}

// newCleanupDocument projects a cleanup plan and, after confirmation, the
// deleted runs.
func newCleanupDocument(plan factory.CleanupPlan, deleted []store.CleanupDeletionResult, err error) operationDocument {
	document := newOperationDocument("cleanup", err)
	cleanup := cleanupResultDocument{Before: plan.Before.UTC(), Runs: []cleanupRunDocument{}, Skipped: []skippedRunDocument{}, Deleted: []string{}}
	for _, run := range plan.Runs {
		cleanup.Runs = append(cleanup.Runs, cleanupRunDocument{RunID: run.RunID, Status: run.Status, EligibleAt: run.EligibleAt.UTC(), Worktree: run.Worktree, Branch: run.Branch, StoredOutputs: append([]string{}, run.StoredOutputs...)})
	}
	for _, skipped := range plan.Skipped {
		cleanup.Skipped = append(cleanup.Skipped, skippedRunDocument{RunID: skipped.RunID, Reason: skipped.Reason})
	}
	for _, run := range deleted {
		cleanup.Deleted = append(cleanup.Deleted, run.RunID)
	}
	document.Result = cleanup
	return document
}

// resetRunDocument is one run of a reset plan.
type resetRunDocument struct {
	RunID    string       `json:"run_id"`
	Status   store.Status `json:"status"`
	Worktree string       `json:"worktree"`
	Branch   string       `json:"branch"`
}

// resetRemainingDocument is one planned target that reset did not remove.
type resetRemainingDocument struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// resetResultDocument is the result of `factory reset --json`.
type resetResultDocument struct {
	RepositoryPath      string                   `json:"repository_path,omitempty"`
	ConfigPath          string                   `json:"config_path,omitempty"`
	OperationalDataPath string                   `json:"operational_data_path,omitempty"`
	Runs                []resetRunDocument       `json:"runs"`
	Retained            []string                 `json:"retained"`
	Removed             []string                 `json:"removed"`
	Remaining           []resetRemainingDocument `json:"remaining"`
}

// newResetDocument projects a reset plan and, after confirmation, what reset
// removed and what remains.
func newResetDocument(result factory.ResetResult, err error) operationDocument {
	document := newOperationDocument("reset", err)
	plan := result.Plan
	reset := resetResultDocument{
		RepositoryPath: plan.RepositoryPath, ConfigPath: plan.ConfigPath, OperationalDataPath: plan.OperationalDataPath,
		Runs: []resetRunDocument{}, Retained: append([]string{}, plan.Retained...), Removed: append([]string{}, result.Removed...), Remaining: []resetRemainingDocument{},
	}
	for _, run := range plan.Runs {
		reset.Runs = append(reset.Runs, resetRunDocument{RunID: run.RunID, Status: run.Status, Worktree: run.Worktree, Branch: run.Branch})
	}
	for _, remaining := range result.Remaining {
		reset.Remaining = append(reset.Remaining, resetRemainingDocument{Target: remaining.Target, Reason: remaining.Reason})
	}
	document.Result = reset
	return document
}

// writeConfirmationRefusal writes the refusal of an unconfirmed destructive
// command with the plan it would apply. It keeps the text exit status 2.
func writeConfirmationRefusal(output, errorsOutput io.Writer, configPath string, document operationDocument, command string) int {
	bindConfiguration(document.Verification, configPath)
	document.Outcome, document.Attempted = operationRefused, false
	document.Reason = &reasonDocument{Code: command + ".confirmation_required", Message: fmt.Sprintf("%s requires --confirm; no resources removed", command)}
	if !writeJSONDocument(output, errorsOutput, document) {
		return 1
	}
	return 2
}
