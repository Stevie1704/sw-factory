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
		Authentication: config.AuthenticationConfig{CodexAuthPath: codexPath, ClaudeAuthPath: claudePath},
		Image: worker.ImageReference{
			Name:   "ghcr.io/example/factory-worker",
			Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Checker:               checker,
		AuthenticationChecker: checker,
	})...)
	if !report.Ready() {
		t.Fatalf("harness report = %#v, want ready", report)
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
		Policy: &config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{"test": config.HarnessCodex}},
		Image: worker.ImageReference{
			Name:   "ghcr.io/example/factory-worker",
			Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Checker: &harnessDoctorChecker{},
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
	err       error
	calls     int
	authCalls int
	authError error
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

var _ worker.HarnessChecker = (*harnessDoctorChecker)(nil)
var _ worker.HarnessAuthenticationChecker = (*harnessDoctorChecker)(nil)
