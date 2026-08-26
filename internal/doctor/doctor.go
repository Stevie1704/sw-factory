// Package doctor contains the neutral startup-diagnosis result and composition
// seam. Subsystem packages contribute checks; this package only runs them and
// reports whether any check blocks a factory run.
package doctor

import "context"

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
)

// Result is one bounded, operator-facing startup diagnosis. Problem and
// Action are deliberately separate so every failure explains both what is
// wrong and how to correct it.
type Result struct {
	// Name identifies the subsystem and check being diagnosed.
	Name string
	// Status is passed, warning, or failed.
	Status Status
	// Problem describes the observed issue without command output or secrets.
	Problem string
	// Action describes the corrective operator action.
	Action string
}

// Check is a subsystem-owned startup check. Checks must return a bounded
// Result rather than exposing raw process output to the coordinator or CLI.
type Check func(context.Context) Result

// Report contains every check result in the order supplied to Run.
type Report struct {
	// Results contains successful, warning, and failed checks.
	Results []Result
}

// Success creates a passing result for one named subsystem check.
func Success(name string) Result {
	return Result{Name: name, Status: StatusPassed}
}

// Warning creates a non-blocking result with an operator-visible action.
func Warning(name, problem, action string) Result {
	return Result{Name: name, Status: StatusWarning, Problem: problem, Action: action}
}

// Failure creates a blocking result with an operator-visible action.
func Failure(name, problem, action string) Result {
	return Result{Name: name, Status: StatusFailed, Problem: problem, Action: action}
}

// Run executes every supplied check, including checks after a failure, and
// preserves the deterministic contribution order for CLI output.
func Run(ctx context.Context, checks ...Check) Report {
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		if check == nil {
			results = append(results, Failure("diagnosis", "a startup check was not configured", "configure the missing subsystem check"))
			continue
		}
		result := check(ctx)
		if result.Name == "" {
			result.Name = "diagnosis"
		}
		if result.Status != StatusPassed && result.Status != StatusWarning && result.Status != StatusFailed {
			result = Failure(result.Name, "the startup check returned an invalid result", "repair the check implementation before starting the factory")
		} else if result.Status != StatusPassed && (result.Problem == "" || result.Action == "") {
			result.Problem = "the startup check did not provide a complete diagnosis"
			result.Action = "repair the check implementation before starting the factory"
		}
		results = append(results, result)
	}
	return Report{Results: results}
}

// Ready reports whether no check blocks startup. Warnings are intentionally
// non-blocking and remain visible in the report.
func (r Report) Ready() bool {
	for _, result := range r.Results {
		if result.Status == StatusFailed {
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
