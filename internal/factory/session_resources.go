package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// SessionResourceStore is the optional store seam that lists the ideation
// sessions of the registered repository and their role invocations. Reset
// reads it to find session-role resources; a store without it has none.
type SessionResourceStore interface {
	IdeationSessions(context.Context, string) ([]store.IdeationSession, error)
	SessionInvocations(context.Context, string) ([]store.SessionInvocation, error)
}

// ResetSession describes the host resources the roles of one ideation
// session leave: the worker containers and role volumes, the snapshot
// worktree and Git metadata projection of a role that was active when the
// coordinator stopped, and the session packet directory.
type ResetSession struct {
	// SessionID identifies the session.
	SessionID string
	// WorkerRunID is the worker owner key that names the session's worker
	// resources.
	WorkerRunID string
	// WorkerIDs are the session-role workers.
	WorkerIDs []string
	// Roles select the role volumes of those workers.
	Roles []string
	// Snapshots are the snapshot worktrees that still exist.
	Snapshots []string
	// GitProjections are the snapshot Git metadata projections that still
	// exist.
	GitProjections []string
	// PacketRoots are the session packet directories that still exist.
	PacketRoots []string
	// credentialStoreIDs are the credential stores the roles used.
	credentialStoreIDs []string
}

// listResetSessions returns the validated session-role resources of every
// session with at least one role invocation. A session whose persisted paths
// do not have the shape the coordinator creates is returned as a reason, so
// reset never removes a path it did not derive.
func listResetSessions(ctx context.Context, sessions SessionResourceStore, registration config.RepositoryRegistration) ([]ResetSession, []ResetBlocker, error) {
	stored, err := sessions.IdeationSessions(ctx, registration.Path)
	if err != nil {
		return nil, nil, err
	}
	var planned []ResetSession
	var blockers []ResetBlocker
	for _, session := range stored {
		invocations, err := sessions.SessionInvocations(ctx, session.ID)
		if err != nil {
			return nil, nil, err
		}
		if len(invocations) == 0 {
			continue
		}
		resources, reason := validateSessionResources(registration, session.ID, invocations)
		if reason != "" {
			blockers = append(blockers, ResetBlocker{
				Reason: fmt.Sprintf("session %s: %s", session.ID, reason),
				Action: "resolve the session's persisted state, then repeat factory reset",
			})
			continue
		}
		planned = append(planned, resources)
	}
	return planned, blockers, nil
}

// validateSessionResources derives the exact resources of one session from
// its invocations and refuses any persisted identity or path that the
// coordinator would not have created.
func validateSessionResources(registration config.RepositoryRegistration, sessionID string, invocations []store.SessionInvocation) (ResetSession, string) {
	if !safeLocalIdentifier(sessionID) {
		return ResetSession{}, "session identifier is not a safe local identifier"
	}
	resources := ResetSession{SessionID: sessionID, WorkerRunID: sessionWorkerOwner(sessionID)}
	workers, roles, credentials, packets := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, invocation := range invocations {
		if !safeLocalIdentifier(invocation.ID) || !safeLocalIdentifier(invocation.Role) {
			return ResetSession{}, fmt.Sprintf("invocation %q has an unsafe identity or role", invocation.ID)
		}
		if invocation.WorkerID != sessionWorkerID(sessionID, invocation.Role) {
			return ResetSession{}, fmt.Sprintf("invocation %q has a worker identity outside the session", invocation.ID)
		}
		workers[invocation.WorkerID], roles[invocation.Role] = true, true
		if storeID := invocation.CredentialStoreID; storeID != "" {
			if storeID != registration.Path {
				return ResetSession{}, fmt.Sprintf("invocation %q has a credential store identity that does not match the registered repository", invocation.ID)
			}
			credentials[storeID] = true
		}
		if reason := addSessionPaths(&resources, packets, registration, sessionID, invocation); reason != "" {
			return ResetSession{}, reason
		}
	}
	resources.WorkerIDs, resources.Roles, resources.credentialStoreIDs = sortedKeys(workers), sortedKeys(roles), sortedKeys(credentials)
	resources.PacketRoots = sortedKeys(packets)
	sort.Strings(resources.Snapshots)
	sort.Strings(resources.GitProjections)
	return resources, ""
}

// addSessionPaths validates the snapshot and packet paths of one invocation
// and adds the ones that still exist to resources and to the packet roots.
func addSessionPaths(resources *ResetSession, packets map[string]bool, registration config.RepositoryRegistration, sessionID string, invocation store.SessionInvocation) string {
	if invocation.SnapshotPath == "" {
		if invocation.InvocationDirectory != "" || invocation.ResultDirectory != "" {
			return fmt.Sprintf("invocation %q has packet paths without a snapshot", invocation.ID)
		}
		return ""
	}
	snapshot := filepath.Clean(invocation.SnapshotPath)
	if !filepath.IsAbs(snapshot) || filepath.Base(snapshot) != invocation.ID {
		return fmt.Sprintf("invocation %q has a snapshot that is not an absolute invocation-scoped path", invocation.ID)
	}
	if pathWithin(registration.Path, snapshot) || pathWithin(snapshot, registration.Path) {
		return fmt.Sprintf("invocation %q has a snapshot that overlaps the registered repository", invocation.ID)
	}
	root := filepath.Join(filepath.Dir(snapshot), ".factory-sessions", sessionID)
	for _, output := range []struct{ path, want string }{
		{invocation.InvocationDirectory, filepath.Join(root, invocation.ID, "packet")},
		{invocation.ResultDirectory, filepath.Join(root, invocation.ID, "results")},
	} {
		if output.path != "" && filepath.Clean(output.path) != output.want {
			return fmt.Sprintf("invocation %q has an output path outside its session directory", invocation.ID)
		}
	}
	for _, target := range []struct {
		path  string
		label string
		add   func(string)
	}{
		{snapshot, "snapshot worktree", func(path string) { resources.Snapshots = append(resources.Snapshots, path) }},
		{gitMetadataProjectionPath(invocation.ID, snapshot), "snapshot Git metadata", func(path string) { resources.GitProjections = append(resources.GitProjections, path) }},
		{root, "session packet directory", func(path string) { packets[path] = true }},
	} {
		if reason := removableDirectoryTarget(target.path, target.label); reason != "" {
			return reason
		}
		if _, err := os.Lstat(target.path); err == nil {
			target.add(target.path)
		}
	}
	return ""
}

// sortedKeys returns the keys of set in ascending order.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
