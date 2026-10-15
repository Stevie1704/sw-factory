package factory

import (
	"context"
	"fmt"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// WithSessionRoles opens the operational store and calls use with a
// session-role coordinator for the registered repository. The session
// command dispatcher (#267) starts and cancels roles through it. A store
// without session support skips use.
func (s *Service) WithSessionRoles(ctx context.Context, use func(*SessionRoles) error) error {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, err := s.registration()
	if err != nil {
		return err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return fmt.Errorf("open operational store for session roles: %w", err)
	}
	defer func() { _ = opened.Close() }()
	sessionStore, ok := opened.(SessionRoleStore)
	if !ok {
		return nil
	}
	policy, err := s.deps.LoadRepository(registration.RepositoryConfigPath)
	if err != nil {
		return fmt.Errorf("load repository configuration for session roles: %w", err)
	}
	snapshots, _ := s.deps.Worktree.(gitadapter.ContextSnapshotter)
	return use(NewSessionRoles(SessionRoleDependencies{
		Store: sessionStore, Harnesses: s.deps.HeadlessHarnesses, Workers: s.deps.Worker, Snapshots: snapshots,
		Registration: registration, Policy: policy, Now: s.deps.Now, NewInvocationID: s.newInvocationID,
	}))
}

// ReconcileSessionRoles observes every active session-role invocation once.
// The polling supervisor calls it on every pass, so a role result is applied
// and a waiting session is released without a browser or a terminal.
func (s *Service) ReconcileSessionRoles(ctx context.Context) error {
	return s.WithSessionRoles(ctx, func(roles *SessionRoles) error { return roles.Reconcile(ctx) })
}
