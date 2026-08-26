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
			return doctor.Failure("configuration", "host configuration is invalid", "repair the host configuration")
		},
		func(context.Context) doctor.Result {
			executed = append(executed, "docker")
			return doctor.Success("docker")
		},
		func(context.Context) doctor.Result {
			executed = append(executed, "authentication")
			return doctor.Warning("authentication", "no host credential source is configured", "authenticate the harness interactively")
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
	result := doctor.Failure("GitHub permissions", "repository write permission is unavailable", "grant repository issue, contents, pull-request, and status permissions")
	if result.Name != "GitHub permissions" || result.Status != doctor.StatusFailed {
		t.Fatalf("result identity = %#v, want failed GitHub permissions", result)
	}
	if result.Problem == "" || result.Action == "" {
		t.Fatalf("result = %#v, want problem and action", result)
	}
}
