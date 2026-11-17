package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// StoreMigrationRefusal is one reason why the explicit store migration did
// not run. Codes are stable and match the upgrade plan blockers.
type StoreMigrationRefusal struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// StoreMigrationResult is the outcome of the explicit store migration.
type StoreMigrationResult struct {
	// Migration is what ran; it is empty when a refusal exists.
	Migration store.Migration
	// RolledBack reports that an interrupted write was rolled back first.
	RolledBack bool
	Refusals   []StoreMigrationRefusal
}

// CoordinatorLockHeld reports, without changing anything, whether a live
// coordinator owns the repository lock of one registration.
func CoordinatorLockHeld(registration config.RepositoryRegistration) (bool, error) {
	return coordinatorLockHeld(coordinatorLockPath(registration))
}

// MigrateStore is the supported store migration entry point of an upgrade.
// It holds the coordinator lock for its whole duration, so no coordinator
// can start, rolls back an interrupted write, and refuses unless the store
// is quiescent and its schema is supported. Only then does it back up and
// migrate the store in one transaction.
func MigrateStore(ctx context.Context, registration config.RepositoryRegistration) (StoreMigrationResult, error) {
	lock, err := acquireCoordinatorLock(coordinatorLockPath(registration))
	if errors.Is(err, ErrCoordinatorAlreadyRunning) {
		return refuseMigration("coordinator.running", "a live coordinator holds the repository lock"), nil
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
	if refusals := migrationRefusals(inspection); len(refusals) != 0 {
		result.Refusals = refusals
		return result, nil
	}
	if result.Migration, err = store.Migrate(ctx, path); err != nil {
		return StoreMigrationResult{}, err
	}
	return result, nil
}

// refuseMigration returns a result with one refusal.
func refuseMigration(code, detail string) StoreMigrationResult {
	return StoreMigrationResult{Refusals: []StoreMigrationRefusal{{Code: code, Detail: detail}}}
}

// migrationRefusals lists every reason why the inspected store must not be
// migrated now.
func migrationRefusals(inspection store.UpgradeInspection) []StoreMigrationRefusal {
	switch {
	case !inspection.Exists:
		return []StoreMigrationRefusal{{Code: "store.missing", Detail: "no operational store exists at " + inspection.Path}}
	case inspection.SchemaVersion > store.CurrentSchemaVersion:
		return []StoreMigrationRefusal{{Code: "store.schema_newer", Detail: fmt.Sprintf("the store has schema %d; this build supports %d", inspection.SchemaVersion, store.CurrentSchemaVersion)}}
	case !inspection.Supported():
		return []StoreMigrationRefusal{{Code: "store.schema_unsupported", Detail: fmt.Sprintf("the store has schema %d; this build inspects %d to %d", inspection.SchemaVersion, store.MinimumUpgradeSchemaVersion, store.CurrentSchemaVersion)}}
	}
	var refusals []StoreMigrationRefusal
	if len(inspection.ActiveRuns) != 0 {
		refusals = append(refusals, StoreMigrationRefusal{Code: "run.active", Detail: "active runs: " + strings.Join(inspection.ActiveRuns, ", ")})
	}
	if inspection.ActiveInvocations != 0 {
		refusals = append(refusals, StoreMigrationRefusal{Code: "invocation.active", Detail: fmt.Sprintf("%d invocations are active", inspection.ActiveInvocations)})
	}
	if inspection.PendingEffects != 0 {
		refusals = append(refusals, StoreMigrationRefusal{Code: "effect.pending", Detail: fmt.Sprintf("%d external effects are journaled", inspection.PendingEffects)})
	}
	return refusals
}
