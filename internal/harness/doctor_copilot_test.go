package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/harness"
)

// copilotDoctorPolicy selects Copilot for implementation and Codex for test.
func copilotDoctorPolicy() *config.RepositoryConfig {
	return &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{
		"test": config.HarnessCodex, "implementation": config.HarnessCopilot,
	}}
}

// copilotStartupReport runs the harness startup checks for policy and
// authentication against a fake worker that proves every harness.
func copilotStartupReport(t *testing.T, policy *config.RepositoryConfig, authentication config.AuthenticationConfig, checker *harnessDoctorChecker) doctor.Report {
	t.Helper()
	return doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
		Policy:                policy,
		Authentication:        authentication,
		Image:                 probedImage,
		Checker:               checker,
		AuthenticationChecker: checker,
		SkillChecker:          checker,
		SkillEvidencePath: writeSkillEvidenceForHarnesses(t, probedImage.Digest, probedHarnessVersion, harness.MandatorySkills(),
			harness.NameClaude, harness.NameCodex, harness.NameCopilot),
	})...)
}

// resultNamed returns the one result with name, failing the test otherwise.
func resultNamed(t *testing.T, report doctor.Report, name string) doctor.Result {
	t.Helper()
	for _, result := range report.Results {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("report has no %q result: %#v", name, report.Results)
	return doctor.Result{}
}

// hasResultNamed reports whether report contains a result called name.
func hasResultNamed(report doctor.Report, name string) bool {
	for _, result := range report.Results {
		if result.Name == name {
			return true
		}
	}
	return false
}

// writeCopilotToken writes a private token file and returns its path.
func writeCopilotToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copilot-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestStartupChecksLeaveCopilotOutOfCodexAndClaudeConfigurations verifies a
// repository that cannot select Copilot, including one with an old harness
// override permission, never probes it, so an image built before Copilot
// support stays usable.
func TestStartupChecksLeaveCopilotOutOfCodexAndClaudeConfigurations(t *testing.T) {
	policy := &config.RepositoryConfig{
		RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex},
		AllowedOverrides:    []config.OverrideName{config.OverrideHarness},
	}
	checker := &harnessDoctorChecker{}
	report := copilotStartupReport(t, policy, config.AuthenticationConfig{}, checker)
	for _, name := range []string{"copilot worker executable", "copilot worker skill contract", "copilot authentication"} {
		if hasResultNamed(report, name) {
			t.Fatalf("report contains %q for a repository that cannot select Copilot", name)
		}
	}
	if checker.calls != 2 {
		t.Fatalf("executable probes = %d, want Codex and Claude only", checker.calls)
	}
	if harness.RequiresSkillSmoke(policy, harness.NameCopilot) {
		t.Fatal("legacy override makes Copilot smoke evidence blocking")
	}
}

// TestStartupChecksProbeCopilotWhenSelectedOrOptedIn verifies a role default
// or an explicit override target makes Copilot a probed, evidence-blocking
// harness.
func TestStartupChecksProbeCopilotWhenSelectedOrOptedIn(t *testing.T) {
	optedIn := &config.RepositoryConfig{
		RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex},
		AllowedOverrides:    []config.OverrideName{config.OverrideHarness},
		OverrideHarnesses:   []config.Harness{config.HarnessCopilot},
	}
	for name, policy := range map[string]*config.RepositoryConfig{"role default": copilotDoctorPolicy(), "override target": optedIn} {
		t.Run(name, func(t *testing.T) {
			checker := &harnessDoctorChecker{}
			report := copilotStartupReport(t, policy, config.AuthenticationConfig{CopilotAuthPath: writeCopilotToken(t, "github_pat_example")}, checker)
			if result := resultNamed(t, report, "copilot worker executable"); result.Status != doctor.StatusPassed {
				t.Fatalf("copilot executable = %#v, want passed", result)
			}
			if result := resultNamed(t, report, "copilot worker skill contract"); result.Status != doctor.StatusPassed {
				t.Fatalf("copilot skill contract = %#v, want passed", result)
			}
			if !harness.RequiresSkillSmoke(policy, harness.NameCopilot) {
				t.Fatal("selectable Copilot does not require smoke evidence")
			}
		})
	}
}

