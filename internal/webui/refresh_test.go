package webui_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/webui"
)

// TestRefreshIsAnnouncedOnlyWhenEnabled verifies a positive refresh interval
// puts the interval on <body> and loads the same-origin refresh script, and
// that a zero interval does neither.
func TestRefreshIsAnnouncedOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	reader := fakeReader{supervisor: factory.SupervisorView{ObservedAt: fixtureNow}}

	refreshing := get(t, webui.NewHandler(reader, webui.Options{RefreshInterval: 5 * time.Second}), "/").Body.String()
	assertContains(t, refreshing,
		`<body data-refresh-seconds="5">`,
		`<script src="/assets/refresh.js" defer></script>`,
		`id="refresh-status"`, "UI server unreachable",
	)
	assertNoExternalResources(t, refreshing)

	static := get(t, webui.NewHandler(reader, webui.Options{}), "/").Body.String()
	for _, unwanted := range []string{"data-refresh-seconds", "refresh.js", "refresh-status"} {
		if strings.Contains(static, unwanted) {
			t.Errorf("page without refresh contains %q", unwanted)
		}
	}
}

// TestRefreshScriptIsServed verifies the refresh script is served from the
// UI's own origin with a JavaScript content type.
func TestRefreshScriptIsServed(t *testing.T) {
	t.Parallel()

	response := get(t, webui.NewHandler(fakeReader{}, webui.Options{}), "/assets/refresh.js")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /assets/refresh.js status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Fatalf("Content-Type = %q, want text/javascript", got)
	}
	// The script reads the interval the layout writes as data-refresh-seconds.
	assertContains(t, response.Body.String(), "dataset.refreshSeconds")
}
