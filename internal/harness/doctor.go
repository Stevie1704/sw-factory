package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// CapabilityResolver returns the static capabilities for one configured
// harness. It performs no worker or terminal side effect.
type CapabilityResolver func(string) (Capabilities, error)

// CapabilityError identifies a role-to-harness capability refusal without
// retaining an adapter's underlying error text, which may contain sensitive
// environment details.
type CapabilityError struct {
	// Role is the repository-declared role that cannot be resumed safely.
	Role string
	// Harness is the repository-declared harness selected for Role.
	Harness string
	// Reason is a fixed safe explanation of the capability mismatch.
	Reason string
}

// Error returns the bounded capability refusal shown to a coordinator caller.
func (e *CapabilityError) Error() string {
	return fmt.Sprintf("role %q selected harness %q %s", e.Role, e.Harness, e.Reason)
}

// CapabilitiesFor returns the built-in adapter capabilities for a harness
// name. Unknown names fail closed before an issue can be claimed.
func CapabilitiesFor(name string) (Capabilities, error) {
	switch strings.TrimSpace(name) {
	case NameCodex:
		return (&Codex{}).Capabilities(), nil
	case NameClaude:
		return (&Claude{}).Capabilities(), nil
	default:
		return Capabilities{}, fmt.Errorf("%w: %q", ErrUnknownHarness, name)
	}
}

// ValidateInteractiveResumeCapabilities verifies every role-selected
// harness can resume an interrupted interactive session before claim effects
// begin.
func ValidateInteractiveResumeCapabilities(policy config.RepositoryConfig, resolve CapabilityResolver) error {
	if len(policy.RoleHarnessDefaults) == 0 {
		return errors.New("no role harnesses are configured")
	}
	if resolve == nil {
		resolve = CapabilitiesFor
	}
	roles := make([]string, 0, len(policy.RoleHarnessDefaults))
	for role := range policy.RoleHarnessDefaults {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		selected := string(policy.RoleHarnessDefaults[role])
		capabilities, err := resolve(selected)
		if err != nil {
			return &CapabilityError{Role: role, Harness: selected, Reason: "has no usable adapter"}
		}
		if capabilities.Name != selected {
			return &CapabilityError{Role: role, Harness: selected, Reason: "resolved to a different adapter"}
		}
		if !capabilities.InteractiveResume {
			return &CapabilityError{Role: role, Harness: selected, Reason: "does not support interactive resume"}
		}
	}
	return nil
}

// StartupRequest contains the configuration and worker seam needed by the
// harness-owned startup checks.
type StartupRequest struct {
	// Policy is the checked-in role-to-harness selection.
	Policy *config.RepositoryConfig
	// Authentication contains optional host credential-file paths.
	Authentication config.AuthenticationConfig
	// Image is the immutable worker image used for executable probes.
	Image worker.ImageReference
	// Checker probes harness executables inside the worker image.
	Checker worker.HarnessChecker
	// AuthenticationChecker verifies configured credentials inside the worker
	// image without returning credential contents.
	AuthenticationChecker worker.HarnessAuthenticationChecker
	// Resolve supplies adapter capabilities; nil selects built-ins.
	Resolve CapabilityResolver
	// SkillChecker verifies the installed skill set inside the worker image.
	SkillChecker worker.SkillContractChecker
	// SkillEvidencePath is the host path of the recorded worker skill smoke
	// evidence.
	SkillEvidencePath string
}

// StartupChecks returns independent capability, executable, and authentication
// checks. Every selected harness contributes a check before the run begins.
func StartupChecks(request StartupRequest) []doctor.Check {
	checks := []doctor.Check{interactiveResumeCheck(request.Policy, request.Resolve)}
	selected := selectedHarnesses(request.Policy)
	for _, name := range selected {
		harnessName := name
		checks = append(checks, executableCheck(request.Checker, request.Image, harnessName))
		checks = append(checks, skillContractCheck(request, harnessName))
	}
	checks = append(checks,
		credentialCheck(NameCodex, request.Authentication.CodexAuthPath, request.Image, request.AuthenticationChecker),
		credentialCheck(NameClaude, request.Authentication.ClaudeAuthPath, request.Image, request.AuthenticationChecker),
	)
	return checks
}

