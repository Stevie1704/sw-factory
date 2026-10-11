package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// Registration, supervisor, and run states of the status document. An
// unknown or unavailable state means status could not observe the value; it
// never means the value is absent.
const (
	stateRegistered    = "registered"
	stateNone          = "none"
	stateUnknown       = "unknown"
	stateLive          = "live"
	stateStale         = "stale"
	stateMissing       = "missing"
	stateNotConfigured = "not_configured"
	stateActive        = "active"
	stateTerminal      = "terminal"
	stateUnavailable   = "unavailable"
)

// startAction is the supported action for an active run without a live
// supervisor. The coordinator it starts mutates tracker and code-host state.
var startAction = actionDocument{
	Summary:       "run `factory start` so the active run can progress",
	Effect:        doctor.EffectExternalMutation,
	Preconditions: []string{"factory doctor reports no blocking check"},
	GuideTopic:    "debug",
}

// statusDocument is the `factory status --json` view of a status result.
type statusDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Command       string               `json:"command"`
	Outcome       string               `json:"outcome"`
	ConfigPath    string               `json:"config_path,omitempty"`
	Registration  registrationDocument `json:"registration"`
	Supervisor    supervisorDocument   `json:"supervisor"`
	Run           runDocument          `json:"run"`
	Recovery      *recoveryDocument    `json:"recovery,omitempty"`
	Blockers      []blockerDocument    `json:"blockers"`
	Error         *errorDocument       `json:"error,omitempty"`
}

// registrationDocument identifies the selected repository and provider.
type registrationDocument struct {
	State      string           `json:"state"`
	Provider   factory.Provider `json:"provider,omitempty"`
	Repository string           `json:"repository,omitempty"`
	Path       string           `json:"path,omitempty"`
}

// supervisorDocument reports coordinator liveness.
type supervisorDocument struct {
	State     string             `json:"state"`
	LockHeld  bool               `json:"lock_held"`
	Heartbeat *heartbeatDocument `json:"heartbeat,omitempty"`
}

