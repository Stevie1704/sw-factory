package factory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// destructiveWorkerRoles covers every factory-declared role that can create a
// run-scoped worker home, including deterministic gate execution.
var destructiveWorkerRoles = factoryWorkerRoles()

// factoryWorkerRoles derives destructive role identities from the factory
// registry so a newly declared role cannot leave its worker home
// behind.
func factoryWorkerRoles() []string {
	roles := []string{"gate"}
	for _, definition := range workflow.DefaultRegistry().Roles() {
		roles = append(roles, definition.Name)
	}
	return roles
}

// runLocalResources is the complete set of exact local resources one persisted
// run owns. Every field is derived from validated persisted identities and the
// registered absolute paths, never from a filesystem or Docker prefix scan, so
// a malformed row cannot widen a destructive operation.
type runLocalResources struct {
	// Branch is the local factory branch. Remote branches are never included.
	Branch string
	// Worktree is the exact local worktree path.
	Worktree string
	// WorkerIDs contains the exact worker container identities for this run.
	WorkerIDs []string
	// StoredOutputs contains the exact generated invocation and result
	// directories.
	StoredOutputs []string
	// Roles selects the run-scoped role-home volumes.
	Roles []string
	// CredentialStoreIDs contains the persisted factory-managed credential
	// store identities this run mounted. Ordinary cleanup retains them.
	CredentialStoreIDs []string
	// UnsafeCredentialStore explains why a persisted credential-store identity
	// could not be validated, or is empty when every identity is safe. It is a
	// report rather than a rejection, because cleanup never addresses credential
	// storage and must not start failing on an identity it does not use.
	UnsafeCredentialStore string
}

// validateRunLocalResources derives one run's exact local resources and returns
// a bounded operator-facing reason when any persisted identity, path, or
// pending effect makes destruction unsafe. It is fail-closed: an identity that
// cannot be proven run-scoped rejects the whole run.
func validateRunLocalResources(registration config.RepositoryRegistration, candidate store.RunRemovalCandidate) (runLocalResources, string) {
	run := candidate.Run
	if !safeLocalIdentifier(run.ID) {
		return runLocalResources{}, "run identifier is not a safe local identifier"
	}
	if candidate.PendingEffect != nil {
		return runLocalResources{}, fmt.Sprintf("pending external effect %q requires reconciliation", candidate.PendingEffect.ID)
	}
	if resolvePath(run.RepositoryPath) != resolvePath(registration.Path) {
		return runLocalResources{}, "run repository does not match the registered repository"
	}
	expectedBranch := "factory/" + run.ID
	if run.Branch != expectedBranch {
		return runLocalResources{}, fmt.Sprintf("branch %q is not the run-owned branch %q", run.Branch, expectedBranch)
	}
	if !filepath.IsAbs(run.Worktree) || filepath.Base(filepath.Clean(run.Worktree)) != run.ID {
		return runLocalResources{}, "worktree is not an absolute run-scoped path"
	}
	worktree := filepath.Clean(run.Worktree)
	if pathWithin(registration.Path, worktree) || pathWithin(worktree, registration.Path) {
		return runLocalResources{}, "worktree overlaps the registered repository"
	}
	if reason := removableDirectoryTarget(worktree, "worktree"); reason != "" {
		return runLocalResources{}, reason
	}

	storedOutputs := make([]string, 0, len(candidate.Invocations)*2)
	seenOutputs := make(map[string]struct{}, len(candidate.Invocations)*2)
	for _, invocation := range candidate.Invocations {
		expectedRoot := invocationRoot(run, invocation.ID)
		for _, target := range []struct {
			path string
			want string
		}{
			{path: invocation.InvocationDirectory, want: filepath.Join(expectedRoot, "packet")},
			{path: invocation.ResultDirectory, want: filepath.Join(expectedRoot, "results")},
		} {
			path := target.path
			if path == "" {
				continue
			}
			clean := filepath.Clean(path)
			if clean != filepath.Clean(target.want) {
				return runLocalResources{}, fmt.Sprintf("invocation %q has an output path outside its owned directory", invocation.ID)
			}
			if pathWithin(registration.Path, clean) {
				return runLocalResources{}, "stored output overlaps the registered repository"
			}
			if _, exists := seenOutputs[clean]; !exists {
				seenOutputs[clean] = struct{}{}
				storedOutputs = append(storedOutputs, clean)
			}
		}
	}
	if err := worker.ValidateCleanupStoredOutputs(run.ID, storedOutputs); err != nil {
		return runLocalResources{}, err.Error()
	}
	sort.Strings(storedOutputs)

	roles := append([]string(nil), destructiveWorkerRoles...)
	roleSet := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		roleSet[role] = struct{}{}
	}
	workerIDs := []string{run.ID}
	seenWorkerIDs := map[string]struct{}{run.ID: {}}
	var credentialStoreIDs []string
	unsafeCredentialStore := ""
	seenCredentialStoreIDs := make(map[string]struct{}, len(candidate.Invocations))
	for _, invocation := range candidate.Invocations {
		if !safeLocalIdentifier(invocation.Role) {
			return runLocalResources{}, fmt.Sprintf("invocation %q has an unsafe worker role", invocation.ID)
		}
		workerID := workerIDForInvocation(invocation)
		if !safeLocalIdentifier(workerID) {
			return runLocalResources{}, fmt.Sprintf("invocation %q has an unsafe worker identity", invocation.ID)
		}
		if _, exists := seenWorkerIDs[workerID]; !exists {
			seenWorkerIDs[workerID] = struct{}{}
			workerIDs = append(workerIDs, workerID)
		}
		if _, exists := roleSet[invocation.Role]; !exists {
			roleSet[invocation.Role] = struct{}{}
			roles = append(roles, invocation.Role)
		}
		if storeID := invocation.CredentialStoreID; storeID != "" {
			if storeID != registration.Path {
				unsafeCredentialStore = fmt.Sprintf("invocation %q has a credential store identity that does not match the registered repository", invocation.ID)
			} else if _, exists := seenCredentialStoreIDs[storeID]; !exists {
				seenCredentialStoreIDs[storeID] = struct{}{}
				credentialStoreIDs = append(credentialStoreIDs, storeID)
			}
		}
	}
	sort.Strings(workerIDs)
	sort.Strings(roles)
	sort.Strings(credentialStoreIDs)
	return runLocalResources{
		Branch:                run.Branch,
		Worktree:              worktree,
		WorkerIDs:             workerIDs,
		StoredOutputs:         storedOutputs,
		Roles:                 roles,
		CredentialStoreIDs:    credentialStoreIDs,
		UnsafeCredentialStore: unsafeCredentialStore,
	}, ""
}

// removableDirectoryTarget verifies that an exact planned directory is safe to
// remove. Missing directories are valid because a prior partial removal may
// already have removed them.
func removableDirectoryTarget(path, label string) string {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return label + " is not a safe absolute directory"
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("inspect %s: %v", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return label + " must not be a symbolic link"
	}
	if !info.IsDir() {
		return label + " is not a directory"
	}
	return ""
}

// safeLocalIdentifier accepts only one path component suitable for both
// generated directory names and the worker runtime's run identity.
func safeLocalIdentifier(value string) bool {
	if value == "" || value == "." || value == ".." || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}
