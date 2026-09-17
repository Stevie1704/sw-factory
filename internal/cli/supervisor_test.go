package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestWriteSupervisorStatusRendersLiveAndStaleHeartbeats verifies the CLI
// distinguishes a currently live projection from the last stale record.
func TestWriteSupervisorStatusRendersLiveAndStaleHeartbeats(t *testing.T) {
	t.Parallel()

	heartbeat := &store.SupervisorHeartbeat{
		Coordinator: "host-a",
		PID:         1234,
		RenewedAt:   time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC),
		ExpiresAt:   time.Date(2026, 9, 17, 10, 5, 0, 0, time.UTC),
	}
	result := factory.StatusResult{RepositoryPath: "/repo", SupervisorHeartbeat: heartbeat, SupervisorLive: true}
	var output bytes.Buffer
	if !writeSupervisorStatus(&output, &output, result) {
		t.Fatal("writeSupervisorStatus() reported a write failure")
	}
	if !strings.Contains(output.String(), "supervisor: live (coordinator=host-a pid=1234") {
		t.Fatalf("live output = %q", output.String())
	}

	output.Reset()
	result.SupervisorLive = false
	if !writeSupervisorStatus(&output, &output, result) {
		t.Fatal("writeSupervisorStatus() reported a write failure for stale output")
	}
	if !strings.Contains(output.String(), "supervisor: not live (coordinator=host-a pid=1234") || !strings.Contains(output.String(), "expires=2026-09-17T10:05:00Z") {
		t.Fatalf("stale output = %q", output.String())
	}
}
