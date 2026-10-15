package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Stevie1704/sw-factory/internal/version"
)

// ReportSchema is the version of both the results file and the report.
const ReportSchema = 1

// Evidence kinds. A deterministic result comes from the CI suite with fake
// adapters; only a live result is evidence that an agent can use the release.
const (
	EvidenceLive          = "live"
	EvidenceDeterministic = "deterministic"
)

// Outcomes. They stay distinct: a skipped or blocked scenario is never a pass.
const (
	OutcomePass    = "pass"
	OutcomeFail    = "fail"
	OutcomeBlocked = "blocked"
	OutcomeNotRun  = "not-run"
)

// outcomes lists every outcome in report order.
var outcomes = []string{OutcomePass, OutcomeFail, OutcomeBlocked, OutcomeNotRun}

// notRecorded is the reason of a scenario that the results file omits.
const notRecorded = "not recorded in this run"

// Results is the file that an operator agent records during an acceptance
// run. It holds observations only; the report adds the release identity.
type Results struct {
	SchemaVersion   int       `json:"schema_version"`
	HostHarness     *Harness  `json:"host_harness"`
	WorkerDigest    string    `json:"worker_digest"`
	WorkerHarnesses []Harness `json:"worker_harnesses"`
	Results         []Result  `json:"results"`
}

// Harness is an observed harness and its version.
type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Result is the recorded outcome of one scenario for one evidence kind.
type Result struct {
	Scenario string `json:"scenario"`
	Evidence string `json:"evidence"`
	Outcome
}

// Outcome is what one evidence kind observed for one scenario.
type Outcome struct {
	Outcome         string         `json:"outcome"`
	Reason          string         `json:"reason,omitempty"`
	DurationSeconds *int           `json:"duration_seconds,omitempty"`
	Interventions   []Intervention `json:"interventions,omitempty"`
	Workarounds     []Workaround   `json:"workarounds,omitempty"`
	Usage           *Usage         `json:"usage,omitempty"`
}

// Intervention is one human step the agent needed, with its cause.
type Intervention struct {
	Cause   string `json:"cause"`
	Summary string `json:"summary"`
}

// Workaround is one action outside the supported interfaces.
type Workaround struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