// heartbeatDocument is the latest recorded supervisor heartbeat.
type heartbeatDocument struct {
	Coordinator string    `json:"coordinator"`
	PID         int       `json:"pid"`
	RenewedAt   time.Time `json:"renewed_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// runDocument is the active or latest run. Only State is set when no run
// exists or when the run could not be read.
type runDocument struct {
	State               string                   `json:"state"`
	ID                  string                   `json:"id,omitempty"`
	IssueNumber         int                      `json:"issue_number,omitempty"`
	Stage               store.Stage              `json:"stage,omitempty"`
	Status              store.Status             `json:"status,omitempty"`
	Activity            factory.RunActivity      `json:"activity,omitempty"`
	Branch              string                   `json:"branch,omitempty"`
	Worktree            string                   `json:"worktree,omitempty"`
	CheckpointSHA       string                   `json:"checkpoint_sha,omitempty"`
	TestPolicy          string                   `json:"test_policy,omitempty"`
	Route               string                   `json:"route,omitempty"`
	LifecycleReason     string                   `json:"lifecycle_reason,omitempty"`
	ActiveInvocationIDs []string                 `json:"active_invocation_ids,omitempty"`
	FailedGates         []failedGateDocument     `json:"failed_gates,omitempty"`
	DefectProposals     []defectProposalDocument `json:"defect_proposals,omitempty"`
	UpdatedAt           *time.Time               `json:"updated_at,omitempty"`
}

// failedGateDocument is one blocking gate that failed at the checkpoint.
type failedGateDocument struct {
	Gate          string            `json:"gate"`
	Phase         store.GatePhase   `json:"phase"`
	Outcome       store.GateOutcome `json:"outcome"`
	CheckpointSHA string            `json:"checkpoint_sha"`
}

// defectProposalDocument names one worker defect proposal without its
// worker-authored text; `factory bug-report --run-id` shows the text.
type defectProposalDocument struct {
	InvocationID string `json:"invocation_id"`
	Code         string `json:"code"`
}

// recoveryDocument is the coordinator's read-only recovery diagnosis.
type recoveryDocument struct {
	Code             string                 `json:"code"`
	RunID            string                 `json:"run_id"`
	SourcesAgree     bool                   `json:"sources_agree"`
	Recoverable      bool                   `json:"recoverable"`
	InvocationExists bool                   `json:"invocation_exists"`
	PendingEffect    *pendingEffectDocument `json:"pending_effect,omitempty"`
	Discrepancies    []discrepancyDocument  `json:"discrepancies"`
	SafeActions      []string               `json:"safe_actions"`
}

// pendingEffectDocument identifies a reserved external effect.
type pendingEffectDocument struct {
	ID   string                  `json:"id"`
	Kind store.PendingEffectKind `json:"kind"`
}

// discrepancyDocument is one disagreement between persisted and external
// projections.
type discrepancyDocument struct {
	InvocationID string                          `json:"invocation_id,omitempty"`
	Kind         factory.RecoveryDiscrepancyKind `json:"kind"`
	Source       string                          `json:"source"`
	Field        string                          `json:"field"`
	Expected     string                          `json:"expected"`
	Observed     string                          `json:"observed"`
	Recoverable  bool                            `json:"recoverable"`
}

// blockerDocument is one known condition that stops or threatens progress.
type blockerDocument struct {
	Code         string          `json:"code"`
	LikelyOrigin doctor.Origin   `json:"likely_origin"`
	Problem      string          `json:"problem"`
	Facts        []factDocument  `json:"facts"`
	Action       *actionDocument `json:"action,omitempty"`
}

// newStatusDocument projects a status result, and the failure that stopped
// its collection when err is not nil, without changing either.
func newStatusDocument(result factory.StatusResult, err error) statusDocument {
	document := statusDocument{
		SchemaVersion: diagnosticsSchemaVersion,
		Command:       "status",
		Outcome:       outcomeOK,
		ConfigPath:    result.ConfigPath,
		Registration:  newRegistrationDocument(result, err),
		Supervisor:    newSupervisorDocument(result),
		Run:           newRunDocument(result, err),
		Recovery:      newRecoveryDocument(result.Recovery),
		Blockers:      statusBlockers(result),
	}
	if err != nil {
		document.Outcome = outcomeError
		document.Error = statusError(err)
	}
	return document
}

// statusSourceMessages are the fixed messages of status collection
// failures. The underlying error text is not shown, because a decode error
// can quote configuration values.
var statusSourceMessages = map[factory.StatusSource]string{
	factory.StatusSourceConfiguration: "the host configuration cannot be loaded; run factory doctor for the field-level diagnosis",
	factory.StatusSourceStore:         "the operational store cannot be read",
	factory.StatusSourceSupervisor:    "the coordinator lock or supervisor heartbeat cannot be read",
}

// statusError classifies a status failure by its typed source. Only an
// unloadable host configuration has evidence of an environment origin; a
// failed read alone does not identify its origin.
func statusError(err error) *errorDocument {
	var unavailable *factory.StatusUnavailableError
	if !errors.As(err, &unavailable) {
		return newErrorDocument("status.operational_failure", doctor.OriginUnknown, errors.New("status collection failed"))
	}
	origin := doctor.OriginUnknown
	if unavailable.Source == factory.StatusSourceConfiguration {
		origin = doctor.OriginEnvironment
	}
	return newErrorDocument("status."+string(unavailable.Source)+".unavailable", origin, errors.New(statusSourceMessages[unavailable.Source]))
}

// newRegistrationDocument distinguishes no registration from an unread one.
func newRegistrationDocument(result factory.StatusResult, err error) registrationDocument {
	if result.RepositoryPath == "" {
		if err != nil {
			return registrationDocument{State: stateUnknown}
		}
		return registrationDocument{State: stateNone}
	}
	return registrationDocument{State: stateRegistered, Provider: result.Provider, Repository: result.Repository.String(), Path: result.RepositoryPath}
}

// newSupervisorDocument distinguishes live, stale, missing, unconfigured,
// and uninspected supervisor liveness.
func newSupervisorDocument(result factory.StatusResult) supervisorDocument {
	document := supervisorDocument{State: supervisorState(result), LockHeld: result.SupervisorLockHeld}
	if heartbeat := result.SupervisorHeartbeat; heartbeat != nil {
		document.Heartbeat = &heartbeatDocument{
			Coordinator: heartbeat.Coordinator,
			PID:         heartbeat.PID,
			RenewedAt:   heartbeat.RenewedAt.UTC(),
			ExpiresAt:   heartbeat.ExpiresAt.UTC(),
		}
	}
	return document
}

// supervisorState returns the liveness class of one status result.
func supervisorState(result factory.StatusResult) string {
	switch {
	case result.SupervisorLive:
		return stateLive
	case !result.SupervisorObserved && result.RepositoryPath == "" && result.ConfigPath != "":
		return stateNotConfigured
	case !result.SupervisorObserved:
		return stateUnknown
	case result.SupervisorHeartbeat == nil:
		return stateMissing
	default:
		return stateStale
	}
}

// newRunDocument projects the latest run or the explicit reason none is
// shown.
func newRunDocument(result factory.StatusResult, err error) runDocument {
	run := result.LatestRun
	if run == nil {
		if err != nil {
			return runDocument{State: stateUnavailable}
		}
		return runDocument{State: stateNone}
	}
	document := runDocument{
		State:               stateActive,
		ID:                  run.ID,
		IssueNumber:         run.IssueNumber,
		Stage:               run.Stage,
		Status:              run.Status,
		Activity:            result.Activity,
		Branch:              run.Branch,
		Worktree:            run.Worktree,
		CheckpointSHA:       run.CheckpointSHA,
		TestPolicy:          factory.TestPolicyDescription(result.TestPolicyMode),
		Route:               result.Route.Description(),
		LifecycleReason:     run.LifecycleReason,
		ActiveInvocationIDs: append([]string(nil), run.ActiveInvocationIDs...),
	}
	if store.IsTerminalStatus(run.Status) {
		document.State = stateTerminal
	}
	if !run.UpdatedAt.IsZero() {
		updated := run.UpdatedAt.UTC()
		document.UpdatedAt = &updated
	}
	for _, gate := range result.FailedGates {
		document.FailedGates = append(document.FailedGates, failedGateDocument{Gate: gate.GateName, Phase: gate.Phase, Outcome: gate.Outcome, CheckpointSHA: gate.CheckpointSHA})
	}
	for _, proposal := range result.DefectProposals {
		document.DefectProposals = append(document.DefectProposals, defectProposalDocument{InvocationID: proposal.InvocationID, Code: proposal.Code})
	}
	return document
}

// newRecoveryDocument projects the coordinator's recovery diagnosis.
func newRecoveryDocument(diagnosis *factory.RecoveryDiagnosis) *recoveryDocument {
	if diagnosis == nil {
		return nil
	}
	document := &recoveryDocument{
		Code:             diagnosis.Code,
		RunID:            diagnosis.RunID,
		SourcesAgree:     diagnosis.SourcesAgree,
		Recoverable:      diagnosis.Recoverable,
		InvocationExists: diagnosis.InvocationExists,
		Discrepancies:    make([]discrepancyDocument, 0, len(diagnosis.Discrepancies)),
		SafeActions:      append([]string{}, diagnosis.SafeActions...),
	}
	if pending := diagnosis.PendingEffect; pending != nil {
		document.PendingEffect = &pendingEffectDocument{ID: pending.ID, Kind: pending.Kind}
	}
	for _, discrepancy := range diagnosis.Discrepancies {
		document.Discrepancies = append(document.Discrepancies, discrepancyDocument(discrepancy))
	}
	return document
}

// statusBlockers derives the known blockers from typed status fields only.
// Each blocker keeps a likely origin; a disagreement or a paused run alone
// does not prove its origin, so those stay unknown.
func statusBlockers(result factory.StatusResult) []blockerDocument {
	blockers := []blockerDocument{}
	run := result.LatestRun
	if run == nil {
		return blockers
	}
	runFact := factDocument{Name: "run_id", Value: run.ID}
	if !store.IsTerminalStatus(run.Status) && result.SupervisorObserved && !result.SupervisorLive {
		code, problem := "run.supervisor.stale", "the active run's supervisor heartbeat expired or its lock is not held"
		if result.SupervisorHeartbeat == nil {
			code, problem = "run.supervisor.missing", "the active run has no recorded supervisor heartbeat"
		}
		action := startAction
		blockers = append(blockers, blockerDocument{Code: code, LikelyOrigin: doctor.OriginEnvironment, Problem: problem, Facts: []factDocument{runFact}, Action: &action})
	}
	if run.Status != store.StatusActive {
		for _, gate := range result.FailedGates {
			code, origin := gateBlocker(gate.Outcome)
			blockers = append(blockers, blockerDocument{
				Code:         code,
				LikelyOrigin: origin,
				Problem:      fmt.Sprintf("blocking gate %q ended with outcome %s at the current checkpoint", gate.GateName, gate.Outcome),
				Facts:        []factDocument{runFact, {Name: "gate", Value: gate.GateName}, {Name: "phase", Value: string(gate.Phase)}, {Name: "checkpoint_sha", Value: gate.CheckpointSHA}},
			})
		}
	}
	if code, origin, ok := pausedRunBlocker(run.Status); ok {
		blockers = append(blockers, blockerDocument{Code: code, LikelyOrigin: origin, Problem: "the run is " + string(run.Status), Facts: []factDocument{runFact}})
	}
	if recovery := result.Recovery; recovery != nil {
		if !recovery.SourcesAgree {
			blockers = append(blockers, blockerDocument{Code: "recovery.sources_disagree", LikelyOrigin: doctor.OriginUnknown, Problem: "persisted and observed run projections disagree", Facts: []factDocument{runFact}})
		}
		if pending := recovery.PendingEffect; pending != nil {
			blockers = append(blockers, blockerDocument{Code: "recovery.pending_effect", LikelyOrigin: doctor.OriginUnknown, Problem: "an external effect was reserved but not confirmed", Facts: []factDocument{runFact, {Name: "effect_id", Value: pending.ID}}})
		}
	}
	return blockers
}

// gateBlocker classifies a gate outcome by its evidence. A failed command is
// repository evidence and a runtime error is environment evidence. A setup
// failure can come from the repository setup command or from the runtime,
// so its origin stays unknown.
func gateBlocker(outcome store.GateOutcome) (string, doctor.Origin) {
	switch outcome {
	case store.GateOutcomeFailed:
		return "run.gate.failed", doctor.OriginRepository
	case store.GateOutcomeError:
		return "run.gate.error", doctor.OriginEnvironment
	default:
		return "run.gate." + string(outcome), doctor.OriginUnknown
	}
}

// pausedRunBlocker classifies a run status that stops unattended progress.
func pausedRunBlocker(status store.Status) (string, doctor.Origin, bool) {
	switch status {
	case store.StatusWaitingForHarness:
		return "run.waiting_for_harness", doctor.OriginEnvironment, true
	case store.StatusWaitingForHuman:
		return "run.waiting_for_human", doctor.OriginUnknown, true
	case store.StatusFailed:
		return "run.failed", doctor.OriginUnknown, true
	}
	return "", "", false
}
