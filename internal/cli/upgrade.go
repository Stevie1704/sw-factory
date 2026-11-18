package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/upgrade"
	"github.com/Stevie1704/sw-factory/internal/version"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// upgradeImages is the read-only Docker seam of upgrade planning: it checks
// that an exact image exists locally and reads its release label, and never
// pulls.
type upgradeImages interface {
	onboarding.ImageChecker
	worker.ReleaseChecker
}

// upgradeImagesForCLI is the Docker seam, so the CLI contract can be tested
// without Docker.
var upgradeImagesForCLI = func() upgradeImages { return worker.NewDockerRuntime() }

// upgradeIdentityForCLI is the build identity seam: the plan runs from the
// target binary, so its identity is the target release.
var upgradeIdentityForCLI = version.Current

// upgradeLookPathForCLI resolves the factory command that PATH names.
var upgradeLookPathForCLI = exec.LookPath

// upgradePlanDocument is the JSON result of upgrade plan.
type upgradePlanDocument struct {
	Command        string `json:"command"`
	Config         string `json:"config"`
	OnboardingPlan string `json:"onboarding_plan,omitempty"`
	// StoreBackups lists the existing pre-migration backups, as provenance.
	StoreBackups []string `json:"store_backups"`
	upgrade.Plan
}

// migrateStoreDocument is the JSON result of upgrade migrate-store.
type migrateStoreDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	Command       string                 `json:"command"`
	Outcome       string                 `json:"outcome"`
	Migration     *store.Migration       `json:"migration,omitempty"`
	RolledBack    bool                   `json:"rolled_back_interrupted_write,omitempty"`
	Refusals      []store.UpgradeFinding `json:"refusals"`
	Error         *errorDocument         `json:"error,omitempty"`
}

// migrateStoreSchema is the version of the migrate-store document.
const migrateStoreSchema = 1

// runUpgrade dispatches the upgrade subcommands.
func runUpgrade(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	if len(args) > 0 && args[0] == "plan" {
		return runUpgradePlan(ctx, args[1:], defaultConfigPath, output, errorsOutput)
	}
	if len(args) > 0 && args[0] == "migrate-store" {
		return runUpgradeMigrateStore(ctx, args[1:], defaultConfigPath, output, errorsOutput)
	}
	writeError(errorsOutput, errors.New("upgrade needs a subcommand: plan or migrate-store; read factory guide maintenance"))
	return 2
}

