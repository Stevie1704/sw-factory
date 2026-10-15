package factory

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/workflow"
	"slices"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// Inspection page bounds keep CLI responses and store queries finite.
const (
	DefaultRunPageSize = 20
	MaxRunPageSize     = 100
)

// ErrInspectionBounds reports an invalid pagination window.
var ErrInspectionBounds = errors.New("invalid inspection bounds")

// RunIdentity is the allowlisted state shared by local run projections. Free
// text, host paths, frozen packet contents, and replay payloads are excluded.
type RunIdentity struct {
	ID                string       `json:"id"`
	IssueNumber       int          `json:"issue_number"`
	Stage             store.Stage  `json:"stage"`
	Status            store.Status `json:"status"`
	Activity          RunActivity  `json:"activity"`
	Revision          int64        `json:"revision"`
	CheckpointSHA     string       `json:"checkpoint_sha,omitempty"`
	PullRequestNumber int          `json:"pull_request_number,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

// RunPage is a deterministic bounded window of persisted runs. Offsets are
// zero-based rows in updated_at descending, ID ascending order; concurrent
// updates can move rows between requests.
type RunPage struct {
	Runs       []RunIdentity `json:"runs"`
	Offset     int           `json:"offset"`
	Limit      int           `json:"limit"`
	NextOffset *int          `json:"next_offset"`
	Ordering   string        `json:"ordering"`
}

// Runs reads one page locally without creating, migrating, or repairing a store.
func (s *Service) Runs(ctx context.Context, offset, limit int) (RunPage, error) {
	if offset < 0 || limit < 1 || limit > MaxRunPageSize {
		return RunPage{}, ErrInspectionBounds
	}
	return readRunView(ctx, s, func(view runViewRead) (RunPage, error) {
		reader, ok := view.reader.(interface {
			ListRunsPage(context.Context, int, int) ([]store.Run, error)
		})
		if !ok {
			return RunPage{}, errors.New("operational store does not support run pagination")
		}
		runs, err := reader.ListRunsPage(ctx, offset, limit+1)
		if err != nil {
			return RunPage{}, err
		}
		page := RunPage{Runs: []RunIdentity{}, Offset: offset, Limit: limit, Ordering: "updated_at DESC, id ASC"}
		if len(runs) > limit {
			next := offset + limit
			page.NextOffset = &next
			runs = runs[:limit]
		}
		for _, run := range runs {
			page.Runs = append(page.Runs, projectRunIdentity(run))
		}
		return page, nil
	})
}

// projectRunIdentity selects metadata without serializing operational objects.
func projectRunIdentity(run store.Run) RunIdentity {
	return RunIdentity{ID: run.ID, IssueNumber: run.IssueNumber, Stage: run.Stage,
		Status: run.Status, Activity: RunActivityFor(run), Revision: run.Revision,
		CheckpointSHA: run.CheckpointSHA, PullRequestNumber: run.PullRequestNumber,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt}
}

// FrozenPolicyIdentity fingerprints only the frozen repository policy. Neither
// command text nor the specification packet is returned to callers.
type FrozenPolicyIdentity struct {
	State         string          `json:"state"`
	PacketVersion int             `json:"packet_version,omitempty"`
	SHA256        string          `json:"sha256,omitempty"`
	Route         workflow.Route  `json:"route,omitempty"`
	TestMode      config.TestMode `json:"test_mode,omitempty"`
}

// InvocationIdentity contains retained attempt identities and observed status.
type InvocationIdentity struct {
	ID            string                 `json:"id"`
	Role          string                 `json:"role"`
	Harness       string                 `json:"harness"`
	Stage         store.Stage            `json:"stage"`
	Status        store.InvocationStatus `json:"status"`
	PromptVersion string                 `json:"prompt_version"`
	CraftSHA256   string                 `json:"craft_sha256,omitempty"`
	ReviewRoundID string                 `json:"review_round_id,omitempty"`
	ReviewUnitID  string                 `json:"review_unit_id,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

// GateObservation identifies a persisted outcome without command output or
// free-text skip reasons. Ordinal identifies a gate in the frozen policy.
type GateObservation struct {
	DiagnosticCode    string            `json:"diagnostic_code,omitempty"`
	EvidenceSelectors []string          `json:"evidence_selectors,omitempty"`
	Phase             store.GatePhase   `json:"phase"`
	CheckpointSHA     string            `json:"checkpoint_sha"`
	Ordinal           int               `json:"ordinal"`
	Outcome           store.GateOutcome `json:"outcome"`
	Status            string            `json:"status"`
	Blocking          bool              `json:"blocking"`
	SetupFingerprint  string            `json:"setup_fingerprint,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// RetryBudget projects durable retry accounting, including an outstanding reservation.
type RetryBudget struct {
	State   string `json:"state"`
	Used    int    `json:"used"`
	Limit   int    `json:"limit"`
	Pending int    `json:"pending,omitempty"`
}

// EffectIdentity omits the replay payload and exposes only the reserved operation.
type EffectIdentity struct {
	ID        string                  `json:"id"`
	Kind      store.PendingEffectKind `json:"kind"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
}

// InspectionCoverage states exactly which retained records a page covers.
type InspectionCoverage struct {
	TimelineTotal   int      `json:"timeline_total"`
	EvidenceTotal   int      `json:"evidence_total"`
	Offset          int      `json:"offset"`
	Limit           int      `json:"limit"`
	InvocationTotal int      `json:"invocation_total"`
	GateTotal       int      `json:"gate_total"`
	NextOffset      *int     `json:"next_offset"`
	Limitations     []string `json:"limitations"`
}

// RunInspection is the allowlisted default run view. No operational store
// object is serialized: new store fields cannot silently expand disclosure.
type RunInspection struct {
	Timeline      []RunObservation       `json:"timeline"`
	Evidence      []EvidenceReference    `json:"evidence"`
	State         string                 `json:"state"`
	Run           RunIdentity            `json:"run"`
	Policy        FrozenPolicyIdentity   `json:"policy"`
	Checkpoints   map[string]string      `json:"checkpoints"`
	Invocations   []InvocationIdentity   `json:"invocations"`
	Gates         []GateObservation      `json:"gates"`
	RetryBudgets  map[string]RetryBudget `json:"retry_budgets,omitempty"`
	PendingEffect *EffectIdentity        `json:"pending_effect,omitempty"`
	Coverage      InspectionCoverage     `json:"coverage"`
}

// RunInspect inspects any named run, even while another run is active. A
// retained evaluation summary distinguishes cleanup from an unknown identity.
func (s *Service) RunInspect(ctx context.Context, runID string, offset, limit int) (RunInspection, error) {
	if offset < 0 || limit < 1 || limit > MaxRunPageSize {
		return RunInspection{}, ErrInspectionBounds
	}
	return readRunView(ctx, s, func(view runViewRead) (RunInspection, error) {
		detail, err := readRunDetail(ctx, view, runID, false)
		if errors.Is(err, ErrRunNotFound) {
			summary, summaryErr := view.reader.EvaluationSummary(ctx, runID)
			if summaryErr != nil {
				return RunInspection{}, summaryErr
			}
			if summary != nil {
				return RunInspection{State: "cleaned_up", Run: RunIdentity{ID: runID},
					Evidence: []EvidenceReference{}, Timeline: []RunObservation{},
					Policy:      FrozenPolicyIdentity{State: "unavailable"},
					Checkpoints: map[string]string{}, Invocations: []InvocationIdentity{}, Gates: []GateObservation{},
					Coverage: InspectionCoverage{Offset: offset, Limit: limit, Limitations: []string{"Operational records and artifacts were cleaned up; only the local evaluation summary is retained."}}}, nil
			}
		}
		if err != nil {
			return RunInspection{}, err
		}
		return projectRunInspection(detail, offset, limit), nil
	})
}

// projectRunInspection is shared by CLI inspection and the web run-detail view.
func projectRunInspection(detail RunDetail, offset, limit int) RunInspection {
	run := detail.Run
	out := RunInspection{State: "retained", Run: projectRunIdentity(run),
		Policy: FrozenPolicyIdentity{State: "unavailable"},
		Checkpoints: map[string]string{"base": run.BaseCheckpointSHA, "test": run.TestCheckpointSHA,
			"accepted_implementation": run.AcceptedImplementationCheckpointSHA, "current": run.CheckpointSHA, "merge": run.MergeCommitSHA},
		Invocations: []InvocationIdentity{}, Gates: []GateObservation{},
		RetryBudgets: map[string]RetryBudget{
			"check_repair":  {Used: run.CheckRepairAttempts, Limit: run.CheckRepairBudget, Pending: run.CheckRepairPendingAttempt},
			"review_repair": {Used: run.ReviewRepairAttempts, Limit: run.ReviewRepairBudget, Pending: run.ReviewRepairPendingAttempt},
			"test_revision": {Used: run.TestRevisionAttempts, Limit: run.TestRevisionBudget}},
		Coverage: InspectionCoverage{Offset: offset, Limit: limit, InvocationTotal: len(detail.Invocations),
			Limitations: []string{"Invocation records retain creation and latest status, not every transition.",
				"Gate rows retain the latest outcome per phase/checkpoint/ordinal; repeated attempts at the same checkpoint overwrite earlier outcomes, and packet changes can remove rows.",
				"Phase diagnostics retain only the latest failure file and may describe a different checkpoint."}}}
	testRevisionDisabled := false
	if packet, err := decodeSpecificationPacket(run.SpecificationPacket); err == nil {
		testRevisionDisabled = packet.RepositoryConfig.TestPolicy.Mode == config.TestModeAdvisory && !independentTestStageDeclared(packet)
		for name, frozen := range map[string]int{"check_repair": packet.RepositoryConfig.RetryLimits.CheckRepair, "review_repair": packet.RepositoryConfig.RetryLimits.ReviewRepair, "test_revision": testRevisionBudgetForPacket(packet)} {
			budget := out.RetryBudgets[name]
			if budget.Limit == 0 {
				budget.Limit = frozen
			}
			out.RetryBudgets[name] = budget
		}
		if data, err := json.Marshal(packet.RepositoryConfig); err == nil {
			out.Policy = FrozenPolicyIdentity{State: "available", PacketVersion: packet.Version,
				SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Route: statusRouteForRun(run), TestMode: packet.RepositoryConfig.TestPolicy.Mode}
		}
	}
	for name, budget := range out.RetryBudgets {
		budget.State = "unavailable"
		if budget.Limit > 0 {
			budget.State = "available"
		} else if name == "test_revision" && testRevisionDisabled {
			budget.State = "not_applicable"
		}
		out.RetryBudgets[name] = budget
	}
	for _, inv := range pageWindow(detail.Invocations, offset, limit) {
		out.Invocations = append(out.Invocations, InvocationIdentity{ID: inv.ID, Role: inv.Role, Harness: inv.Harness,
			Stage: inv.Stage, Status: inv.Status, PromptVersion: inv.PromptVersion, CraftSHA256: inv.PromptCraftSHA256,
			ReviewRoundID: inv.ReviewRoundID, ReviewUnitID: inv.ReviewUnitID, CreatedAt: inv.CreatedAt, UpdatedAt: inv.UpdatedAt})
	}
	gates := []GateObservation{}
	for _, group := range detail.Gates {
		for _, g := range group.Results {
			gates = append(gates, GateObservation{Phase: g.Phase, CheckpointSHA: g.CheckpointSHA, Ordinal: g.Ordinal,
				Outcome: g.Outcome, Status: g.Status, Blocking: g.Blocking, SetupFingerprint: g.SetupFingerprint, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt})
		}
	}
	out.Coverage.GateTotal = len(gates)
	out.Gates = pageWindow(gates, offset, limit)
	for i := range out.Gates {
		g := &out.Gates[i]
		switch g.Outcome {
		case store.GateOutcomeFailed:
			g.DiagnosticCode = "run.gate.failed"
		case store.GateOutcomeError:
			g.DiagnosticCode = "run.gate.error"
		case store.GateOutcomeSetupFailed:
			g.DiagnosticCode = "run.gate.setup_failed"
		}
		if g.DiagnosticCode != "" {
			g.EvidenceSelectors = []string{"gate-failure." + string(g.Phase), "gate-output." + g.CheckpointSHA}
		}
	}
	if effect := detail.PendingEffect; effect != nil {
		out.PendingEffect = &EffectIdentity{ID: effect.ID, Kind: effect.Kind, CreatedAt: effect.CreatedAt, UpdatedAt: effect.UpdatedAt}
	}
	timeline := retainedChronology(detail)
	out.Timeline = pageWindow(timeline, offset, limit)
	out.Coverage.TimelineTotal = len(timeline)
	out.Evidence, out.Coverage.EvidenceTotal = evidenceReferences(detail, offset, limit)
	if max(len(detail.Invocations), len(gates), len(timeline), out.Coverage.EvidenceTotal)-offset > limit {
		next := offset + limit
		out.Coverage.NextOffset = &next
	}
	return out
}

// pageWindow selects a non-nil bounded window without overflowing offset+limit.
func pageWindow[T any](items []T, offset, limit int) []T {
	start := min(offset, len(items))
	return items[start : start+min(limit, len(items)-start)]
}

// RunObservation is a retained fact, not a reconstructed lifecycle event.
// Creation never claims an initial status; observed status is the latest row.
type RunObservation struct {
	At            time.Time       `json:"at"`
	Kind          string          `json:"kind"`
	InvocationID  string          `json:"invocation_id,omitempty"`
	Phase         store.GatePhase `json:"phase,omitempty"`
	CheckpointSHA string          `json:"checkpoint_sha,omitempty"`
	Ordinal       int             `json:"ordinal,omitempty"`
	Outcome       string          `json:"outcome,omitempty"`
}

// retainedChronology orders persisted facts by timestamp and then kind,
// invocation ID, phase, checkpoint and ordinal so ties remain deterministic.
func retainedChronology(detail RunDetail) []RunObservation {
	observations := []RunObservation{}
	for _, inv := range detail.Invocations {
		observations = append(observations, RunObservation{At: inv.CreatedAt, Kind: "invocation.created", InvocationID: inv.ID},
			RunObservation{At: inv.UpdatedAt, Kind: "invocation.observed", InvocationID: inv.ID, Outcome: string(inv.Status)})
	}
	for _, group := range detail.Gates {
		for _, gate := range group.Results {
			observations = append(observations, RunObservation{At: gate.UpdatedAt, Kind: "gate.observed", Phase: gate.Phase,
				CheckpointSHA: gate.CheckpointSHA, Ordinal: gate.Ordinal, Outcome: string(gate.Outcome)})
		}
	}
	slices.SortFunc(observations, func(a, b RunObservation) int {
		if order := a.At.Compare(b.At); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Kind, b.Kind); order != 0 {
			return order
		}
		if order := cmp.Compare(a.InvocationID, b.InvocationID); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Phase, b.Phase); order != 0 {
			return order
		}
		if order := cmp.Compare(a.CheckpointSHA, b.CheckpointSHA); order != 0 {
			return order
		}
		return cmp.Compare(a.Ordinal, b.Ordinal)
	})
	return observations
}
