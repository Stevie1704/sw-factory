package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Stevie1704/sw-factory/internal/gateproof"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// proofRuntimeForCLI is the worker runtime seam of onboard verify, so the CLI
// contract can be tested without Docker.
var proofRuntimeForCLI = func() worker.CleanupRuntime { return worker.NewDockerRuntime() }

// proofCleanupSchema is the version of the onboard verify --cleanup document.
const proofCleanupSchema = 1

// proofCleanupDocument is the JSON result of onboard verify --cleanup.
type proofCleanupDocument struct {
	SchemaVersion int `json:"schema_version"`
	gateproof.CleanupResult
}

// runOnboardVerify proves the candidate repository's setup and gates in its
// pinned worker, or removes the leftovers of interrupted proofs. It needs no
// host registration, claims no issue, starts no agent, and publishes nothing.
// It exits 0 only for a passed proof or a complete cleanup.
func runOnboardVerify(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("onboard verify", errorsOutput)
	repository := flags.String("repository", "", "Git checkout of the candidate repository to prove")
	cleanup := flags.Bool("cleanup", false, "remove the workers and workspaces of interrupted proofs instead of proving")
	asJSON := flags.Bool("json", false, "write the result as one schema-versioned JSON document; required")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("onboard verify does not accept positional arguments"))
		return 2
	}
	if !*asJSON || (*repository == "") == !*cleanup {
		writeError(errorsOutput, errors.New("onboard verify needs either --repository <path> or --cleanup, and --json"))
		return 2
	}
	root, err := proofRoot()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	verifier := gateproof.Verifier{Runtime: proofRuntimeForCLI(), Root: root}
	if *cleanup {
		return writeProofCleanup(ctx, verifier, output, errorsOutput)
	}
	path, err := filepath.Abs(*repository)
	if err != nil {
		writeError(errorsOutput, fmt.Errorf("resolve the repository path: %w", err))
		return 1
	}
	report := verifier.Verify(ctx, path)
	if !writeJSONDocument(output, errorsOutput, report) || report.Result != gateproof.ResultPassed {
		return 1
	}
	return 0
}

// writeProofCleanup removes leftover proofs and writes what it removed.
func writeProofCleanup(ctx context.Context, verifier gateproof.Verifier, output, errorsOutput io.Writer) int {
	result, err := verifier.Cleanup(ctx)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeJSONDocument(output, errorsOutput, proofCleanupDocument{SchemaVersion: proofCleanupSchema, CleanupResult: result}) || len(result.Failed) != 0 {
		return 1
	}
	return 0
}

// proofRoot returns the directory that holds proof workspaces. It lies in the
// user cache directory below the operator's home, which a VM-backed Docker
// daemon can bind-mount.
func proofRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate the user cache directory for the proof workspace; set HOME: %w", err)
	}
	return filepath.Join(cache, "sw-factory", "onboard-proof"), nil
}
