package effect

import (
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// StateTransition is the shared state-machine input used by initial claims and
// later transitions. The only difference is whether a status comment is
// created or edited.
type StateTransition struct {
	Repository    github.Repository
	Issue         github.Issue
	Previous      store.Run
	Next          store.Run
	CreateComment bool
	// StopWorker makes terminal transitions keep worker shutdown inside the
	// same durable effect as the GitHub and run projections.
	StopWorker bool
	// PersistBeforeEffects is used by replay-sensitive commands so the
	// processed-comment watermark is durable before GitHub mutation.
	PersistBeforeEffects bool
	// InvalidateResults makes a packet revision boundary invalidate prior
	// invocation and gate projections atomically with the run update.
	InvalidateResults bool
	// InvalidateAllResults makes an authorized specification amendment invalidate
	// every prior gate result, including the baseline result for the superseded
	// packet version.
	InvalidateAllResults bool
}

// stateTransitionEffectPayload is the serialized intent for one paired issue
// label and status-comment projection.
type stateTransitionEffectPayload struct {
	Repository           github.Repository
	Issue                github.Issue
	Previous             store.Run
	Next                 store.Run
	CreateComment        bool
	StopWorker           bool
	InvalidateResults    bool
	InvalidateAllResults bool
}

// statusCommentEffectPayload is the serialized intent for a command
// watermark and its corresponding status-comment edit.
type statusCommentEffectPayload struct {
	Repository github.Repository
	Previous   store.Run
	Next       store.Run
}

// clarificationCommentEffectPayload is the serialized intent for one
// coordinator-authored clarification comment and its run identity projection.
type clarificationCommentEffectPayload struct {
	Repository github.Repository
	Target     int
	Body       string
	// PacketVersion scopes the comment marker to one clarification round so a
	// replay repairs that round's comment instead of overwriting an earlier one.
	PacketVersion int
}

// labelTransitionEffectPayload is the serialized intent for a standalone
// complete issue-label replacement.
type labelTransitionEffectPayload struct {
	Repository  github.Repository
	IssueNumber int
	Labels      []string
}

// commitStatusEffectPayload is the serialized intent for one exact-SHA status.
type commitStatusEffectPayload struct {
	Repository github.Repository
	Status     github.CommitStatus
}

// checkpointRequestJSON keeps the git adapter request independent from the
// unexported effect implementation while retaining every replay input.
type checkpointRequestJSON struct {
	RunID        string
	WorktreePath string
	ParentSHA    string
	Kind         string
	Paths        []string
	Message      string
}

// checkpointEffectPayload is the complete restart intent for a checkpoint and
// its immediately following run projection.
type checkpointEffectPayload struct {
	Request    checkpointRequestJSON
	Repository github.Repository
	Issue      github.Issue
	Previous   store.Run
	Next       store.Run
}

// pushEffectPayload is the serialized intent for one branch push.
type pushEffectPayload struct {
	Request     pushRequestJSON
	ExpectedSHA string
}

// pushRequestJSON keeps the git adapter request independent from the effect
// journal package boundary.
type pushRequestJSON struct {
	WorktreePath string
	Branch       string
}

// pullRequestEffectPayload is the serialized intent for one draft PR mutation.
type pullRequestEffectPayload struct {
	Repository github.Repository
	Number     int
	Request    github.PullRequestRequest
	PersistRun bool
	Issue      github.Issue
	Previous   store.Run
	Next       store.Run
}

// workerLaunchEffectPayload is the serialized intent for one worker launch or
// reuse. The request type itself is already a portable worker seam.
type workerLaunchEffectPayload struct {
	Request worker.StartRequest
}

// harnessResumeEffectPayload is the durable intent for one native-session
// continuation. The target resume count lets recovery recognize a reservation
// that crossed the harness boundary before the invocation row was finalized.
type harnessResumeEffectPayload struct {
	Request           harness.StartRequest
	Invocation        store.Invocation
	TargetResumeCount int
	// Manual marks an operator-requested resume. It does not consume the
	// automatic recovery ceiling.
	Manual bool
}

// resultAcceptanceEffectPayload is the complete durable intent for accepting
// a validated structured report.
type resultAcceptanceEffectPayload struct {
	Repository github.Repository
	Issue      github.Issue
	Session    harness.Session
	Invocation store.Invocation
	// WorkerID identifies the worker that owns this invocation. Empty legacy
	// payloads intentionally fall back to the run-scoped worker identity.
	WorkerID       string
	Previous       store.Run
	Next           store.Run
	StopWorker     bool
	AcceptedReport string // JSON-encoded report.Report snapshot for deterministic replay
}

// checkpointRequest converts a serialized checkpoint request back to the Git
// adapter's portable input type.
func checkpointRequest(value checkpointRequestJSON) gitadapter.CheckpointRequest {
	return gitadapter.CheckpointRequest{
		RunID:        value.RunID,
		WorktreePath: value.WorktreePath,
		ParentSHA:    value.ParentSHA,
		Kind:         gitadapter.CheckpointKind(value.Kind),
		Paths:        append([]string(nil), value.Paths...),
		Message:      value.Message,
	}
}
