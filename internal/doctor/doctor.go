// Package doctor contains the neutral startup-diagnosis result and composition
// seam. Subsystem packages contribute checks; this package only runs them and
// reports whether any check blocks a factory run.
package doctor

import (
	"context"
	"strings"
)

// Status is the outcome class of one startup check.
type Status string

const (
	// StatusPassed means the check found the subsystem ready.
	StatusPassed Status = "passed"
	// StatusWarning means the check found an operator action that is not a
	// blocking prerequisite for the current configuration.
	StatusWarning Status = "warning"
	// StatusFailed means the subsystem is not safe to use for a factory run.
	StatusFailed Status = "failed"
	// StatusSkipped means the check did not observe its subject because a
	// prerequisite another check reports was unavailable. It does not block
	// startup by itself and it never counts as healthy.
	StatusSkipped Status = "skipped"
)

// Origin is the likely origin class of a diagnosed condition. It is a
// classification from the check's own evidence, not proof of a cause.
type Origin string

const (
	// OriginRepository means the repository or its checked-in policy is the
	// likely origin, for example an invalid factory.yaml or a failed gate.
	OriginRepository Origin = "repository"
	// OriginEnvironment means the host environment is the likely origin, for
	// example a stopped Docker daemon or an expired credential.
	OriginEnvironment Origin = "environment"
	// OriginFactory means Factory itself is the likely origin, for example a
	// missing diagnosis adapter or an invalid check result.
	OriginFactory Origin = "factory"
	// OriginUnknown means the evidence does not identify the origin.
	OriginUnknown Origin = "unknown"
)

// Effect classifies what the suggested action changes when an operator
// performs it.
type Effect string

const (
	// EffectInspection means the action only reads state.
	EffectInspection Effect = "inspection"
	// EffectLocalMutation means the action changes host-local state, such as
	// a configuration file, a credential, or a local service.
	EffectLocalMutation Effect = "local_mutation"
	// EffectExternalMutation means the action changes state outside the host,
	// such as tracker permissions, labels, or a pushed repository change.
	EffectExternalMutation Effect = "external_mutation"
)

// Condition is the stable, wording-independent classification of one
// diagnosed condition. Subsystems declare each condition once and pass it to
// the result constructors, so changing a problem text never changes a code.
type Condition struct {
	// Code is the stable identifier "<subsystem>.<check>.<condition>".
	Code string
	// Origin is the likely origin of the condition.
	Origin Origin
	// Effect classifies the suggested corrective action.
	Effect Effect
	// Topic names the `factory guide` topic that explains the action.
	Topic string
	// Preconditions are bounded statements that must be true before the
	// suggested action is safe to perform.
	Preconditions []string
	// Unobserved marks a condition in which the check could not observe its
	// subject, such as an unavailable diagnosis adapter.
	Unobserved bool
}

// Fact is one bounded observation that supports a diagnosis. Values never
// contain command output or secrets.
type Fact struct {
	// Name identifies the observed property.
	Name string
	// Value is the observed value.
	Value string
}

// Result is one bounded, operator-facing startup diagnosis. Problem and
// Action are deliberately separate so every failure explains both what is
// wrong and how to correct it.
type Result struct {
	// Condition is the stable classification of the result.
	Condition
	// Name identifies the subsystem and check being diagnosed.
	Name string
	// Status is passed, warning, failed, or skipped.
	Status Status
	// Problem describes the observed issue without command output or secrets.
	Problem string
	// Action describes the corrective operator action.
	Action string
	// Detail is optional bounded information a passed check reports, such
	// as the effective value of a setting.
	Detail string
	// Facts are bounded observations that support the diagnosis.
	Facts []Fact
}

// Check is a subsystem-owned startup check. Checks must return a bounded
// Result rather than exposing raw process output to the coordinator or CLI.
type Check func(context.Context) Result

// Report contains every check result in the order supplied to Run.
type Report struct {
	// Results contains successful, warning, failed, and skipped checks.
	Results []Result
}

// unclassified replaces the missing condition of a result whose producer
// did not classify it. Its origin stays unknown on purpose.
var unclassified = Condition{Code: "doctor.unclassified", Origin: OriginUnknown, Effect: EffectInspection, Topic: "debug"}

// checkMissing reports a check that the composition did not configure.
var checkMissing = Condition{Code: "doctor.check.missing", Origin: OriginFactory, Effect: EffectInspection, Topic: "reporting", Unobserved: true}

