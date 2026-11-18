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

// checkpointExternalForCLI is the external state seam: it reads the work
// tracker, the code host, and the Git remote of one registration. A test
// replaces it so the CLI contract runs without network access.
var checkpointExternalForCLI = func(configPath string, registration config.RepositoryRegistration, targetBranch string) checkpoint.ExternalReader {
	dependencies := serviceDependencies(configPath)
	pullRequests := dependencies.PullRequests
	if pullRequests == nil {
		pullRequests, _ = dependencies.Tracker.(codehost.PullRequestClient)
	}
	return factory.NewExternalStateReader(registration, factory.ExternalStateSources{
		Tracker:      dependencies.Tracker,
		PullRequests: pullRequests,
		Remote:       gitadapter.NewWorktreeManager(),
		TargetBranch: targetBranch,
	})
}

// runCheckpoint dispatches the checkpoint subcommands.
func runCheckpoint(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	if len(args) > 0 && args[0] == "create" {
		return runCheckpointCreate(ctx, args[1:], defaultConfigPath, output, errorsOutput)
	}
	writeError(errorsOutput, errors.New("checkpoint needs a subcommand: create; read factory guide maintenance"))
	return 2
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

// createCheckpoint loads the registration and its policy and creates the
// checkpoint.
func createCheckpoint(ctx context.Context, configPath, destination string) (factory.CheckpointResult, error) {
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return factory.CheckpointResult{}, fmt.Errorf("resolve %s: %w", configPath, err)
	}
	host, err := config.LoadHost(absoluteConfig)
	if err == nil && len(host.Repositories) == 0 {
		err = errors.New("the host configuration registers no repository")
	}
	if err != nil {
		return factory.CheckpointResult{}, err
	}
	registration := host.Repositories[0]
	policyPath := registration.RepositoryConfigPath
	if policyPath == "" {
		policyPath = config.DefaultRepositoryConfigPath(registration.Path)
	}
	policy, err := config.LoadRepository(policyPath)
	if err != nil {
		return factory.CheckpointResult{}, fmt.Errorf("read factory.yaml: %w", err)
	}
	return factory.CreateCheckpoint(ctx, factory.CheckpointRequest{
		ConfigPath:   absoluteConfig,
		Registration: registration,
		Output:       destination,
		TargetBranch: policy.TargetBranch,
		WorkerImage:  policy.WorkerBuild.Image + "@" + policy.WorkerBuild.Digest,
		Build:        version.Current(),
		External:     checkpointExternalForCLI(absoluteConfig, registration, policy.TargetBranch),
		Now:          time.Now,
	})
}
