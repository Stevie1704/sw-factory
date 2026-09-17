package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// StartupCheck returns the SQLite readiness check for one existing host-local
// operational-store path. Diagnosis uses the read-only opener so it never
// creates, migrates, backs up, chmods, or initializes the operational store.
func StartupCheck(path string) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if strings.TrimSpace(path) == "" {
			return doctor.Failure("SQLite", "the operational store path is not configured", "register the repository with a private operational_data_path")
		}
		opened, err := OpenReadOnly(ctx, path)
		if err != nil {
			return doctor.Failure("SQLite", "the existing operational store cannot be opened read-only with its current schema", "run the normal store initialization or migration, then retry the diagnosis")
		}
		if err := opened.Close(); err != nil {
			return doctor.Failure("SQLite", "the operational store could not be closed after diagnosis", "repair the local SQLite store or its filesystem permissions")
		}
		return doctor.Success("SQLite")
	}
}

// SupervisorStartupCheck reports whether the operational store contains a
// live coordinator heartbeat. Absence is a warning rather than a blocking
// prerequisite because this check is also run immediately before `factory
// start`, when no supervisor is expected to exist yet.
func SupervisorStartupCheck(path string, now time.Time) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if strings.TrimSpace(path) == "" {
			return doctor.Warning("Supervisor", "the supervisor heartbeat cannot be read because the operational store is not configured", "register the repository before inspecting supervisor liveness")
		}
		opened, err := OpenReadOnly(ctx, path)
		if err != nil {
			return doctor.Warning("Supervisor", "the supervisor heartbeat is unavailable because the operational store is not ready", "repair the operational store, then run the diagnosis again")
		}
		heartbeat, heartbeatErr := opened.ReadSupervisorHeartbeat(ctx)
		run, runErr := opened.CurrentRun(ctx)
		closeErr := opened.Close()
		if heartbeatErr != nil || runErr != nil || closeErr != nil {
			return doctor.Warning("Supervisor", "the supervisor heartbeat could not be read from the operational store", "repair the operational store, then run the diagnosis again")
		}
		if heartbeat == nil {
			return supervisorHeartbeatWarning(run)
		}
		if heartbeat.Live(now.UTC()) {
			return doctor.Success("Supervisor")
		}
		return supervisorHeartbeatWarning(run)
	}
}

// supervisorHeartbeatWarning keeps the absence diagnosis explicit when an
// active run would otherwise look like ordinary unattended progress.
func supervisorHeartbeatWarning(run *Run) doctor.Result {
	if run != nil && !IsTerminalStatus(run.Status) {
		return doctor.Warning("Supervisor", fmt.Sprintf("active run %s has no live supervisor heartbeat", run.ID), "run `factory start` so the active run can progress")
	}
	return doctor.Warning("Supervisor", "no live supervisor heartbeat is recorded", "run `factory start` when unattended progression is needed")
}
