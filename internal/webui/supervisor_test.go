package webui_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/webui"
)

// fakeReader returns one fixed supervisor view on every page, so a test can
// render each banner state without a store.
type fakeReader struct {
	supervisor factory.SupervisorView
}

// RunOverview returns an empty run list with the fixed supervisor view.
func (reader fakeReader) RunOverview(context.Context) (factory.RunOverview, error) {
	return factory.RunOverview{RepositoryPath: "/repo", Supervisor: reader.supervisor}, nil
}

// RunDetail returns a minimal waiting run with the fixed supervisor view.
func (reader fakeReader) RunDetail(_ context.Context, runID string) (factory.RunDetail, error) {
	return factory.RunDetail{
		Supervisor: reader.supervisor,
		Run:        store.Run{ID: runID, Stage: store.StageCheck, Status: store.StatusWaitingForHuman},
	}, nil
}

// RunEvaluation returns the run identity with no run and no summary, and the
// fixed supervisor view.
func (reader fakeReader) RunEvaluation(_ context.Context, runID string) (factory.RunEvaluation, error) {
	return factory.RunEvaluation{Supervisor: reader.supervisor, RunID: runID}, nil
}

// fixtureHeartbeat is a heartbeat renewed shortly before fixtureNow that
// expires one minute after it.
var fixtureHeartbeat = &store.SupervisorHeartbeat{
	Coordinator: "host-a", PID: 4242,
	StartedAt: fixtureNow.Add(-time.Hour), RenewedAt: fixtureNow.Add(-10 * time.Second), ExpiresAt: fixtureNow.Add(time.Minute),
}

// TestSupervisorBannerShowsEachLivenessState verifies the banner names the
// coordinator liveness, marks the state for styling, and shows when the page
// read the store, on every page.
func TestSupervisorBannerShowsEachLivenessState(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		supervisor factory.SupervisorView
		state      string
		text       string
	}{
		"live": {
			supervisor: factory.SupervisorView{Heartbeat: fixtureHeartbeat, Live: true, ObservedAt: fixtureNow},
			state:      "live",
			text:       "supervisor: live (coordinator host-a, pid 4242, renewed 2026-10-09T11:59:50Z, expires 2026-10-09T12:01:00Z)",
		},
		"expired": {
			supervisor: factory.SupervisorView{Heartbeat: fixtureHeartbeat, ObservedAt: fixtureNow.Add(time.Hour)},
			state:      "expired",
			text:       "supervisor: not live (coordinator host-a, pid 4242, expired 2026-10-09T12:01:00Z)",
		},
		"no heartbeat": {
			supervisor: factory.SupervisorView{ObservedAt: fixtureNow},
			state:      "missing",
			text:       "supervisor: not live (no heartbeat recorded)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			handler := webui.NewHandler(fakeReader{supervisor: test.supervisor}, webui.Options{})
			readAt := "read at " + test.supervisor.ObservedAt.UTC().Format(time.RFC3339)
			for _, path := range []string{"/", "/runs/" + waitingRunID} {
				response := get(t, handler, path)
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d, want 200", path, response.Code)
				}
				assertContains(t, response.Body.String(), `id="supervisor" data-state="`+test.state+`"`, test.text, readAt)
			}
		})
	}
}

// TestSupervisorWarningNamesAnActiveRunWithoutLiveSupervisor verifies the
// banner warns only when a non-terminal run exists while the supervisor is
// not live, the same rule as factory status.
func TestSupervisorWarningNamesAnActiveRunWithoutLiveSupervisor(t *testing.T) {
	t.Parallel()

	const warning = "active run run-waiting has no live supervisor; run factory start"
	for name, test := range map[string]struct {
		supervisor factory.SupervisorView
		warn       bool
	}{
		"active run, not live":    {supervisor: factory.SupervisorView{ActiveRunID: waitingRunID, ObservedAt: fixtureNow}, warn: true},
		"active run, live":        {supervisor: factory.SupervisorView{ActiveRunID: waitingRunID, Heartbeat: fixtureHeartbeat, Live: true, ObservedAt: fixtureNow}},
		"no active run, not live": {supervisor: factory.SupervisorView{ObservedAt: fixtureNow}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := get(t, webui.NewHandler(fakeReader{supervisor: test.supervisor}, webui.Options{}), "/").Body.String()
			if got := strings.Contains(body, warning); got != test.warn {
				t.Fatalf("page shows warning = %v, want %v; body:\n%s", got, test.warn, body)
			}
			if test.warn {
				assertContains(t, body, `href="/runs/`+waitingRunID+`"`)
			}
		})
	}
}

// TestStoreBackedPagesWarnAfterTheHeartbeatExpires verifies the factory
// service reports the waiting run as active, so every page warns once the
// stored heartbeat has expired and stays quiet while it is live.
func TestStoreBackedPagesWarnAfterTheHeartbeatExpires(t *testing.T) {
	t.Parallel()

	const warning = "active run " + waitingRunID + " has no live supervisor"
	fixture := newUIFixture(t)
	later := fixtureNow.Add(time.Hour)
	expired := webui.NewHandler(
		factory.NewWithDependencies(fixture.configPath, factory.Dependencies{Now: func() time.Time { return later }}),
		webui.Options{},
	)

	for _, path := range []string{"/", "/runs/" + terminalRunID} {
		assertContains(t, get(t, expired, path).Body.String(), warning, `data-state="expired"`)
		if body := get(t, fixture.handler, path).Body.String(); strings.Contains(body, warning) {
			t.Errorf("GET %s warns while the heartbeat is live", path)
		}
	}
}
