package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/version"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// checkpointCreateSchema is the version of the checkpoint create document.
const checkpointCreateSchema = 1

// checkpointCreateDocument is the JSON result of checkpoint create.
type checkpointCreateDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	Command       string                 `json:"command"`
	Outcome       string                 `json:"outcome"`
	Checkpoint    *checkpointDocument    `json:"checkpoint,omitempty"`
	Refusals      []store.UpgradeFinding `json:"refusals"`
	Error         *errorDocument         `json:"error,omitempty"`
}

// checkpointDocument names one created checkpoint and its manifest.
type checkpointDocument struct {
	Path     string              `json:"path"`
	Manifest checkpoint.Manifest `json:"manifest"`
}

// checkpointInspectSchema is the version of the checkpoint inspect document.
const checkpointInspectSchema = 1

// checkpointInspectDocument is the JSON result of checkpoint inspect.
// Verified is true only for the verified outcome.
type checkpointInspectDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	Command       string                 `json:"command"`
	Outcome       string                 `json:"outcome"`
	Verified      bool                   `json:"verified"`
	Checkpoint    *checkpointDocument    `json:"checkpoint,omitempty"`
	Refusals      []store.UpgradeFinding `json:"refusals"`
	Error         *errorDocument         `json:"error,omitempty"`
}

// checkpointExternalForCLI is the external state seam: it reads the work
// tracker, the code host, and the Git remote of one registration. A test
// replaces it so the CLI contract runs without network access.
var checkpointExternalForCLI = func(configPath string, registration config.RepositoryRegistration, targetBranch string) checkpoint.ExternalReader {
	return factory.NewExternalStateReader(registration, checkpointSources(configPath, targetBranch))
}

// checkpointOwnedForCLI is the owned-object seam of restore: it lists the
// factory-owned objects of one registration. A test replaces it so the CLI
// contract runs without network access.
var checkpointOwnedForCLI = func(configPath string, registration config.RepositoryRegistration, targetBranch string) checkpoint.OwnedObjectLister {
	return factory.NewOwnedObjectLister(registration, checkpointSources(configPath, targetBranch))
}

// checkpointSources returns the tracker, code host, and remote read seams
// of the configured installation.
func checkpointSources(configPath, targetBranch string) factory.ExternalStateSources {
	dependencies := serviceDependencies(configPath)
	pullRequests := dependencies.PullRequests
	if pullRequests == nil {
		pullRequests, _ = dependencies.Tracker.(codehost.PullRequestClient)
	}
	return factory.ExternalStateSources{
		Tracker:      dependencies.Tracker,
		PullRequests: pullRequests,
		Remote:       gitadapter.NewWorktreeManager(),
		TargetBranch: targetBranch,
	}
}

// checkpointWorkersForCLI is the Docker seam of the worker-activity
// condition, so the CLI contract can be tested without Docker.
var checkpointWorkersForCLI = func() factory.WorkerProbe { return worker.NewDockerRuntime() }

// checkpointContainersForCLI is the Docker seam that lists every running
// worker container for a restore over a live store that does not open.
var checkpointContainersForCLI = func() factory.WorkerContainerLister { return worker.NewDockerRuntime() }

// runCheckpoint dispatches the checkpoint subcommands.
func runCheckpoint(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	if len(args) > 0 && args[0] == "create" {
		return runCheckpointCreate(ctx, args[1:], defaultConfigPath, output, errorsOutput)
	}
	if len(args) > 0 && args[0] == "inspect" {
		return runCheckpointInspect(ctx, args[1:], output, errorsOutput)
	}
	if len(args) > 0 && args[0] == "restore" {
		return runCheckpointRestore(ctx, args[1:], defaultConfigPath, output, errorsOutput)
	}
	writeError(errorsOutput, errors.New("checkpoint needs a subcommand: create, inspect, or restore; read factory guide maintenance"))
	return 2
}