// selectedHarnesses returns both built-in harnesses plus any extra name in the
// policy, in deterministic order. The worker image is therefore checked for
// both supported adapters even when a repository currently selects only one.
func selectedHarnesses(policy *config.RepositoryConfig) []string {
	seen := map[string]struct{}{NameClaude: {}, NameCodex: {}}
	if policy == nil {
		return []string{NameClaude, NameCodex}
	}
	for _, selected := range policy.RoleHarnessDefaults {
		name := strings.TrimSpace(string(selected))
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	selected := make([]string, 0, len(seen))
	for name := range seen {
		selected = append(selected, name)
	}
	sort.Strings(selected)
	return selected
}

// interactiveResumeCheck adapts the capability validation error to a bounded
// operator-facing diagnosis without exposing implementation error details.
func interactiveResumeCheck(policy *config.RepositoryConfig, resolve CapabilityResolver) doctor.Check {
	return func(context.Context) doctor.Result {
		if policy == nil {
			return doctor.Failure("harness capability", "repository harness policy is unavailable", "repair the checked-in role_harness_defaults configuration")
		}
		if err := ValidateInteractiveResumeCapabilities(*policy, resolve); err != nil {
			return doctor.Failure("harness capability", err.Error(), "select an adapter with interactive resume support for every declared role")
		}
		return doctor.Success("harness capability")
	}
}

// executableCheck probes one selected harness in the exact pinned image.
func executableCheck(checker worker.HarnessChecker, image worker.ImageReference, name string) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if checker == nil {
			return doctor.Failure(name+" worker executable", "the worker harness diagnosis adapter is unavailable", "configure the Docker worker runtime")
		}
		if err := checker.CheckHarness(ctx, worker.HarnessCheckRequest{Image: image, Name: name}); err != nil {
			return doctor.Failure(name+" worker executable", "the configured "+name+" executable is not usable in the worker image", "rebuild or load the pinned worker image with the configured "+name+" harness")
		}
		return doctor.Success(name + " worker executable")
	}
}

// credentialCheck validates an optional source and, when configured, asks the
// worker adapter to verify it with the harness's local auth command. It never
// opens or reports credential contents in a diagnosis result.
func credentialCheck(name, path string, image worker.ImageReference, checker worker.HarnessAuthenticationChecker) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if strings.TrimSpace(path) == "" {
			return doctor.Warning(name+" authentication", "no host credential file is configured", "authenticate during the first visible worker session or configure a private credential file")
		}
		info, err := os.Lstat(path)
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") || err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 || info.Size() == 0 {
			return doctor.Failure(name+" authentication", "the configured host credential source is unavailable or unsafe", "provide a non-empty regular credential file readable only by its owner")
		}
		if checker == nil {
			return doctor.Failure(name+" authentication", "the worker authentication diagnosis adapter is unavailable", "configure the Docker worker runtime to verify the credential inside the pinned image")
		}
		if err := checker.CheckHarnessAuthentication(ctx, worker.HarnessAuthenticationCheckRequest{Image: image, Name: name, AuthPath: path}); err != nil {
			return doctor.Failure(name+" authentication", "the configured credential is not usable by the worker harness", "authenticate the "+name+" harness and provide a valid private credential source")
		}
		return doctor.Success(name + " authentication")
	}
}

// skillContractCheck verifies one harness advertises every role-mandated skill
// in the pinned worker image, and that a recorded smoke result proves a real
// invocation of that exact image and harness build could use them. Startup
// reads the recorded result rather than paying for a model call.
func skillContractCheck(request StartupRequest, name string) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		diagnosis := name + " worker skill contract"
		if request.SkillChecker == nil {
			return doctor.Failure(diagnosis, "the worker skill diagnosis adapter is unavailable", "configure the Docker worker runtime")
		}
		required := MandatorySkills()
		contract, err := request.SkillChecker.CheckSkillContract(ctx, worker.SkillContractRequest{Image: request.Image, Harness: name, Skills: required})
		if err != nil {
			return doctor.Failure(diagnosis, "the pinned worker image does not advertise every role-mandated skill to "+name, "rebuild the worker image so each role-mandated skill is installed once per discovery root and left model-visible")
		}
		evidence, err := LoadSkillSmokeEvidence(request.SkillEvidencePath)
		if err != nil {
			return doctor.Failure(diagnosis, "the recorded worker skill smoke evidence is unavailable", "record a smoke result for the pinned worker digest with scripts/smoke-skills.sh")
		}
		if !evidence.Covers(request.Image.Digest, name, contract.Version, required) {
			return doctor.Failure(diagnosis, "no recorded smoke result proves "+name+" can use the role-mandated skills in the pinned worker image", "record a smoke result for the pinned worker digest and harness version with scripts/smoke-skills.sh")
		}
		return doctor.Success(diagnosis)
	}
}
