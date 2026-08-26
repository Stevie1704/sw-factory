package store

import (
	"context"
	"strings"

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
