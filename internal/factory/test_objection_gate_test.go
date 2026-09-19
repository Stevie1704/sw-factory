package factory

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// TestAutomatedTestObjectionGateFollowsFrozenRepositoryPolicy verifies the
// checked-in policy is the sole authority for the bounded revision cycle.
func TestAutomatedTestObjectionGateFollowsFrozenRepositoryPolicy(t *testing.T) {
	packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{}}

	allowed, reason := automatedTestObjectionGate(packet)
	if allowed || reason != "test objection recorded; automated revision is disabled by repository policy" {
		t.Fatalf("disabled policy gate = allowed=%v reason=%q, want policy denial", allowed, reason)
	}

	packet.RepositoryConfig.TestPolicy.AllowAutomatedObjections = true
	allowed, reason = automatedTestObjectionGate(packet)
	if !allowed || reason != "" {
		t.Fatalf("enabled policy gate = allowed=%v reason=%q, want allowed", allowed, reason)
	}
}
