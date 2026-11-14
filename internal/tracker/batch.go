package tracker

import "context"

// PlanPreparer reserves durable local identities as an explicit coordinator
// operation before read-only PreflightPlan. It never approves or publishes.
type PlanPreparer interface {
	PreparePlan(context.Context, Repository, PlanGraph) error
}

// PlanBatchPublisher is the narrow atomic Git-batch extension of PlanPublisher.
// Find observes all effects; Publish adopts matching effects before continuing.
// A partial result is evidence only and must not release any item for execution.
type PlanBatchPublisher interface {
	PublishPlanBatch(context.Context, Repository, PlanPreview, ...PlanBatchRecorder) (PlanBatchResult, error)
	FindPlanBatch(context.Context, Repository, PlanPreview) (PlanBatchResult, bool, error)
}

// PlanBatchResult is durable publication evidence, independent of authorization.
type PlanBatchResult struct {
	CommitAttempted      bool                   `json:"commit_attempted,omitempty"`
	PushAttempted        bool                   `json:"push_attempted,omitempty"`
	PullRequestAttempted bool                   `json:"pull_request_attempted,omitempty"`
	CommitSHA            string                 `json:"commit_sha"`
	Branch               string                 `json:"branch"`
	BaseSHA              string                 `json:"base_sha"`
	Pushed               bool                   `json:"pushed"`
	PullRequestNumber    int                    `json:"pull_request_number,omitempty"`
	PullRequestURL       string                 `json:"pull_request_url,omitempty"`
	Complete             bool                   `json:"complete"`
	Items                map[string]PlanItemRef `json:"items,omitempty"`
}

// PlanBatchRecorder persists each subeffect's intent/evidence before the
// adapter crosses its Git or code-host boundary. Failure stops publication.
type PlanBatchRecorder func(PlanBatchResult) error
