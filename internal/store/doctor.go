package store

import (
	"context"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// StartupCheck returns the SQLite readiness check for one host-local
// operational-store path. The normal store opener remains the single owner of
// directory, schema, migration, and permission validation.
func StartupCheck(path string) doctor.Check {
	return func(ctx context.Context) doctor.Result {
		if strings.TrimSpace(path) == "" {
			return doctor.Failure("SQLite", "the operational store path is not configured", "register the repository with a private operational_data_path")
		}
		opened, err := Open(ctx, path)
		if err != nil {
			return doctor.Failure("SQLite", "the operational store cannot be opened with its supported schema", "repair the operational store path, permissions, or schema before starting the factory")
		}
		if err := opened.Close(); err != nil {
			return doctor.Failure("SQLite", "the operational store could not be closed after diagnosis", "repair the local SQLite store or its filesystem permissions")
		}
		return doctor.Success("SQLite")
	}
}