// Usage is the measured model usage and cost of a live result, or the reason
// it was not measured.
type Usage struct {
	Measured     bool     `json:"measured"`
	Reason       string   `json:"reason,omitempty"`
	InputTokens  *int     `json:"input_tokens,omitempty"`
	OutputTokens *int     `json:"output_tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
}

// Report is the release evidence report of one acceptance run.
type Report struct {
	SchemaVersion         int              `json:"schema_version"`
	ManifestSchemaVersion int              `json:"manifest_schema_version"`
	GeneratedAt           time.Time        `json:"generated_at"`
	Release               version.Identity `json:"release"`
	HostHarness           *Harness         `json:"host_harness"`
	WorkerDigest          string           `json:"worker_digest"`
	WorkerHarnesses       []Harness        `json:"worker_harnesses"`
	Scenarios             []ScenarioReport `json:"scenarios"`
	Summary               Summary          `json:"summary"`
	InterventionsByCause  map[string]int   `json:"interventions_by_cause"`
	DurationSeconds       int              `json:"duration_seconds"`
}

// ScenarioReport is the live and deterministic outcome of one scenario.
type ScenarioReport struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Live          Outcome `json:"live"`
	Deterministic Outcome `json:"deterministic"`
	Gaps          []Gap   `json:"gaps"`
}

// Summary counts the scenarios per outcome for each evidence kind.
type Summary struct {
	Live          map[string]int `json:"live"`
	Deterministic map[string]int `json:"deterministic"`
}

// DecodeResults reads one results file and refuses unknown fields, so a
// misspelled field cannot drop an observation silently.
func DecodeResults(source io.Reader) (Results, error) {
	decoder := json.NewDecoder(source)
	decoder.DisallowUnknownFields()
	var results Results
	if err := decoder.Decode(&results); err != nil {
		return Results{}, fmt.Errorf("decode acceptance results: %w", err)
	}
	if decoder.More() {
		return Results{}, errors.New("decode acceptance results: more than one JSON document")
	}
	return results, nil
}

// BuildReport combines the manifest, the recorded results, and the identity
// of the running build into a report. Every manifest scenario appears; an
// evidence kind without a recorded result is not run.
func BuildReport(manifest Manifest, results Results, release version.Identity, now time.Time) (Report, error) {
	if results.SchemaVersion != ReportSchema {
		return Report{}, fmt.Errorf("results schema_version %d, want %d", results.SchemaVersion, ReportSchema)
	}
	if err := results.validate(); err != nil {
		return Report{}, fmt.Errorf("acceptance results: %w", err)
	}
	recorded, err := indexResults(manifest, results.Results)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion:         ReportSchema,
		ManifestSchemaVersion: manifest.SchemaVersion,
		GeneratedAt:           now.UTC(),
		Release:               release,
		HostHarness:           results.HostHarness,
		WorkerDigest:          results.WorkerDigest,
		WorkerHarnesses:       results.WorkerHarnesses,
		Summary:               Summary{Live: zeroCounts(), Deterministic: zeroCounts()},
		InterventionsByCause:  map[string]int{},
	}
	if report.WorkerHarnesses == nil {
		report.WorkerHarnesses = []Harness{}
	}
	for _, scenario := range manifest.Scenarios {
		entry := ScenarioReport{
			ID:            scenario.ID,
			Title:         scenario.Title,
			Live:          recordedOutcome(recorded, scenario.ID, EvidenceLive),
			Deterministic: recordedOutcome(recorded, scenario.ID, EvidenceDeterministic),
			Gaps:          scenario.Gaps,
		}
		report.add(entry)
	}
	return report, nil
}

// add appends one scenario and accumulates its counts, interventions, and
// duration.
func (r *Report) add(entry ScenarioReport) {
	r.Scenarios = append(r.Scenarios, entry)
	r.Summary.Live[entry.Live.Outcome]++
	r.Summary.Deterministic[entry.Deterministic.Outcome]++
	for _, outcome := range []Outcome{entry.Live, entry.Deterministic} {
		for _, intervention := range outcome.Interventions {
			r.InterventionsByCause[intervention.Cause]++
		}
		if outcome.DurationSeconds != nil {
			r.DurationSeconds += *outcome.DurationSeconds
		}
	}
}

// resultKey identifies one recorded result.
type resultKey struct {
	scenario string
	evidence string
}

// indexResults validates each result and indexes it by scenario and evidence.
func indexResults(manifest Manifest, results []Result) (map[resultKey]Outcome, error) {
	recorded := map[resultKey]Outcome{}
	for _, result := range results {
		if _, ok := manifest.Scenario(result.Scenario); !ok {
			return nil, fmt.Errorf("result names unknown scenario %q", result.Scenario)
		}
		key := resultKey{scenario: result.Scenario, evidence: result.Evidence}
		if _, ok := recorded[key]; ok {
			return nil, fmt.Errorf("scenario %q has more than one %s result", result.Scenario, result.Evidence)
		}
		recorded[key] = result.Outcome
	}
	return recorded, nil
}

// recordedOutcome returns the recorded outcome, or not run when none exists.
func recordedOutcome(recorded map[resultKey]Outcome, scenario, evidence string) Outcome {
	if outcome, ok := recorded[resultKey{scenario: scenario, evidence: evidence}]; ok {
		return outcome
	}
	return Outcome{Outcome: OutcomeNotRun, Reason: notRecorded}
}

// zeroCounts returns a count of zero for every outcome, so a reader sees an
// explicit zero rather than a missing key.
func zeroCounts() map[string]int {
	counts := make(map[string]int, len(outcomes))
	for _, outcome := range outcomes {
		counts[outcome] = 0
	}
	return counts
}