// resultInvalid reports a check that returned a malformed result.
var resultInvalid = Condition{Code: "doctor.result.invalid", Origin: OriginFactory, Effect: EffectInspection, Topic: "reporting", Unobserved: true}

// AdapterMissing classifies a check whose diagnosis adapter is not
// configured or not capable. The check observed nothing, and the likely
// origin is Factory's own composition.
func AdapterMissing(code string) Condition {
	return Condition{Code: code, Origin: OriginFactory, Effect: EffectInspection, Topic: "reporting", Unobserved: true}
}

// Success creates a passing result for one named subsystem check.
func Success(code, name string) Result {
	return Result{Condition: Condition{Code: code}, Name: name, Status: StatusPassed}
}

// Warning creates a non-blocking result with an operator-visible action.
func Warning(condition Condition, name, problem, action string) Result {
	return Result{Condition: condition, Name: name, Status: StatusWarning, Problem: problem, Action: action}
}

// Failure creates a blocking result with an operator-visible action.
func Failure(condition Condition, name, problem, action string) Result {
	return Result{Condition: condition, Name: name, Status: StatusFailed, Problem: problem, Action: action}
}

// Skipped creates a result for a check that could not observe its subject
// because a prerequisite is unavailable. The reason names that prerequisite.
func Skipped(code, name, reason string) Result {
	return Result{Condition: Condition{Code: code, Origin: OriginUnknown, Unobserved: true}, Name: name, Status: StatusSkipped, Problem: reason}
}

// WithFacts returns the result with the supplied supporting observations.
func (r Result) WithFacts(facts ...Fact) Result {
	r.Facts = append(append([]Fact(nil), r.Facts...), facts...)
	return r
}

// Subsystem returns the first segment of the stable code.
func (r Result) Subsystem() string {
	subsystem, _, _ := strings.Cut(r.Code, ".")
	return subsystem
}

// BlocksStartup reports whether this result prevents a factory run.
func (r Result) BlocksStartup() bool {
	return r.Status == StatusFailed
}

// Run executes every supplied check, including checks after a failure, and
// preserves the deterministic contribution order for CLI output.
func Run(ctx context.Context, checks ...Check) Report {
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		if check == nil {
			results = append(results, Failure(checkMissing, "diagnosis", "a startup check was not configured", "configure the missing subsystem check"))
			continue
		}
		results = append(results, normalize(check(ctx)))
	}
	return Report{Results: results}
}

// normalize replaces an invalid or incomplete result with a bounded
// diagnosis, and keeps an unclassified result explicitly unknown.
func normalize(result Result) Result {
	if result.Name == "" {
		result.Name = "diagnosis"
	}
	switch result.Status {
	case StatusPassed, StatusWarning, StatusFailed:
		if result.Status != StatusPassed && (result.Problem == "" || result.Action == "") {
			return Failure(resultInvalid, result.Name, "the startup check did not provide a complete diagnosis", "repair the check implementation before starting the factory")
		}
	case StatusSkipped:
		if result.Problem == "" {
			return Failure(resultInvalid, result.Name, "the startup check did not provide a complete diagnosis", "repair the check implementation before starting the factory")
		}
	default:
		return Failure(resultInvalid, result.Name, "the startup check returned an invalid result", "repair the check implementation before starting the factory")
	}
	if result.Code == "" {
		result.Condition = unclassified
	}
	if result.Origin == "" && result.Status != StatusPassed {
		result.Origin = OriginUnknown
	}
	return result
}

// Ready reports whether no check blocks startup. Warnings are intentionally
// non-blocking and remain visible in the report.
func (r Report) Ready() bool {
	for _, result := range r.Results {
		if result.BlocksStartup() {
			return false
		}
	}
	return true
}

// Complete reports whether every check observed its subject. A skipped or
// unobservable check makes the collection incomplete, never healthy.
func (r Report) Complete() bool {
	for _, result := range r.Results {
		if result.Unobserved {
			return false
		}
	}
	return true
}

// Failures returns all blocking results without changing their order.
func (r Report) Failures() []Result {
	return resultsWithStatus(r.Results, StatusFailed)
}

// Warnings returns all non-blocking warning results without changing their
// order.
func (r Report) Warnings() []Result {
	return resultsWithStatus(r.Results, StatusWarning)
}

// resultsWithStatus selects one result status while preserving report order.
func resultsWithStatus(results []Result, status Status) []Result {
	selected := make([]Result, 0)
	for _, result := range results {
		if result.Status == status {
			selected = append(selected, result)
		}
	}
	return selected
}
