package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestValidateInteractiveResumeCapabilitiesRefusesAMissingCapability verifies
// a role cannot select an adapter that cannot recover an interactive session.
func TestValidateInteractiveResumeCapabilitiesRefusesAMissingCapability(t *testing.T) {
	policy := config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex}}
	err := harness.ValidateInteractiveResumeCapabilities(policy, func(string) (harness.Capabilities, error) {
		return harness.Capabilities{Name: harness.NameCodex}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "interactive resume") {
		t.Fatalf("ValidateInteractiveResumeCapabilities() error = %v, want missing capability", err)
	}
}

// TestStartupChecksProbeBothHarnessesAndConfiguredCredentials verifies the
// harness contributor checks both adapters and never reads credential content
// into a diagnosis result.
func TestStartupChecksProbeBothHarnessesAndConfiguredCredentials(t *testing.T) {
	root := t.TempDir()
	codexPath := filepath.Join(root, "auth.json")
	claudePath := filepath.Join(root, ".credentials.json")
	for _, path := range []string{codexPath, claudePath} {
		if err := os.WriteFile(path, []byte("credential contents"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checker := &harnessDoctorChecker{}
	report := doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
		Policy: &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{
			"test": config.HarnessCodex, "implementation": config.HarnessClaude,
		}},
		Authentication:        config.AuthenticationConfig{CodexAuthPath: codexPath, ClaudeAuthPath: claudePath},
		Image:                 probedImage,
		Checker:               checker,
		AuthenticationChecker: checker,
		SkillChecker:          checker,
		SkillEvidencePath:     writeSkillEvidence(t, probedImage.Digest, probedHarnessVersion, harness.MandatorySkills()),
	})...)
	if !report.Ready() {
		t.Fatalf("harness report = %#v, want ready", report)
	}
	if checker.skillCalls != 2 {
		t.Fatalf("harness skill contract checks = %d, want Codex and Claude", checker.skillCalls)
	}
	if checker.calls != 2 {
		t.Fatalf("harness executable checks = %d, want Codex and Claude", checker.calls)
	}
	if checker.authCalls != 2 {
		t.Fatalf("harness authentication checks = %d, want Codex and Claude", checker.authCalls)
	}
}

// TestStartupChecksKeepMissingOptionalCredentialsNonBlocking verifies a host
// may defer authentication to an interactive worker session.
func TestStartupChecksKeepMissingOptionalCredentialsNonBlocking(t *testing.T) {
	report := doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
		Policy:            &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"test": config.HarnessCodex}},
		Image:             probedImage,
		Checker:           &harnessDoctorChecker{},
		SkillChecker:      &harnessDoctorChecker{},
		SkillEvidencePath: writeSkillEvidence(t, probedImage.Digest, probedHarnessVersion, harness.MandatorySkills()),
	})...)
	if !report.Ready() || len(report.Warnings()) != 2 {
		t.Fatalf("harness report = %#v, want two non-blocking auth warnings", report)
	}
}

// TestStartupChecksDoNotRenderCredentialErrors verifies configured credential
// failures never include a path's contents or the underlying error string.
func TestStartupChecksDoNotRenderCredentialErrors(t *testing.T) {
	secret := "credential-secret"
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	report := doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
		Authentication: config.AuthenticationConfig{CodexAuthPath: path},
	})...)
	for _, result := range report.Results {
		if strings.Contains(result.Problem+result.Action, secret) {
			t.Fatalf("harness result exposed credential contents: %#v", result)
		}
	}
}

// TestStartupChecksDoNotRenderWorkerAuthenticationErrors verifies an auth
// command's private diagnostic output is also reduced to a safe result.
func TestStartupChecksDoNotRenderWorkerAuthenticationErrors(t *testing.T) {
	secret := "worker-auth-secret"
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("credential contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	checker := &harnessDoctorChecker{authError: errors.New(secret)}
	report := doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
		Authentication:        config.AuthenticationConfig{CodexAuthPath: path},
		AuthenticationChecker: checker,
	})...)
	for _, result := range report.Results {
		if strings.Contains(result.Problem+result.Action, secret) {
			t.Fatalf("harness result exposed worker authentication error: %#v", result)
		}
	}
}

// harnessDoctorChecker records worker-image harness probes.
type harnessDoctorChecker struct {
	err        error
	calls      int
	authCalls  int
	authError  error
	skillCalls int
	skillError error
	version    string
}

// CheckHarness implements the worker harness diagnosis seam.
func (c *harnessDoctorChecker) CheckHarness(context.Context, worker.HarnessCheckRequest) error {
	c.calls++
	return c.err
}

