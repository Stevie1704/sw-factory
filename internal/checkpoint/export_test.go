package checkpoint

import "context"

// ApplyRestoreWithCrash runs ApplyRestore and calls crash at each named
// point. An error from crash stops the restore at once, as a process crash
// does, so tests can continue it from the files it leaves.
func ApplyRestoreWithCrash(ctx context.Context, plan RestorePlan, build Factory, crash func(point string) error) error {
	return applyRestore(ctx, plan, build, crash)
}

// ContinueRestoreWithCrash runs ContinueRestore and calls crash at each
// named point, so tests can stop a rollback in the middle.
func ContinueRestoreWithCrash(ctx context.Context, storePath string, crash func(point string) error) (RestoreContinuation, error) {
	return continueRestore(ctx, storePath, crash)
}