// TestStartupChecksExplainAnOldImageWithoutCopilot verifies an image without
// a usable Copilot executable fails with an upgrade instruction.
func TestStartupChecksExplainAnOldImageWithoutCopilot(t *testing.T) {
	checker := &harnessDoctorChecker{err: errors.New("copilot: not found")}
	report := copilotStartupReport(t, copilotDoctorPolicy(), config.AuthenticationConfig{}, checker)
	result := resultNamed(t, report, "copilot worker executable")
	if result.Status != doctor.StatusFailed || result.Code != "harness.executable.unusable" {
		t.Fatalf("copilot executable = %#v, want harness.executable.unusable failure", result)
	}
	for _, want := range []string{"built before", "scripts/build-worker.sh", "worker_build.digest"} {
		if !strings.Contains(result.Action, want) {
			t.Fatalf("action = %q, want upgrade instruction containing %q", result.Action, want)
		}
	}
}

// TestStartupChecksDiagnoseTheCopilotCredentialSource verifies the doctor
// separates a missing source, an unusable source, and a usable managed source
// whose entitlement it cannot establish offline, without running a worker
// authentication probe or exposing the token.
func TestStartupChecksDiagnoseTheCopilotCredentialSource(t *testing.T) {
	const secret = "github_pat_secretvalue"
	unsupported := writeCopilotToken(t, "ghp_classic")
	for _, test := range []struct {
		name   string
		policy *config.RepositoryConfig
		path   string
		status doctor.Status
		code   string
		want   string
	}{
		{name: "missing for a selected role", policy: copilotDoctorPolicy(), status: doctor.StatusFailed, code: "harness.authentication.source_missing", want: "--copilot-auth"},
		{
			name: "missing for an override target only",
			policy: &config.RepositoryConfig{
				RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex},
				AllowedOverrides:    []config.OverrideName{config.OverrideHarness},
				OverrideHarnesses:   []config.Harness{config.HarnessCopilot},
			},
			status: doctor.StatusWarning, code: "harness.authentication.source_missing", want: "--copilot-auth",
		},
		{name: "unsupported token type", policy: copilotDoctorPolicy(), path: unsupported, status: doctor.StatusFailed, code: "harness.authentication.source_unsupported", want: "github_pat_"},
		{name: "usable managed source", policy: copilotDoctorPolicy(), path: writeCopilotToken(t, secret), status: doctor.StatusWarning, code: "harness.authentication.entitlement_unverified", want: "offline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker := &harnessDoctorChecker{}
			report := copilotStartupReport(t, test.policy, config.AuthenticationConfig{CopilotAuthPath: test.path}, checker)
			result := resultNamed(t, report, "copilot authentication")
			if result.Status != test.status || result.Code != test.code {
				t.Fatalf("copilot authentication = %#v, want %s %s", result, test.status, test.code)
			}
			if !strings.Contains(result.Problem+" "+result.Action, test.want) {
				t.Fatalf("result = %q / %q, want it to mention %q", result.Problem, result.Action, test.want)
			}
			if strings.Contains(result.Problem+result.Action, secret) || strings.Contains(result.Problem+result.Action, "ghp_classic") {
				t.Fatalf("result exposes the token: %#v", result)
			}
			if checker.authCalls != 0 {
				t.Fatalf("worker authentication probes = %d, want none for Codex without a source and Copilot offline", checker.authCalls)
			}
		})
	}
}

// TestStartupChecksBlockCopilotWithoutSmokeEvidence verifies a repository
// that selects Copilot cannot start until a Copilot smoke result for the
// pinned image is recorded, and that the action names the Copilot smoke call
// with its dedicated token file.
func TestStartupChecksBlockCopilotWithoutSmokeEvidence(t *testing.T) {
	checker := &harnessDoctorChecker{}
	report := doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
		Policy:                copilotDoctorPolicy(),
		Authentication:        config.AuthenticationConfig{CopilotAuthPath: writeCopilotToken(t, "github_pat_example")},
		Image:                 probedImage,
		Checker:               checker,
		AuthenticationChecker: checker,
		SkillChecker:          checker,
		SkillEvidencePath: writeSkillEvidenceForHarnesses(t, probedImage.Digest, probedHarnessVersion, harness.MandatorySkills(),
			harness.NameClaude, harness.NameCodex),
	})...)
	result := resultNamed(t, report, "copilot worker skill contract")
	if result.Status != doctor.StatusFailed || result.Code != "harness.skills.evidence_stale" {
		t.Fatalf("copilot skill contract = %#v, want a harness.skills.evidence_stale failure", result)
	}
	for _, want := range []string{"HARNESSES=copilot", "COPILOT_TOKEN_PATH=", "scripts/smoke-skills.sh"} {
		if !strings.Contains(result.Action, want) {
			t.Errorf("action = %q, want it to contain %q", result.Action, want)
		}
	}
}
