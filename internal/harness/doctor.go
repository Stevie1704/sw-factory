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
// harness. It performs no worker side effect.
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
		return Capabilities{Name: NameCodex, NativeResume: true, Headless: true}, nil
	case NameClaude:
		return Capabilities{Name: NameClaude, NativeResume: true, Headless: true}, nil
	case NameCopilot:
		return Capabilities{Name: NameCopilot, NativeResume: true, Headless: true}, nil
	default:
		return Capabilities{}, fmt.Errorf("%w: %q", ErrUnknownHarness, name)
	}
}

// ValidateNativeResumeCapabilities verifies every role-selected
// harness can resume an interrupted native session before claim effects begin.
func ValidateNativeResumeCapabilities(policy config.RepositoryConfig, resolve CapabilityResolver) error {
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
		if !capabilities.NativeResume {
			return &CapabilityError{Role: role, Harness: selected, Reason: "does not support native resume"}
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
	// ImageReady reports whether the worker image could be inspected. A probe
	// inside the image is skipped when it returns an error. Nil means ready.
	ImageReady func(context.Context) error
	// Checker probes harness executables inside the worker image.
	Checker worker.HarnessChecker
	// AuthenticationChecker verifies configured credentials inside the worker
	// image without returning credential contents.
	AuthenticationChecker worker.HarnessAuthenticationChecker
	// Resolve supplies adapter capabilities; nil selects built-ins.
	Resolve CapabilityResolver
	// SkillChecker verifies the worker skill set inside the worker image.
	SkillChecker worker.SkillContractChecker
	// SkillEvidencePath is the host path of the recorded worker skill smoke
	// evidence.
	SkillEvidencePath string
	// HeadlessChecker verifies the worker process helper used by the migrated
	// terminal-free adapters.
	HeadlessChecker worker.HeadlessChecker
	// AllRolesHeadless reports that every declared role selects a harness the
	// coordinator runs without a terminal, so the worker process helper is a
	// blocking prerequisite.
	AllRolesHeadless bool
}

// StartupChecks returns independent capability, executable, and authentication
// checks. Every selected harness contributes a check before the run begins.
func StartupChecks(request StartupRequest) []doctor.Check {
	checks := []doctor.Check{nativeResumeCheck(request)}
	selected := selectedHarnesses(request.Policy)
	for _, name := range selected {
		harnessName := name
		checks = append(checks, executableCheck(request, harnessName))
		checks = append(checks, skillContractCheck(request, harnessName))
	}
	checks = append(checks,
		credentialCheck(request, NameCodex, request.Authentication.CodexAuthPath),
		credentialCheck(request, NameClaude, request.Authentication.ClaudeAuthPath),
	)
	return checks
}

// selectedHarnesses returns both built-in harnesses plus any extra name in the
// policy, in deterministic order. The worker image is checked for both
// supported adapters even when a repository currently selects only one.
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

// RequiresSkillSmoke reports whether one harness can run a repository role in
// the current policy. An unavailable policy remains fail-closed because the
// coordinator cannot safely determine which harnesses the repository permits.
func RequiresSkillSmoke(policy *config.RepositoryConfig, harnessName string) bool {
	if policy == nil {
		return true
	}
	for _, selected := range policy.RoleHarnessDefaults {
		if strings.TrimSpace(string(selected)) == harnessName {
			return true
		}
	}
	for _, override := range policy.AllowedOverrides {
		if override == config.OverrideHarness {
			return true
		}
	}
	return false
}

// Harness diagnosis conditions. Codes are stable; wording may change.
var (
	capabilityPolicyUnavailable  = doctor.Condition{Code: "harness.capability.policy_unavailable", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup", Unobserved: true}
	capabilityUnsupported        = doctor.Condition{Code: "harness.capability.unsupported", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	headlessHelperMissing        = doctor.Condition{Code: "harness.capability.headless_helper_missing", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	executableAdapterMissing     = doctor.AdapterMissing("harness.executable.adapter_missing")
	executableUnusable           = doctor.Condition{Code: "harness.executable.unusable", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	authenticationSourceMissing  = doctor.Condition{Code: "harness.authentication.source_missing", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	authenticationSourceUnsafe   = doctor.Condition{Code: "harness.authentication.source_unsafe", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	authenticationAdapterMissing = doctor.AdapterMissing("harness.authentication.adapter_missing")
	authenticationRejected       = doctor.Condition{Code: "harness.authentication.rejected", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	skillsAdapterMissing         = doctor.AdapterMissing("harness.skills.adapter_missing")
	skillsContractUnmet          = doctor.Condition{Code: "harness.skills.contract_unmet", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	skillsEvidenceMissing        = doctor.Condition{Code: "harness.skills.evidence_missing", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	skillsEvidenceStale          = doctor.Condition{Code: "harness.skills.evidence_stale", Origin: doctor.OriginRepository, Effect: doctor.EffectLocalMutation, Topic: "setup"}
)

// imageReady reports whether probes inside the worker image can run.
func imageReady(ctx context.Context, request StartupRequest) bool {
	return request.ImageReady == nil || request.ImageReady(ctx) == nil
}

// harnessFact names the diagnosed harness as a supporting observation.
func harnessFact(name string) doctor.Fact {
	return doctor.Fact{Name: "harness", Value: name}
}

// nativeResumeCheck adapts the capability validation error to a bounded
// operator-facing diagnosis without exposing implementation error details.
func nativeResumeCheck(request StartupRequest) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		policy := request.Policy
		if policy == nil {
			return doctor.Failure(capabilityPolicyUnavailable, "harness capability", "repository harness policy is unavailable", "repair the checked-in role_harness_defaults configuration")
		}
		if err := ValidateNativeResumeCapabilities(*policy, request.Resolve); err != nil {
			return doctor.Failure(capabilityUnsupported, "harness capability", err.Error(), "select an adapter with native resume support for every declared role")
		}
		if request.AllRolesHeadless && request.HeadlessChecker != nil {
			if !imageReady(ctx, request) {
				return doctor.Skipped("harness.capability.not_checked", "harness capability", worker.ImageUnavailableReason)
			}
			if err := request.HeadlessChecker.CheckHeadless(ctx, worker.HeadlessCheckRequest{Image: request.Image}); err != nil {
				return worker.ProbeFailure(headlessHelperMissing, "harness capability", err, "the pinned worker image does not contain a usable headless process helper", "rebuild the pinned worker image with factory-worker-headless")
			}
		}
		return doctor.Success("harness.capability.ready", "harness capability")
	}
}

// executableCheck probes one selected harness in the exact pinned image.
func executableCheck(request StartupRequest, name string) doctor.Check {
	checker, image := request.Checker, request.Image
	return func(ctx context.Context) doctor.Result {
		if checker == nil {
			return doctor.Failure(executableAdapterMissing, name+" worker executable", "the worker harness diagnosis adapter is unavailable", "configure the Docker worker runtime").WithFacts(harnessFact(name))
		}
		if !imageReady(ctx, request) {
			return doctor.Skipped("harness.executable.not_checked", name+" worker executable", worker.ImageUnavailableReason).WithFacts(harnessFact(name))
		}
		if err := checker.CheckHarness(ctx, worker.HarnessCheckRequest{Image: image, Name: name}); err != nil {
			return worker.ProbeFailure(executableUnusable, name+" worker executable", err, "the configured "+name+" executable is not usable in the worker image", "rebuild or load the pinned worker image with the configured "+name+" harness").WithFacts(harnessFact(name))
		}
		return doctor.Success("harness.executable.ready", name+" worker executable").WithFacts(harnessFact(name))
	}
}

// credentialCheck validates an optional source and, when configured, asks the
// worker adapter to verify it with the harness's local auth command. It never
// opens or reports credential contents in a diagnosis result.
func credentialCheck(request StartupRequest, name, path string) doctor.Check {
	image, checker := request.Image, request.AuthenticationChecker
	return func(ctx context.Context) doctor.Result {
		if strings.TrimSpace(path) == "" {
			// ADR 0002 keeps a host source optional: a harness without one runs
			// on the credential its own role volume already holds. An
			// unattended headless run cannot log in for itself, though, so the
			// action names the two things that actually produce one.
			return doctor.Warning(authenticationSourceMissing, name+" authentication", "no host credential file is configured", "configure a private credential file, or keep the credential this harness already persisted in its worker role volume").WithFacts(harnessFact(name))
		}
		info, err := os.Lstat(path)
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") || err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 || info.Size() == 0 {
			return doctor.Failure(authenticationSourceUnsafe, name+" authentication", "the configured host credential source is unavailable or unsafe", "provide a non-empty regular credential file readable only by its owner").WithFacts(harnessFact(name))
		}
		if checker == nil {
			return doctor.Failure(authenticationAdapterMissing, name+" authentication", "the worker authentication diagnosis adapter is unavailable", "configure the Docker worker runtime to verify the credential inside the pinned image")
		}
		if !imageReady(ctx, request) {
			return doctor.Skipped("harness.authentication.not_checked", name+" authentication", worker.ImageUnavailableReason).WithFacts(harnessFact(name))
		}
		if err := checker.CheckHarnessAuthentication(ctx, worker.HarnessAuthenticationCheckRequest{Image: image, Name: name, AuthPath: path}); err != nil {
			return worker.ProbeFailure(authenticationRejected, name+" authentication", err, "the configured credential is not usable by the worker harness", "authenticate the "+name+" harness and provide a valid private credential source").WithFacts(harnessFact(name))
		}
		return doctor.Success("harness.authentication.ready", name+" authentication").WithFacts(harnessFact(name))
	}
}

// skillContractCheck verifies one harness advertises every role-mandated skill
// in the pinned worker image, and that a recorded smoke result proves a real
// invocation of that exact image and harness build could use them. Startup
// reads the recorded result rather than paying for a model call. Evidence is
// blocking only when the repository can select the harness through its role
// defaults or an allowed harness override; the deterministic image contract
// remains blocking for every shipped harness.
func skillContractCheck(request StartupRequest, name string) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		diagnosis := name + " worker skill contract"
		blocking := RequiresSkillSmoke(request.Policy, name)
		if request.SkillChecker == nil {
			return doctor.Failure(skillsAdapterMissing, diagnosis, "the worker skill diagnosis adapter is unavailable", "configure the Docker worker runtime").WithFacts(harnessFact(name))
		}
		if !imageReady(ctx, request) {
			return doctor.Skipped("harness.skills.not_checked", diagnosis, worker.ImageUnavailableReason).WithFacts(harnessFact(name))
		}
		required := RequiredSkills(request.Policy, name)
		contract, err := request.SkillChecker.CheckSkillContract(ctx, worker.SkillContractRequest{Image: request.Image, Harness: name, Skills: required})
		if err != nil {
			return worker.ProbeFailure(skillsContractUnmet, diagnosis, err, "the pinned worker image does not satisfy the role-mandated skill contract for "+name, "rebuild the worker image so each role-mandated skill appears once per discovery root and stays model-visible, and verify the configured image digest")
		}
		evidence, err := LoadSkillSmokeEvidence(request.SkillEvidencePath)
		if err != nil {
			return skillEvidenceResult(blocking, skillsEvidenceMissing, diagnosis, "the recorded worker skill smoke evidence is unavailable", "record a smoke result for the pinned worker digest with scripts/smoke-skills.sh")
		}
		if !evidence.Covers(request.Image.Digest, name, contract.Version, required) {
			return skillEvidenceResult(blocking, skillsEvidenceStale, diagnosis, "no recorded smoke result proves "+name+" can use the role-mandated skills in the pinned worker image", "record a smoke result for the pinned worker digest and harness version with scripts/smoke-skills.sh")
		}
		return doctor.Success("harness.skills.ready", diagnosis).WithFacts(harnessFact(name))
	}
}

// skillEvidenceResult applies the repository policy's blocking scope to a
// missing or stale real-invocation smoke result. The deterministic image skill
// contract remains blocking for every shipped harness.
func skillEvidenceResult(blocking bool, condition doctor.Condition, diagnosisName, problem, action string) doctor.Result {
	if blocking {
		return doctor.Failure(condition, diagnosisName, problem, action)
	}
	return doctor.Warning(condition, diagnosisName, problem, action)
}
