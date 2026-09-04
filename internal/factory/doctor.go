package factory

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// DoctorResult contains the complete neutral startup diagnosis for the
// configured repository. A report is returned even when checks fail so one
// invocation exposes every corrective action.
type DoctorResult struct {
	// Report contains all subsystem contributions in deterministic order.
	Report doctor.Report
}

// Ready reports whether the diagnosis contains no blocking check.
func (r DoctorResult) Ready() bool {
	return r.Report.Ready()
}

// Doctor composes subsystem-owned checks without embedding their command or
// protocol knowledge in the factory coordinator.
func (s *Service) Doctor(ctx context.Context) (DoctorResult, error) {
	configuration, configurationCheck := config.StartupCheck(s.configPath)
	checks := []doctor.Check{configurationCheck}

	registration := config.RepositoryRegistration{}
	if configuration.Registration != nil {
		registration = *configuration.Registration
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	repositoryPolicy := configuration.Repository
	image := worker.ImageReference{}
	if repositoryPolicy != nil {
		image = worker.ImageReference{
			Name:   repositoryPolicy.WorkerBuild.Image,
			Digest: repositoryPolicy.WorkerBuild.Digest,
		}
	}

	checks = append(checks, github.StartupChecks(s.doctorGitHub(), repository)...)
	checks = append(checks, gitadapter.StartupChecks(s.doctorGitWorkspace(), gitadapter.DoctorRequest{
		RepositoryPath:     registration.Path,
		RemoteName:         gitadapter.DefaultRemoteName,
		ExpectedOwner:      registration.GitHub.Owner,
		ExpectedRepository: registration.GitHub.Repository,
		TargetBranch:       targetBranch(repositoryPolicy),
		RoleCraft:          roleCraft(repositoryPolicy),
	})...)

	terminalChecker := s.deps.Terminal
	if terminalChecker == nil {
		terminalChecker = terminal.NewCmuxRuntime(nil, registration.Cmux.SocketPath)
	}
	checks = append(checks, terminal.StartupChecks(asTerminalDoctorChecker(terminalChecker))...)

	checks = append(checks, worker.StartupChecks(s.doctorWorker(), image)...)
	checks = append(checks, harness.StartupChecks(harness.StartupRequest{
		Policy:                repositoryPolicy,
		Authentication:        registration.Authentication,
		Image:                 image,
		Checker:               s.doctorHarnessChecker(),
		AuthenticationChecker: s.doctorHarnessAuthenticationChecker(),
		Resolve:               s.deps.HarnessCapabilities,
		SkillChecker:          s.doctorSkillContractChecker(),
		SkillEvidencePath:     skillEvidencePath(registration.Path),
	})...)
	checks = append(checks, store.StartupCheck(registration.OperationalDataPath))

	return DoctorResult{Report: doctor.Run(ctx, checks...)}, nil
}

// doctorGitHub resolves the read-only GitHub diagnosis seam.
func (s *Service) doctorGitHub() github.DoctorClient {
	checker, _ := s.deps.GitHub.(github.DoctorClient)
	return checker
}

// doctorGitWorkspace resolves the read-only Git diagnosis seam from the
// task-oriented workspace first, then the creation-only compatibility seam.
func (s *Service) doctorGitWorkspace() gitadapter.DoctorChecker {
	if checker, ok := s.deps.GitWorkspace.(gitadapter.DoctorChecker); ok {
		return checker
	}
	checker, _ := s.deps.Worktree.(gitadapter.DoctorChecker)
	return checker
}

// asTerminalDoctorChecker resolves terminal diagnosis without expanding the
// portable TerminalRuntime interface for callers that provide their own
// adapter.
func asTerminalDoctorChecker(runtime terminal.TerminalRuntime) terminal.DoctorChecker {
	checker, _ := runtime.(terminal.DoctorChecker)
	return checker
}

// doctorWorker resolves the worker daemon and image diagnosis seam.
func (s *Service) doctorWorker() worker.DoctorChecker {
	checker, _ := s.deps.Worker.(worker.DoctorChecker)
	return checker
}

// doctorHarnessChecker resolves the worker-owned executable probe seam.
func (s *Service) doctorHarnessChecker() worker.HarnessChecker {
	checker, _ := s.deps.Worker.(worker.HarnessChecker)
	return checker
}

// doctorHarnessAuthenticationChecker resolves the worker-owned credential
// probe seam.
func (s *Service) doctorHarnessAuthenticationChecker() worker.HarnessAuthenticationChecker {
	checker, _ := s.deps.Worker.(worker.HarnessAuthenticationChecker)
	return checker
}

// doctorSkillContractChecker resolves the worker-owned skill contract probe
// seam.
func (s *Service) doctorSkillContractChecker() worker.SkillContractChecker {
	checker, _ := s.deps.Worker.(worker.SkillContractChecker)
	return checker
}

// skillEvidencePath resolves the recorded worker skill smoke evidence inside
// the registered repository, leaving the path empty when no repository is
// registered so the diagnosis stays independent of configuration failures.
func skillEvidencePath(repositoryPath string) string {
	if strings.TrimSpace(repositoryPath) == "" {
		return ""
	}
	return filepath.Join(repositoryPath, filepath.FromSlash(harness.SkillEvidenceFile))
}

// targetBranch extracts the checked-in branch while leaving dependent checks
// empty when repository configuration was unavailable.
func targetBranch(policy *config.RepositoryConfig) string {
	if policy == nil {
		return ""
	}
	return policy.TargetBranch
}

// roleCraft extracts the frozen repository role-craft map for Git diagnosis
// while keeping configuration failures independent from later checks.
func roleCraft(policy *config.RepositoryConfig) map[string]string {
	if policy == nil || len(policy.RoleCraft) == 0 {
		return nil
	}
	result := make(map[string]string, len(policy.RoleCraft))
	for role, path := range policy.RoleCraft {
		result[role] = path
	}
	return result
}

var _ Factory = (*Service)(nil)
