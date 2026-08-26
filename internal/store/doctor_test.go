package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestStartupCheckOpensTheVersionedOperationalStore verifies SQLite diagnosis
// delegates schema and permission ownership to the operational store.
func TestStartupCheckOpensTheVersionedOperationalStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	result := doctor.Run(context.Background(), store.StartupCheck(path)).Results[0]
	if result.Status != doctor.StatusPassed {
		t.Fatalf("SQLite result = %#v, want passed", result)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("diagnosed store was not created by the store opener: %v", err)
	}
}

// TestStartupCheckDoesNotExposeSQLiteErrors verifies the CLI-facing result is
// bounded even when the underlying store reports a path-specific failure.
func TestStartupCheckDoesNotExposeSQLiteErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result := doctor.Run(context.Background(), store.StartupCheck(path)).Results[0]
	if result.Status != doctor.StatusFailed {
		t.Fatalf("SQLite result = %#v, want failed", result)
	}
	if strings.Contains(result.Problem+result.Action, "factory.db") {
		t.Fatalf("SQLite result exposed a path: %#v", result)
	}
}
