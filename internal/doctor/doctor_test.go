package doctor_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// TestRunExecutesEveryCheckInDeclaredOrder verifies one failed check does not
// prevent later subsystem checks from reporting their own result.
func TestRunExecutesEveryCheckInDeclaredOrder(t *testing.T) {
	var executed []string
	report := doctor.Run(context.Background(),
		func(context.Context) doctor.Result {
			executed = append(executed, "configuration")
			return doctor.Failure(doctor.Condition{Code: "config.host.invalid", Origin: doctor.OriginEnvironment}, "configuration", "host configuration is invalid", "repair the host configuration")
		},
		func(context.Context) doctor.Result {
			executed = append(executed, "docker")
			return doctor.Success("worker.docker.ready", "docker")
		},
		func(context.Context) doctor.Result {
			executed = append(executed, "authentication")
			return doctor.Warning(doctor.Condition{Code: "harness.authentication.source_missing", Origin: doctor.OriginEnvironment}, "authentication", "no host credential source is configured", "authenticate the harness interactively")
		},
	)

	if want := []string{"configuration", "docker", "authentication"}; !reflect.DeepEqual(executed, want) {
		t.Fatalf("executed checks = %#v, want %#v", executed, want)
	}
	if report.Ready() {
		t.Fatal("report with a failure is ready")
	}
	if got := len(report.Failures()); got != 1 {
		t.Fatalf("failure count = %d, want one", got)
	}
	if got := len(report.Warnings()); got != 1 {
		t.Fatalf("warning count = %d, want one", got)
	}
}

// TestResultConstructorsPreserveActionableFailureDetails verifies a rendered
// result contains the subsystem, problem, and corrective action fields.
func TestResultConstructorsPreserveActionableFailureDetails(t *testing.T) {
	result := doctor.Failure(doctor.Condition{Code: "github.permissions.insufficient", Origin: doctor.OriginEnvironment}, "GitHub permissions", "repository write permission is unavailable", "grant repository issue, contents, pull-request, and status permissions")
	if result.Name != "GitHub permissions" || result.Status != doctor.StatusFailed {
		t.Fatalf("result identity = %#v, want failed GitHub permissions", result)
	}
	if result.Problem == "" || result.Action == "" {
		t.Fatalf("result = %#v, want problem and action", result)
	}
}

// dockerUnavailable is a representative classified condition for the tests.
var dockerUnavailable = doctor.Condition{
	Code:   "worker.docker.unavailable",
	Origin: doctor.OriginEnvironment,
	Effect: doctor.EffectLocalMutation,
	Topic:  "setup",
}

// TestRunKeepsTheStableCodeAndClassificationOfEachResult verifies the code,
// subsystem, and likely origin come from the producer's condition and not
// from the rendered wording.
func TestRunKeepsTheStableCodeAndClassificationOfEachResult(t *testing.T) {
	report := doctor.Run(context.Background(),
		func(context.Context) doctor.Result {
			return doctor.Failure(dockerUnavailable, "docker daemon", "any wording", "any action")
		},
		func(context.Context) doctor.Result { return doctor.Success("store.sqlite.ready", "SQLite") },
	)

	failed := report.Results[0]
	if failed.Code != "worker.docker.unavailable" || failed.Subsystem() != "worker" || failed.Origin != doctor.OriginEnvironment {
		t.Fatalf("failed result = %#v, want the producer's classified condition", failed)
	}
	if passed := report.Results[1]; passed.Code != "store.sqlite.ready" || passed.Subsystem() != "store" {
		t.Fatalf("passed result = %#v, want its stable code", passed)
	}
}

// TestRunReportsAnUnclassifiedFailureAsUnknownOrigin verifies a failure
// without a code is never presented as a classified or Factory-caused
// condition.
func TestRunReportsAnUnclassifiedFailureAsUnknownOrigin(t *testing.T) {
	report := doctor.Run(context.Background(), func(context.Context) doctor.Result {
		return doctor.Result{Name: "legacy", Status: doctor.StatusFailed, Problem: "it failed", Action: "inspect it"}
	})

	result := report.Results[0]
	if result.Code != "doctor.unclassified" || result.Origin != doctor.OriginUnknown {
		t.Fatalf("result = %#v, want doctor.unclassified with unknown origin", result)
	}
	if result.Problem != "it failed" || result.Action != "inspect it" {
		t.Fatalf("result = %#v, want the original problem and action", result)
	}
}

// TestRunAttributesAMissingCheckToTheFactory verifies a composition defect is
// classified as a Factory origin and does not count as an observation.
func TestRunAttributesAMissingCheckToTheFactory(t *testing.T) {
	report := doctor.Run(context.Background(), nil)

	result := report.Results[0]
	if result.Status != doctor.StatusFailed || result.Origin != doctor.OriginFactory || result.Code == "" {
		t.Fatalf("result = %#v, want a classified Factory failure", result)
	}
	if report.Complete() {
		t.Fatal("report with a missing check is complete")
	}
}

// TestReportCompletenessSeparatesUnobservedFromHealthy verifies a skipped or
// unobservable check never makes the collection look complete, while a
// skipped check alone does not block startup.
func TestReportCompletenessSeparatesUnobservedFromHealthy(t *testing.T) {
	healthy := doctor.Run(context.Background(), func(context.Context) doctor.Result { return doctor.Success("store.sqlite.ready", "SQLite") })
	if !healthy.Complete() || !healthy.Ready() {
		t.Fatalf("healthy report complete=%t ready=%t, want both", healthy.Complete(), healthy.Ready())
	}

	skipped := doctor.Run(context.Background(), func(context.Context) doctor.Result {
		return doctor.Skipped("config.caches.not_checked", "caches", "the configuration is unavailable")
	})
	if skipped.Complete() {
		t.Fatal("report with a skipped check is complete")
	}
	if !skipped.Ready() {
		t.Fatal("a skipped check alone blocks startup")
	}
	if got := skipped.Results[0]; got.Status != doctor.StatusSkipped || got.Problem == "" {
		t.Fatalf("skipped result = %#v, want skipped status with its reason", got)
	}

	unobserved := doctor.Condition{Code: "worker.docker.adapter_missing", Origin: doctor.OriginFactory, Unobserved: true}
	adapterMissing := doctor.Run(context.Background(), func(context.Context) doctor.Result {
		return doctor.Failure(unobserved, "docker daemon", "the adapter is unavailable", "configure it")
	})
	if adapterMissing.Complete() {
		t.Fatal("report with an unobserved failure is complete")
	}
}
