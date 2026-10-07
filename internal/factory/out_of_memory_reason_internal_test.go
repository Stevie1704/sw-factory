package factory

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestWithOutOfMemoryCauseNamesOnlyAnOutOfMemoryKill verifies a paused run's
// reason names an out-of-memory kill and keeps every other reason unchanged.
func TestWithOutOfMemoryCauseNamesOnlyAnOutOfMemoryKill(t *testing.T) {
	t.Parallel()

	reason := "focused red-test verification could not be completed"
	oom := fmt.Errorf("run command in worker %q: %w", "run-1", &worker.OutOfMemoryError{})
	if got := withOutOfMemoryCause(reason, oom); !strings.HasPrefix(got, reason+": ") || !strings.Contains(got, "out of memory") {
		t.Fatalf("withOutOfMemoryCause() = %q, want the reason followed by the out-of-memory cause", got)
	}
	if got := withOutOfMemoryCause(reason, errors.New("worker container is not running")); got != reason {
		t.Fatalf("withOutOfMemoryCause() = %q, want the reason unchanged", got)
	}
}
