package factory

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestReleaseActiveInvocationDoesNotMutateCopiedRun verifies filtering one
// run projection cannot alter the shared slice backing an earlier projection.
func TestReleaseActiveInvocationDoesNotMutateCopiedRun(t *testing.T) {
	previous := store.Run{
		ActiveInvocationID:  "inv-specification",
		ActiveInvocationIDs: []string{"inv-specification", "inv-standards"},
	}
	next := previous

	releaseActiveInvocation(&next, "inv-specification")

	if got := previous.ActiveInvocationIDs; len(got) != 2 || got[0] != "inv-specification" || got[1] != "inv-standards" {
		t.Fatalf("previous active invocations = %#v, want unchanged copied projection", got)
	}
	if got := next.ActiveInvocationIDs; len(got) != 1 || got[0] != "inv-standards" {
		t.Fatalf("next active invocations = %#v, want retained standards invocation", got)
	}
}

// TestRecoveryQueriesLatestInvocationAfterEmptyPluralProjection verifies a
// plural-only store still exposes its latest terminal invocation to recovery.
func TestRecoveryQueriesLatestInvocationAfterEmptyPluralProjection(t *testing.T) {
	runStore := &pluralOnlyRecoveryStore{}
	run := store.Run{ID: "run-empty-active-projection"}
	diagnosis := newRecoveryDiagnosis(run.ID)

	(&Service{}).inspectInvocationProjection(context.Background(), &diagnosis, config.RepositoryRegistration{}, runStore, run)

	if runStore.latestCalls != 1 {
		t.Fatalf("latest invocation lookups = %d, want one", runStore.latestCalls)
	}
}

// TestReviewRoundCompleteRequiresOnlyConfiguredRoles verifies each isolated
// review result is required exactly when its frozen role is runnable.
func TestReviewRoundCompleteRequiresOnlyConfiguredRoles(t *testing.T) {
	const checkpoint = "checkpoint"
	for _, test := range []struct {
		name          string
		configured    []string
		specification *store.SpecificationReview
		standards     *store.StandardsReview
		want          bool
	}{
		{
			name:       "unconfigured specification role",
			configured: []string{workflow.RoleStandardsReview},
			standards:  &store.StandardsReview{CheckpointSHA: checkpoint},
			want:       true,
		},
		{
			name:          "unconfigured standards role",
			configured:    []string{workflow.RoleSpecificationReview},
			specification: &store.SpecificationReview{CheckpointSHA: checkpoint},
			want:          true,
		},
		{
			name:       "configured specification role missing result",
			configured: []string{workflow.RoleSpecificationReview},
			want:       false,
		},
		{
			name:          "configured standards role has stale result",
			configured:    []string{workflow.RoleSpecificationReview, workflow.RoleStandardsReview},
			specification: &store.SpecificationReview{CheckpointSHA: checkpoint},
			standards:     &store.StandardsReview{CheckpointSHA: "stale"},
			want:          false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := store.Run{
				CheckpointSHA:       checkpoint,
				SpecificationReview: test.specification,
				StandardsReview:     test.standards,
			}
			packet := SpecificationPacket{
				Version: specificationPacketVersion,
				RepositoryConfig: config.RepositoryConfig{
					RoleHarnessDefaults: make(map[string]config.Harness, len(test.configured)),
					ModelOptions:        make(map[string][]string, len(test.configured)),
				},
			}
			for _, role := range test.configured {
				packet.RepositoryConfig.RoleHarnessDefaults[role] = config.HarnessCodex
				packet.RepositoryConfig.ModelOptions[role] = []string{"gpt-5"}
			}
			serialized, err := json.Marshal(packet)
			if err != nil {
				t.Fatal(err)
			}
			run.SpecificationPacket = string(serialized)

			if got := reviewRoundComplete(run); got != test.want {
				t.Fatalf("reviewRoundComplete() = %t, want %t", got, test.want)
			}
		})
	}
}

// TestMissingConcurrentReviewRoleResumesTheUnlaunchedReviewer verifies a
// partial concurrent launch can continue after the completed review result is
// accepted, regardless of which reviewer finished first.
func TestMissingConcurrentReviewRoleResumesTheUnlaunchedReviewer(t *testing.T) {
	t.Parallel()

	checkpoint := "checkpoint"
	packet := SpecificationPacket{Version: specificationPacketVersion, RepositoryConfig: config.RepositoryConfig{
		RoleHarnessDefaults: map[string]config.Harness{
			workflow.RoleSpecificationReview: config.HarnessCodex,
			workflow.RoleStandardsReview:     config.HarnessCodex,
		},
		ModelOptions: map[string][]string{
			workflow.RoleSpecificationReview: {"gpt-5"},
			workflow.RoleStandardsReview:     {"gpt-5"},
		},
	}}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name            string
		specification   *store.SpecificationReview
		standards       *store.StandardsReview
		wantMissingRole string
	}{
		{name: "standards missing", specification: &store.SpecificationReview{CheckpointSHA: checkpoint}, wantMissingRole: workflow.RoleStandardsReview},
		{name: "specification missing", standards: &store.StandardsReview{CheckpointSHA: checkpoint}, wantMissingRole: workflow.RoleSpecificationReview},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := progressionState{Run: &store.Run{
				Stage:               store.StageReview,
				Status:              store.StatusActive,
				CheckpointSHA:       checkpoint,
				SpecificationPacket: string(packetData),
				SpecificationReview: test.specification,
				StandardsReview:     test.standards,
			}}
			if got, ok := missingConcurrentReviewRole(state); !ok || got != test.wantMissingRole {
				t.Fatalf("missingConcurrentReviewRole() = %q/%t, want %q/true", got, ok, test.wantMissingRole)
			}
		})
	}
}

// pluralOnlyRecoveryStore exposes concurrent and latest invocation lookups but
// intentionally omits the legacy singular active-invocation interface.
type pluralOnlyRecoveryStore struct {
	latestCalls int
}

// CurrentRun satisfies the operational-store seam.
func (*pluralOnlyRecoveryStore) CurrentRun(context.Context) (*store.Run, error) {
	return nil, nil
}

// ActiveInvocations reports an empty concurrent invocation projection.
func (*pluralOnlyRecoveryStore) ActiveInvocations(context.Context, string) ([]store.Invocation, error) {
	return nil, nil
}

// LatestInvocation records recovery's fallback query.
func (s *pluralOnlyRecoveryStore) LatestInvocation(context.Context, string) (*store.Invocation, error) {
	s.latestCalls++
	return nil, nil
}

// Close satisfies the operational-store seam.
func (*pluralOnlyRecoveryStore) Close() error { return nil }