// CheckHarnessAuthentication implements the worker credential diagnosis seam.
func (c *harnessDoctorChecker) CheckHarnessAuthentication(context.Context, worker.HarnessAuthenticationCheckRequest) error {
	c.authCalls++
	return c.authError
}

// CheckSkillContract implements the worker skill contract diagnosis seam.
func (c *harnessDoctorChecker) CheckSkillContract(_ context.Context, request worker.SkillContractRequest) (worker.SkillContract, error) {
	c.skillCalls++
	if c.skillError != nil {
		return worker.SkillContract{}, c.skillError
	}
	version := c.version
	if version == "" {
		version = probedHarnessVersion
	}
	return worker.SkillContract{Harness: request.Harness, Version: version}, nil
}

var _ worker.HarnessChecker = (*harnessDoctorChecker)(nil)
var _ worker.HarnessAuthenticationChecker = (*harnessDoctorChecker)(nil)
var _ worker.SkillContractChecker = (*harnessDoctorChecker)(nil)

// probedHarnessVersion is the harness version a fake worker probe reports.
const probedHarnessVersion = "1.2.3"

// probedImage is the pinned worker image used by the harness diagnosis tests.
var probedImage = worker.ImageReference{
	Name:   "ghcr.io/example/factory-worker",
	Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
}

// writeSkillEvidence records a smoke result for every supported harness at the
// pinned digest and returns the evidence file path.
func writeSkillEvidence(t *testing.T, digest, version string, skills []string) string {
	t.Helper()
	evidence := harness.SkillSmokeEvidence{SchemaVersion: 1}
	for _, name := range []string{harness.NameClaude, harness.NameCodex} {
		evidence.Records = append(evidence.Records, harness.SkillSmokeRecord{
			ImageDigest:    digest,
			Harness:        name,
			HarnessVersion: version,
			Skills:         skills,
			VerifiedAt:     "2026-09-04T00:00:00Z",
		})
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "skill-smoke.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestStartupChecksRefuseAWorkerImageThatHidesAMandatorySkill verifies a role
// cannot start against an image whose harness omits a role-mandated skill from
// its model-visible catalog.
func TestStartupChecksRefuseAWorkerImageThatHidesAMandatorySkill(t *testing.T) {
	secret := "probe-output-secret"
	checker := &harnessDoctorChecker{skillError: errors.New(secret)}
	report := doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
		Image:             probedImage,
		Checker:           &harnessDoctorChecker{},
		SkillChecker:      checker,
		SkillEvidencePath: writeSkillEvidence(t, probedImage.Digest, probedHarnessVersion, harness.MandatorySkills()),
	})...)
	if got := skillContractFailures(report); got != 2 {
		t.Fatalf("harness skill contract failures = %d, want one per shipped harness", got)
	}
	for _, result := range report.Results {
		if strings.Contains(result.Problem+result.Action, secret) {
			t.Fatalf("harness result exposed worker probe output: %#v", result)
		}
	}
}

// TestStartupChecksRequireSmokeEvidenceForThePinnedDigestAndVersion verifies
// startup refuses evidence recorded against a different worker image or a
// different harness build, without making a model call of its own.
func TestStartupChecksRequireSmokeEvidenceForThePinnedDigestAndVersion(t *testing.T) {
	otherDigest := "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	for name, path := range map[string]string{
		"missing file":         filepath.Join(t.TempDir(), "absent.json"),
		"other worker image":   writeSkillEvidence(t, otherDigest, probedHarnessVersion, harness.MandatorySkills()),
		"other harness build":  writeSkillEvidence(t, probedImage.Digest, "9.9.9", harness.MandatorySkills()),
		"incomplete skill set": writeSkillEvidence(t, probedImage.Digest, probedHarnessVersion, []string{"implement"}),
	} {
		t.Run(name, func(t *testing.T) {
			report := doctor.Run(context.Background(), harness.StartupChecks(harness.StartupRequest{
				Image:             probedImage,
				Checker:           &harnessDoctorChecker{},
				SkillChecker:      &harnessDoctorChecker{},
				SkillEvidencePath: path,
			})...)
			if got := skillContractFailures(report); got != 2 {
				t.Fatalf("harness skill contract failures = %d, want one per shipped harness", got)
			}
		})
	}
}

// skillContractFailures counts the blocking skill-contract diagnoses in a
// report, leaving unrelated harness checks out of the assertion.
func skillContractFailures(report doctor.Report) int {
	failures := 0
	for _, result := range report.Failures() {
		if strings.HasSuffix(result.Name, "worker skill contract") {
			failures++
		}
	}
	return failures
}