// runCheckpointInspect verifies one checkpoint directory and writes its
// manifest. It needs no configuration and changes no file. It exits 0 when
// the checkpoint verifies, and 1 for a refusal or an error.
func runCheckpointInspect(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("checkpoint inspect", errorsOutput)
	path := flags.String("path", "", "checkpoint directory to verify; required")
	asJSON := flags.Bool("json", false, operationJSONUsage+"; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return 2
	}
	if !*asJSON || *path == "" {
		writeError(errorsOutput, errors.New("checkpoint inspect needs --path and --json"))
		return 2
	}
	document := checkpointInspectDocument{SchemaVersion: checkpointInspectSchema, Command: "checkpoint inspect", Refusals: []store.UpgradeFinding{}}
	manifest, err := checkpoint.Verify(ctx, *path)
	var refusal *checkpoint.VerificationError
	switch {
	case errors.As(err, &refusal):
		document.Outcome, document.Refusals = "refused", []store.UpgradeFinding{{Code: refusal.Code, Detail: refusal.Detail}}
	case err != nil:
		document.Outcome, document.Error = "error", newErrorDocument("checkpoint.inspect_failed", doctor.OriginUnknown, err)
	default:
		document.Outcome, document.Verified, document.Checkpoint = "verified", true, &checkpointDocument{Path: *path, Manifest: manifest}
	}
	if !writeJSONDocument(output, errorsOutput, document) || document.Outcome != "verified" {
		return 1
	}
	return 0
}

// runCheckpointCreate creates one maintenance checkpoint and writes its
// outcome. It exits 0 when the checkpoint exists, and 1 for a refusal or an
// error.
func runCheckpointCreate(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("checkpoint create", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration of the installation")
	destination := flags.String("output", "", "new private directory for the checkpoint; required")
	asJSON := flags.Bool("json", false, operationJSONUsage+"; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return 2
	}
	if !*asJSON || *destination == "" {
		writeError(errorsOutput, errors.New("checkpoint create needs --output and --json"))
		return 2
	}
	document := checkpointCreateDocument{SchemaVersion: checkpointCreateSchema, Command: "checkpoint create", Refusals: []store.UpgradeFinding{}}
	result, err := createCheckpoint(ctx, *configPath, *destination)
	switch {
	case err != nil:
		document.Outcome, document.Error = "error", newErrorDocument("checkpoint.create_failed", doctor.OriginUnknown, err)
	case len(result.Refusals) != 0:
		document.Outcome, document.Refusals = "refused", result.Refusals
	default:
		document.Outcome, document.Checkpoint = "created", &checkpointDocument{Path: result.Path, Manifest: *result.Manifest}
	}
	if !writeJSONDocument(output, errorsOutput, document) || document.Outcome != "created" {
		return 1
	}
	return 0
}

// checkpointInstallation is the registration and repository policy of one
// host configuration.
type checkpointInstallation struct {
	configPath   string
	registration config.RepositoryRegistration
	policy       config.RepositoryConfig
}

// loadCheckpointInstallation reads the first registration and its policy.
func loadCheckpointInstallation(configPath string) (checkpointInstallation, error) {
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return checkpointInstallation{}, fmt.Errorf("resolve %s: %w", configPath, err)
	}
	host, err := config.LoadHost(absoluteConfig)
	if err == nil && len(host.Repositories) == 0 {
		err = errors.New("the host configuration registers no repository")
	}
	if err != nil {
		return checkpointInstallation{}, err
	}
	registration := host.Repositories[0]
	policyPath := registration.RepositoryConfigPath
	if policyPath == "" {
		policyPath = config.DefaultRepositoryConfigPath(registration.Path)
	}
	policy, err := config.LoadRepository(policyPath)
	if err != nil {
		return checkpointInstallation{}, fmt.Errorf("read factory.yaml: %w", err)
	}
	return checkpointInstallation{configPath: absoluteConfig, registration: registration, policy: policy}, nil
}

