package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// startPrecondition guards the suggested `factory start` action. Starting
// the coordinator is an external mutation: it reconciles tracker state,
// claims issues, and can publish branches and pull requests.
var startPrecondition = doctor.Precondition{Code: "doctor.ready", Description: "factory doctor reports no blocking check"}

// Store diagnosis conditions. Codes are stable; wording may change.
var (
	sqlitePathMissing    = doctor.Condition{Code: "store.sqlite.path_missing", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup", Unobserved: true}
	sqliteUnreadable     = doctor.Condition{Code: "store.sqlite.unreadable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	sqliteCloseFailed    = doctor.Condition{Code: "store.sqlite.close_failed", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "debug"}
	supervisorUnobserved = doctor.Condition{Code: "store.supervisor.unobserved", Origin: doctor.OriginUnknown, Effect: doctor.EffectInspection, Topic: "debug", Unobserved: true}
	// supervisorMissing means no heartbeat was ever recorded.
	supervisorMissing = doctor.Condition{Code: "store.supervisor.missing", Origin: doctor.OriginEnvironment, Effect: doctor.EffectExternalMutation, Topic: "debug", Preconditions: []doctor.Precondition{startPrecondition}}
	// supervisorStale means a recorded heartbeat expired or its coordinator
	// lock is no longer held.
	supervisorStale = doctor.Condition{Code: "store.supervisor.stale", Origin: doctor.OriginEnvironment, Effect: doctor.EffectExternalMutation, Topic: "debug", Preconditions: []doctor.Precondition{startPrecondition}}
)

// StartupCheck returns the SQLite readiness check for one existing host-local
// operational-store path. Diagnosis uses the read-only opener so it never
// creates, migrates, backs up, chmods, or initializes the operational store.
func StartupCheck(path string) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if strings.TrimSpace(path) == "" {
			return doctor.Failure(sqlitePathMissing, "SQLite", "the operational store path is not configured", "register the repository with a private operational_data_path")
		}
		opened, err := OpenReadOnly(ctx, path)
		if err != nil {
			return doctor.Failure(sqliteUnreadable, "SQLite", "the existing operational store cannot be opened read-only with its current schema", "run the normal store initialization or migration, then retry the diagnosis")
		}
		if err := opened.Close(); err != nil {
			return doctor.Failure(sqliteCloseFailed, "SQLite", "the operational store could not be closed after diagnosis", "repair the local SQLite store or its filesystem permissions")
		}
		return doctor.Success("store.sqlite.ready", "SQLite")
	}
}

// SupervisorStartupCheck reports whether the operational store contains a
// live coordinator heartbeat. Absence is a warning rather than a blocking
// prerequisite because this check is also run immediately before `factory
// start`, when no supervisor is expected to exist yet.
func SupervisorStartupCheck(path string, now time.Time) doctor.Check {
	return supervisorStartupCheck(path, now, nil)
}

// SupervisorStartupCheckWithLock reports supervisor liveness only when both
// the persisted heartbeat and the kernel-backed coordinator lock are live.
// The callback is kept outside this package so the store package does not
// depend on the factory lock implementation.
func SupervisorStartupCheckWithLock(path string, now time.Time, lockHeld func() bool) doctor.Check {
	return supervisorStartupCheck(path, now, lockHeld)
}

// supervisorStartupCheck contains the read-only heartbeat diagnosis shared by
// the store-only compatibility check and the factory lock-aware check.
func supervisorStartupCheck(path string, now time.Time, lockHeld func() bool) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if strings.TrimSpace(path) == "" {
			return doctor.Warning(supervisorUnobserved, "Supervisor", "the supervisor heartbeat cannot be read because the operational store is not configured", "register the repository before inspecting supervisor liveness")
		}
		opened, err := OpenReadOnly(ctx, path)
		if err != nil {
			return doctor.Warning(supervisorUnobserved, "Supervisor", "the supervisor heartbeat is unavailable because the operational store is not ready", "repair the operational store, then run the diagnosis again")
		}
		heartbeat, heartbeatErr := opened.ReadSupervisorHeartbeat(ctx)
		run, runErr := opened.CurrentRun(ctx)
		closeErr := opened.Close()
		if heartbeatErr != nil || runErr != nil || closeErr != nil {
			return doctor.Warning(supervisorUnobserved, "Supervisor", "the supervisor heartbeat could not be read from the operational store", "repair the operational store, then run the diagnosis again")
		}
		if heartbeat == nil {
			return supervisorHeartbeatWarning(supervisorMissing, run)
		}
		if heartbeat.Live(now.UTC()) && (lockHeld == nil || lockHeld()) {
			return doctor.Success("store.supervisor.live", "Supervisor")
		}
		return supervisorHeartbeatWarning(supervisorStale, run)
	}
}

// supervisorHeartbeatWarning keeps the absence diagnosis explicit when an
// active run would otherwise look like ordinary unattended progress.
func supervisorHeartbeatWarning(condition doctor.Condition, run *Run) doctor.Result {
	if run != nil && !IsTerminalStatus(run.Status) {
		return doctor.Warning(condition, "Supervisor", fmt.Sprintf("active run %s has no live supervisor heartbeat", run.ID), "run `factory start` so the active run can progress").
			WithFacts(doctor.Fact{Name: "active_run", Value: run.ID})
	}
	return doctor.Warning(condition, "Supervisor", "no live supervisor heartbeat is recorded", "run `factory start` when unattended progression is needed")
}
