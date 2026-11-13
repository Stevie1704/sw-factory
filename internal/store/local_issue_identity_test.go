package store_test

import (
	"errors"
	"github.com/Stevie1704/sw-factory/internal/store"
	"testing"
)

// TestAllocatedLocalIssueIdentityIsDurableAndCorrelationIdempotent checks the allocation seam across restart.
func TestAllocatedLocalIssueIdentityIsDurableAndCorrelationIdempotent(t *testing.T) {
	opened, path := openIdeationStoreForTest(t)
	first, err := opened.AllocateLocalIssueIdentity(t.Context(), "/repos/service", "session/1/task")
	if err != nil {
		t.Fatal(err)
	}
	if first.Number < 1 || first.ID == "" {
		t.Fatalf("identity = %#v", first)
	}
	opened = reopenIdeationStoreForTest(t, opened, path)
	replay, err := opened.AllocateLocalIssueIdentity(t.Context(), "/repos/service", "session/1/task")
	if err != nil || replay != first {
		t.Fatalf("replay = %#v, %v; want %#v", replay, err, first)
	}
	other, err := opened.RegisterLocalIssueIdentity(t.Context(), "/repos/service", "local-ffeeddccbbaa99887766554433221100", "import/1/task")
	if err != nil || other.Number == first.Number {
		t.Fatalf("import = %#v, %v", other, err)
	}
	if _, err := opened.RegisterLocalIssueIdentity(t.Context(), "/repos/service", first.ID, "forged/1/task"); !errors.Is(err, store.ErrLocalIssueIdentityConflict) {
		t.Fatalf("correlation changed: %v", err)
	}
	readonly, err := store.OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	found, err := readonly.LocalIssueIdentityByNumber(t.Context(), "/repos/service", first.Number)
	if err != nil || found != first {
		t.Fatalf("read only lookup = %#v, %v", found, err)
	}
}