// runUpgradePlan writes the upgrade plan of one installation. It only reads:
// it never migrates the store, replaces a binary, pulls an image, resumes
// work, or makes a paid call. It exits 0 for a ready or complete plan and 1
// for a blocked one.
func runUpgradePlan(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("upgrade plan", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration of the installation to upgrade")
	onboardingPlan := flags.String("onboarding-plan", "", "onboarding plan of the repository, whose valid evidence the upgrade reuses")
	resources := flags.String("resources", "", "directory that onboard prepare of the target filled; defaults to the onboarding plan's value")
	asJSON := flags.Bool("json", false, "write the plan as one schema-versioned JSON document; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return 2
	}
	if !*asJSON {
		writeError(errorsOutput, errors.New("upgrade plan needs --json"))
		return 2
	}
	observation, err := observeUpgrade(ctx, *configPath, *onboardingPlan, *resources)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	plan := upgrade.Build(observation)
	document := upgradePlanDocument{Command: "upgrade plan", Config: observation.ConfigPath, OnboardingPlan: observation.OnboardingPlanPath, StoreBackups: observation.Store.Backups, Plan: plan}
	if document.StoreBackups == nil {
		document.StoreBackups = []string{}
	}
	if !writeJSONDocument(output, errorsOutput, document) {
		return 1
	}
	if plan.Result == upgrade.ResultBlocked {
		return 1
	}
	return 0
}

// runUpgradeMigrateStore runs the explicit store migration and writes its
// outcome. It exits 0 when the store is current afterwards.
func runUpgradeMigrateStore(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("upgrade migrate-store", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration of the installation")
	asJSON := flags.Bool("json", false, operationJSONUsage+"; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return 2
	}
	if !*asJSON {
		writeError(errorsOutput, errors.New("upgrade migrate-store needs --json"))
		return 2
	}
	document := migrateStoreDocument{SchemaVersion: migrateStoreSchema, Command: "upgrade migrate-store", Refusals: []store.UpgradeFinding{}}
	host, err := config.LoadHost(*configPath)
	if err == nil && len(host.Repositories) == 0 {
		err = errors.New("the host configuration registers no repository")
	}
	var result factory.StoreMigrationResult
	if err == nil {
		result, err = factory.MigrateStore(ctx, host.Repositories[0])
	}
	if err != nil {
		document.Outcome, document.Error = "error", newErrorDocument("upgrade.migrate_store_failed", doctor.OriginUnknown, err)
		writeJSONDocument(output, errorsOutput, document)
		return 1
	}
	document.RolledBack = result.RolledBack
	if len(result.Refusals) != 0 {
		document.Outcome, document.Refusals = "refused", result.Refusals
		writeJSONDocument(output, errorsOutput, document)
		return 1
	}
	document.Outcome, document.Migration = "migrated", &result.Migration
	if result.Migration.FromVersion == result.Migration.ToVersion {
		document.Outcome = "current"
	}
	if !writeJSONDocument(output, errorsOutput, document) {
		return 1
	}
	return 0
}

// observeUpgrade reads every fact the plan needs. It only reads. An error is
// returned only when the target binary itself cannot be located.
func observeUpgrade(ctx context.Context, configPath, onboardingPlan, resources string) (upgrade.Observation, error) {
	command, err := factoryCommand()
	if err != nil {
		return upgrade.Observation{}, err
	}
	identity := upgradeIdentityForCLI()
	observation := upgrade.Observation{
		Target:         upgrade.Target{Version: identity.Version, Build: identity.Build, Revision: identity.Revision, WorkerBaseImage: identity.WorkerBaseImage},
		FactoryCommand: command,
		ConfigPath:     configPath,
	}
	if absolute, err := filepath.Abs(configPath); err == nil {
		observation.ConfigPath = absolute
	}
	observation.InstalledBinary, observation.InstalledIsTarget = installedBinary(command)
	host, err := config.LoadHost(observation.ConfigPath)
	if err == nil && len(host.Repositories) == 0 {
		err = errors.New("the host configuration registers no repository")
	}
	if err != nil {
		observation.RegistrationProblem = err.Error()
		return observation, nil
	}
	registration := host.Repositories[0]
	observation.Registered, observation.RepositoryPath = true, registration.Path
	observation.Store, err = store.InspectForUpgrade(ctx, registration.OperationalDataPath)
	if err != nil {
		observation.StoreProblem = err.Error()
	}
	if record, err := factory.ObserveCoordinatorLock(registration); err != nil {
		observation.Lock.Problem = err.Error()
	} else {
		observation.Lock = upgrade.LockObservation{Held: record.Held, Holder: string(record.Holder), Version: record.Version, Revision: record.Revision}
	}
	images := upgradeImagesForCLI()
	policy := observePolicy(registration, &observation)
	if policy != nil {
		observeWorker(ctx, images, *policy, identity, &observation)
	}
	return observation, observeOnboarding(ctx, images, registration, onboardingPlan, resources, &observation)
}

// installedBinary returns the factory command that PATH names and whether it
// is the target binary.
func installedBinary(target string) (string, bool) {
	found, err := upgradeLookPathForCLI("factory")
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(found); err == nil {
		found = resolved
	}
	return found, found == target
}

// observePolicy validates factory.yaml with the target parser and returns
// it when it is valid.
func observePolicy(registration config.RepositoryRegistration, observation *upgrade.Observation) *config.RepositoryConfig {
	path := registration.RepositoryConfigPath
	if path == "" {
		path = config.DefaultRepositoryConfigPath(registration.Path)
	}
	if diagnosis := config.DiagnoseRepositoryFile(path); !diagnosis.Valid {
		observation.PolicyProblem = "factory.yaml is invalid for the target release"
		if len(diagnosis.Diagnostics) != 0 {
			first := diagnosis.Diagnostics[0]
			observation.PolicyProblem += ": " + strings.TrimSpace(first.Field+" "+first.Message)
		}
		return nil
	}
	policy, err := config.LoadRepository(path)
	if err != nil {
		observation.PolicyProblem = "factory.yaml is not readable: " + err.Error()
		return nil
	}
	observation.PolicyValid = true
	return &policy
}

// observeWorker reads the pinned worker image, its release label, and the
// presence of the target worker base image. It never pulls.
func observeWorker(ctx context.Context, images upgradeImages, policy config.RepositoryConfig, identity version.Identity, observation *upgrade.Observation) {
	pinned := worker.ImageReference{Name: policy.WorkerBuild.Image, Digest: policy.WorkerBuild.Digest}
	observation.Worker.Image = pinned.Name + "@" + pinned.Digest
	if err := images.CheckImage(ctx, pinned); err != nil {
		observation.Worker.Absence = err.Error()
	} else {
		observation.Worker.Present = true
		if observation.Worker.Release, err = images.ImageRelease(ctx, pinned); err != nil {
			observation.Worker.Problem = "the release label of the pinned worker image is not readable: " + err.Error()
		}
	}
	candidates := baseImageCandidates(policy, identity)
	if len(candidates) == 0 {
		return
	}
	observation.BaseImage.Reference = candidates[0]
	for _, candidate := range candidates {
		name, digest, _ := strings.Cut(candidate, "@")
		err := images.CheckImage(ctx, worker.ImageReference{Name: name, Digest: digest})
		if err == nil {
			observation.BaseImage = upgrade.ImageObservation{Reference: candidate, Present: true}
			return
		}
		observation.BaseImage.Absence = err.Error()
	}
}

// baseImageCandidates returns the target worker base images that the
// repository can build on: the variant of its only selectable harness, if
// the release names one, and the combined image.
func baseImageCandidates(policy config.RepositoryConfig, identity version.Identity) []string {
	var selectable []string
	for _, name := range []config.Harness{config.HarnessClaude, config.HarnessCodex, config.HarnessCopilot} {
		if harness.RequiresSkillSmoke(&policy, string(name)) {
			selectable = append(selectable, string(name))
		}
	}
	var candidates []string
	if len(selectable) == 1 && identity.WorkerBaseImages[selectable[0]] != "" {
		candidates = append(candidates, identity.WorkerBaseImages[selectable[0]])
	}
	if identity.WorkerBaseImage != "" {
		candidates = append(candidates, identity.WorkerBaseImage)
	}
	return candidates
}

// observeOnboarding evaluates the named onboarding plan read-only against
// the target, so its valid evidence is reused. Without a plan, it still
// observes the skill smoke of the pinned worker. The prepared resources are
// verified on their own, so preparing them advances the plan before an
// onboarding plan exists.
func observeOnboarding(ctx context.Context, images upgradeImages, registration config.RepositoryRegistration, planPath, resources string, observation *upgrade.Observation) error {
	observer, err := newOnboardingObserver()
	if err != nil {
		return err
	}
	observer.Images = images
	plan := onboarding.NewPlan(registration.Path, observation.ConfigPath, "")
	if planPath != "" {
		if observation.OnboardingPlanPath, err = filepath.Abs(planPath); err != nil {
			return fmt.Errorf("resolve %s: %w", planPath, err)
		}
		loaded, err := onboarding.Load(observation.OnboardingPlanPath)
		switch {
		case err != nil:
			observation.OnboardingPlanProblem = err.Error()
		case !samePlanRepository(loaded.Repository, registration.Path):
			observation.OnboardingPlanProblem = "the onboarding plan belongs to another repository, " + loaded.Repository
		case !samePlanRepository(loaded.HostConfig, observation.ConfigPath):
			observation.OnboardingPlanForeign = "the onboarding plan uses the host configuration " + loaded.HostConfig + ", not " + observation.ConfigPath
		default:
			plan = loaded
		}
	}
	if resources != "" {
		if plan.Resources, err = filepath.Abs(resources); err != nil {
			return fmt.Errorf("resolve %s: %w", resources, err)
		}
	}
	current := observer.Observe(ctx, plan)
	observation.Smoke = current.Smoke
	observation.Resources = upgrade.ResourcesObservation{Directory: plan.Resources, Verified: current.Resources.Done, Problem: current.Resources.Problem}
	if observation.OnboardingPlanPath == "" || observation.OnboardingPlanProblem != "" || observation.OnboardingPlanForeign != "" {
		return nil
	}
	current.PlanPath, current.FactoryCommand = observation.OnboardingPlanPath, observation.FactoryCommand
	observation.Onboarding = onboarding.Evaluate(plan, current).Steps
	return nil
}
