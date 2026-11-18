package factory

import (
	"context"
	"errors"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// StoreMigrationResult is the outcome of the explicit store migration.
type StoreMigrationResult struct {
	// Migration is what ran; it is empty when a refusal exists.
	Migration store.Migration
	// RolledBack reports that an interrupted write was rolled back first.
	RolledBack bool
	// Refusals are the reasons why nothing migrated. Their codes match the
	// upgrade plan blockers.
	Refusals []store.UpgradeFinding
}

// MigrateStore is the supported store migration entry point of an upgrade.
// It holds the coordinator lock for its whole duration, so no coordinator
// can start, rolls back an interrupted write, and refuses unless the store
// passes the migration gate and its schema is supported. The gate includes
// the worker-activity check through workers, so no harness process of the
// old release still runs. Only then does it back up and migrate the store in
// one transaction.
func MigrateStore(ctx context.Context, registration config.RepositoryRegistration, workers WorkerProbe) (StoreMigrationResult, error) {
	lock, err := acquireCoordinatorLock(coordinatorLockPath(registration), LockHolderStoreMigration)
	if errors.Is(err, ErrCoordinatorAlreadyRunning) {
		return StoreMigrationResult{Refusals: []store.UpgradeFinding{{Code: "coordinator.running", Detail: "a live coordinator holds the repository lock"}}}, nil
	}
	if err != nil {
		return StoreMigrationResult{}, err
	}
	defer func() { _ = lock.release() }()
	path := registration.OperationalDataPath
	inspection, err := store.InspectForUpgrade(ctx, path)
	if err != nil {
		return StoreMigrationResult{}, fmt.Errorf("inspect the operational store: %w", err)
	}
	var result StoreMigrationResult
	if inspection.InterruptedWrite {
		if err := store.RollBackInterruptedWrite(ctx, path); err != nil {
			return StoreMigrationResult{}, fmt.Errorf("roll back the interrupted store write: %w", err)
		}
		result.RolledBack = true
		if inspection, err = store.InspectForUpgrade(ctx, path); err != nil {
			return StoreMigrationResult{}, fmt.Errorf("inspect the operational store: %w", err)
		}
	}
	if result.Refusals = migrationRefusals(ctx, inspection, workers); len(result.Refusals) != 0 {
		return result, nil
	}
	identity := version.Current()
	release := store.ReleaseIdentity{Version: identity.Version, Revision: identity.Revision}
	if result.Migration, err = store.Migrate(ctx, path, release); err != nil {
		return StoreMigrationResult{}, err
	}
	return result, nil
}

// migrationRefusals lists every reason why the inspected store must not be
// migrated now.
func migrationRefusals(ctx context.Context, inspection store.UpgradeInspection, workers WorkerProbe) []store.UpgradeFinding {
	if finding := inspection.SchemaFinding(); finding != nil {
		return []store.UpgradeFinding{*finding}
	}
	return append(inspection.ActivityFindings(), WorkerActivityFindings(ctx, inspection.WorkerProcesses, workers)...)
}