// createCheckpoint loads the registration and its policy and creates the
// checkpoint.
func createCheckpoint(ctx context.Context, configPath, destination string) (factory.CheckpointResult, error) {
	installation, err := loadCheckpointInstallation(configPath)
	if err != nil {
		return factory.CheckpointResult{}, err
	}
	absoluteConfig, registration, policy := installation.configPath, installation.registration, installation.policy
	return factory.CreateCheckpoint(ctx, factory.CheckpointRequest{
		ConfigPath:   absoluteConfig,
		Registration: registration,
		Output:       destination,
		TargetBranch: policy.TargetBranch,
		WorkerImage:  policy.WorkerBuild.Image + "@" + policy.WorkerBuild.Digest,
		Build:        version.Current(),
		External:     checkpointExternalForCLI(absoluteConfig, registration, policy.TargetBranch),
		Workers:      checkpointWorkersForCLI(),
		Now:          time.Now,
	})
}

// checkpointRestoreSchema is the version of the checkpoint restore document.
const checkpointRestoreSchema = 1

// checkpointRestoreDocument is the JSON result of checkpoint restore.
type checkpointRestoreDocument struct {
	SchemaVersion int                     `json:"schema_version"`
	Command       string                  `json:"command"`
	Outcome       string                  `json:"outcome"`
	Plan          *factory.RestorePlan    `json:"plan,omitempty"`
	Refusals      []store.UpgradeFinding  `json:"refusals"`
	Journal       *restoreJournalDocument `json:"journal,omitempty"`
	Error         *errorDocument          `json:"error,omitempty"`
}

// restoreJournalDocument reports the continuation of an interrupted
// restore: the journal state it found and what it reached.
type restoreJournalDocument struct {
	Path   string `json:"path"`
	State  string `json:"state"`
	Result string `json:"result"`
}

// runCheckpointRestore previews the restore of one checkpoint, or applies
// it with --confirm and the digest of the previewed plan. It exits 0 for a
// preview and a restore, and 1 for a refusal, a continued earlier restore,
// or an error.
func runCheckpointRestore(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("checkpoint restore", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration of the installation")
	path := flags.String("path", "", "checkpoint directory to restore; required")
	confirm := flags.String("confirm", "", "digest of the previewed plan; without it, restore only previews")
	asJSON := flags.Bool("json", false, operationJSONUsage+"; required")
	if !parseOnboardFlags(flags, args, errorsOutput) {
		return 2
	}
	if !*asJSON || *path == "" {
		writeError(errorsOutput, errors.New("checkpoint restore needs --path and --json"))
		return 2
	}
	document := checkpointRestoreDocument{SchemaVersion: checkpointRestoreSchema, Command: "checkpoint restore", Refusals: []store.UpgradeFinding{}}
	result, err := restoreCheckpoint(ctx, *configPath, *path, *confirm)
	document.Plan = result.Plan
	switch {
	case err != nil:
		document.Outcome, document.Error = "error", newErrorDocument("checkpoint.restore_failed", doctor.OriginUnknown, err)
	case result.Outcome == factory.RestoreOutcomeRefused:
		document.Outcome, document.Refusals = result.Outcome, result.Refusals
	default:
		document.Outcome = result.Outcome
	}
	if result.Journal != nil {
		document.Journal = &restoreJournalDocument{Path: store.RestoreJournalPath(result.Journal.Journal.StorePath), State: result.Journal.State, Result: result.Journal.Result}
	}
	if !writeJSONDocument(output, errorsOutput, document) || (document.Outcome != factory.RestoreOutcomePreview && document.Outcome != factory.RestoreOutcomeRestored) {
		return 1
	}
	return 0
}

// restoreCheckpoint loads the registration and its policy and previews or
// applies the restore.
func restoreCheckpoint(ctx context.Context, configPath, path, confirm string) (factory.RestoreResult, error) {
	installation, err := loadCheckpointInstallation(configPath)
	if err != nil {
		return factory.RestoreResult{}, err
	}
	targetBranch := installation.policy.TargetBranch
	return factory.RestoreCheckpoint(ctx, factory.RestoreRequest{
		ConfigPath:   installation.configPath,
		Registration: installation.registration,
		Path:         path,
		Confirm:      confirm,
		Build:        version.Current(),
		External:     checkpointExternalForCLI(installation.configPath, installation.registration, targetBranch),
		Owned:        checkpointOwnedForCLI(installation.configPath, installation.registration, targetBranch),
		Workers:      checkpointWorkersForCLI(),
		Containers:   checkpointContainersForCLI(),
	})
}
