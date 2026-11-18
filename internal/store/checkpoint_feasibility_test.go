package store_test

// This file keeps the feasibility evidence of ADR 0032 that the production
// tests do not cover. The snapshot sequence itself is tested in
// snapshot_test.go, and the divergence check in internal/checkpoint and
// internal/factory.

import (
	"context"
	"path/filepath"
	"testing"
)

// TestVacuumIntoFailsInsideTheWriteTransaction verifies the assumption of
// ADR 0032 that VACUUM INTO fails inside the BEGIN IMMEDIATE transaction
// that holds the write lock, in the store's rollback-journal mode and in WAL
// mode. So the snapshot must run on a second connection.
func TestVacuumIntoFailsInsideTheWriteTransaction(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"delete", "wal"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := quiescentStore(t, completedRun("run-1", 7))
			if _, err := openRaw(t, path).ExecContext(ctx, "PRAGMA journal_mode = "+mode); err != nil {
				t.Fatal(err)
			}
			lock, err := openRaw(t, path).Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Close() }()
			if _, err := lock.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatalf("BEGIN IMMEDIATE: %v", err)
			}
			defer func() { _, _ = lock.ExecContext(ctx, "ROLLBACK") }()
			if _, err := lock.ExecContext(ctx, "VACUUM INTO ?", filepath.Join(privateDirectory(t), "store.db")); err == nil {
				t.Fatal("VACUUM INTO inside the write transaction succeeded; ADR 0032 assumes it fails")
			}
		})
	}
}
