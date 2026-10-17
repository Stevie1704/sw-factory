package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/webui"
)

// csrfMeta matches the per-process command token the layout renders.
var csrfMeta = regexp.MustCompile(`<meta name="factory-csrf-token" content="([0-9a-f]{64})">`)

// sessionUI is the run fixture plus one session and a command-capable UI.
type sessionUI struct {
	uiFixture
	token string
}

// newSessionUI adds session-1 at revision 1 and a heartbeat that is live or
// expired at fixtureNow to the run fixture, and reads the command token from
// a page as a browser does.
func newSessionUI(t *testing.T, live bool) sessionUI {
	t.Helper()
	fixture := newUIFixture(t)
	opened, err := store.Open(t.Context(), fixture.storePath)
	if err != nil {
		t.Fatal(err)
	}
	host, err := config.LoadHost(fixture.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-1", RepositoryPath: host.Repositories[0].Path, Title: "Faster onboarding", State: store.IdeationStateAwaitingUser, CreatedAt: fixtureNow,
	}); err != nil {
		t.Fatal(err)
	}
	expires := fixtureNow.Add(time.Minute)
	if !live {
		expires = fixtureNow.Add(-time.Second)
	}
	if err := opened.SaveSupervisorHeartbeat(t.Context(), store.SupervisorHeartbeat{
		Coordinator: "host-a", PID: 4242, StartedAt: expires.Add(-time.Hour), RenewedAt: expires.Add(-time.Minute), ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	service := factory.NewWithDependencies(fixture.configPath, factory.Dependencies{Now: func() time.Time { return fixtureNow }})
	fixture.handler = webui.NewHandler(service, webui.Options{Sessions: service})
	page := get(t, fixture.handler, "/").Body.String()
	match := csrfMeta.FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("page has no command token:\n%s", page)
	}
	return sessionUI{uiFixture: fixture, token: match[1]}
}

// commandBody returns a submit_turn request for session-1.
func commandBody(requestID string, expected int) string {
	return `{"request_id":"` + requestID + `","session_id":"session-1","kind":"submit_turn","expected_revision":` +
		strconv.Itoa(expected) + `,"payload":{"body":"Operators wait for images."}}`
}

// post sends one same-origin command request with the token, then applies
// change to the request before it is served.
func (ui sessionUI) post(t *testing.T, body string, change func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/commands", strings.NewReader(body))
	request.Host = "127.0.0.1:8765"
	request.Header.Set("Origin", "http://127.0.0.1:8765")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Factory-CSRF", ui.token)
	if change != nil {
		change(request)
	}
	recorder := httptest.NewRecorder()
	ui.handler.ServeHTTP(recorder, request)
	return recorder
}

// pendingCommands reads the inbox directly.
func (ui sessionUI) pendingCommands(t *testing.T) []store.IdeationCommand {
	t.Helper()
	opened, err := store.OpenReadOnly(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	pending, err := opened.PendingIdeationCommands(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

// decodeAnswer decodes a JSON answer and checks its content type.
func decodeAnswer(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json; body:\n%s", got, response.Body)
	}
	answer := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer %s: %v", response.Body, err)
	}
	return answer
}

// TestSameOriginCommandIsAcceptedOnceAndReplayed verifies that a same-origin
// request with the token is accepted durably (not completed), and that a
// repeated request ID returns the stored command.
func TestSameOriginCommandIsAcceptedOnceAndReplayed(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	first := ui.post(t, commandBody("request-1", 1), nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202; body:\n%s", first.Code, first.Body)
	}
	answer := decodeAnswer(t, first)
	if answer["state"] != "accepted" || answer["outcome"] != nil || answer["replayed"] != false || answer["request_id"] != "request-1" {
		t.Fatalf("answer = %#v, want an accepted, not completed, command", answer)
	}
	second := ui.post(t, commandBody("request-1", 1), nil)
	if second.Code != http.StatusOK || decodeAnswer(t, second)["replayed"] != true {
		t.Fatalf("repeated POST status = %d, body = %s; want a 200 replay", second.Code, second.Body)
	}
	if pending := ui.pendingCommands(t); len(pending) != 1 {
		t.Fatalf("pending = %d commands, want 1", len(pending))
	}

	status := get(t, ui.handler, "/api/commands/request-1")
	if status.Code != http.StatusOK || decodeAnswer(t, status)["state"] != "accepted" {
		t.Fatalf("GET command status = %d, body = %s", status.Code, status.Body)
	}
	if missing := get(t, ui.handler, "/api/commands/request-missing"); missing.Code != http.StatusNotFound {
		t.Fatalf("GET missing command status = %d, want 404", missing.Code)
	}
}

// TestCrossSiteSubmissionIsRefused verifies that a request a third-party
// page can send, or one without the per-process token, appends nothing, so
// no foreign page can issue an accepted approval.
func TestCrossSiteSubmissionIsRefused(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	approve := `{"request_id":"approve-1","session_id":"session-1","kind":"approve","expected_revision":1,"payload":{"plan_revision":1,"payload_sha256":"` +
		strings.Repeat("a", 64) + `","relationship_mapping_sha256":"` + strings.Repeat("b", 64) + `"}}`
	tests := []struct {
		name   string
		change func(*http.Request)
		want   int
	}{
		{name: "foreign origin", change: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, want: http.StatusForbidden},
		{name: "other loopback port", change: func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9999") }, want: http.StatusForbidden},
		{name: "null origin", change: func(r *http.Request) { r.Header.Set("Origin", "null") }, want: http.StatusForbidden},
		{name: "missing origin", change: func(r *http.Request) { r.Header.Del("Origin") }, want: http.StatusForbidden},
		{name: "cross-site fetch metadata", change: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, want: http.StatusForbidden},
		{name: "missing token", change: func(r *http.Request) { r.Header.Del("X-Factory-CSRF") }, want: http.StatusForbidden},
		{name: "wrong token", change: func(r *http.Request) { r.Header.Set("X-Factory-CSRF", strings.Repeat("0", 64)) }, want: http.StatusForbidden},
		{name: "form encoding", change: func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, want: http.StatusUnsupportedMediaType},
		{name: "plain text", change: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, want: http.StatusUnsupportedMediaType},
		{name: "rebinding host", change: func(r *http.Request) { r.Host = "evil.example:8765" }, want: http.StatusMisdirectedRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if response := ui.post(t, approve, test.change); response.Code != test.want {
				t.Fatalf("POST status = %d, want %d; body:\n%s", response.Code, test.want, response.Body)
			}
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		request := httptest.NewRequest(method, "/api/commands", strings.NewReader(approve))
		request.Host = "127.0.0.1:8765"
		recorder := httptest.NewRecorder()
		ui.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/commands status = %d, want 405", method, recorder.Code)
		}
	}
	if pending := ui.pendingCommands(t); len(pending) != 0 {
		t.Fatalf("pending = %#v, want no command from a refused request", pending)
	}
}

// TestMalformedOrOversizedCommandIsRefused verifies bounded, strict input.
func TestMalformedOrOversizedCommandIsRefused(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	for name, test := range map[string]struct {
		body string
		want int
	}{
		"unknown field": {body: `{"request_id":"r","session_id":"session-1","kind":"cancel","expected_revision":1,"payload":{},"launch":true}`, want: http.StatusBadRequest},
		"unknown kind":  {body: `{"request_id":"r","session_id":"session-1","kind":"launch_harness","expected_revision":1,"payload":{}}`, want: http.StatusBadRequest},
		"oversized":     {body: `{"request_id":"r","session_id":"session-1","kind":"submit_turn","expected_revision":1,"payload":{"body":"` + strings.Repeat("x", 3*1024*1024) + `"}}`, want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			response := ui.post(t, test.body, nil)
			if response.Code != test.want || decodeAnswer(t, response)["outcome"] != "invalid" {
				t.Fatalf("POST status = %d, body = %s; want %d invalid", response.Code, response.Body, test.want)
			}
		})
	}
	if pending := ui.pendingCommands(t); len(pending) != 0 {
		t.Fatalf("pending = %#v, want none", pending)
	}
}

// TestStaleTabGetsAConflictWithTheCurrentRevision verifies the useful
// conflict for a tab that shows an old revision.
func TestStaleTabGetsAConflictWithTheCurrentRevision(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	response := ui.post(t, commandBody("request-stale", 3), nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("POST status = %d, want 409; body:\n%s", response.Code, response.Body)
	}
	answer := decodeAnswer(t, response)
	if answer["outcome"] != "conflict" || answer["current_revision"] != float64(1) || answer["expected_revision"] != float64(3) {
		t.Fatalf("answer = %#v, want a conflict naming revisions 1 and 3", answer)
	}
}

// TestStoppedSupervisorGetsUnavailableAndPagesStayReadable verifies the
// explicit unavailable result and that run, evaluation, and session reads
// keep working while the coordinator is stopped.
func TestStoppedSupervisorGetsUnavailableAndPagesStayReadable(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, false)
	response := ui.post(t, commandBody("request-1", 1), nil)
	if response.Code != http.StatusServiceUnavailable || decodeAnswer(t, response)["outcome"] != "unavailable" {
		t.Fatalf("POST status = %d, body = %s; want 503 unavailable", response.Code, response.Body)
	}
	if pending := ui.pendingCommands(t); len(pending) != 0 {
		t.Fatalf("pending = %#v, want none", pending)
	}
	for _, path := range append(pagePaths, "/api/sessions", "/api/sessions/session-1") {
		if got := get(t, ui.handler, path).Code; got >= http.StatusInternalServerError {
			t.Errorf("GET %s status = %d while the supervisor is stopped", path, got)
		}
	}
}

// TestSessionActivityReloadsAcceptedCommandsAfterReconnect verifies the JSON
// read a reconnecting browser uses.
func TestSessionActivityReloadsAcceptedCommandsAfterReconnect(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	if response := ui.post(t, commandBody("request-1", 1), nil); response.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d", response.Code)
	}
	response := get(t, ui.handler, "/api/sessions/session-1")
	if response.Code != http.StatusOK {
		t.Fatalf("GET activity status = %d; body:\n%s", response.Code, response.Body)
	}
	var activity struct {
		Supervisor struct {
			Live bool `json:"live"`
		} `json:"supervisor"`
		Session struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Revision int64  `json:"revision"`
		} `json:"session"`
		Commands []struct {
			RequestID string `json:"request_id"`
			State     string `json:"state"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &activity); err != nil {
		t.Fatal(err)
	}
	if !activity.Supervisor.Live || activity.Session.ID != "session-1" || activity.Session.Revision != 1 ||
		len(activity.Commands) != 1 || activity.Commands[0].RequestID != "request-1" || activity.Commands[0].State != "accepted" {
		t.Fatalf("activity = %#v, want the session and its accepted command", activity)
	}
	if missing := get(t, ui.handler, "/api/sessions/session-missing"); missing.Code != http.StatusNotFound {
		t.Fatalf("GET missing session status = %d, want 404", missing.Code)
	}
	list := get(t, ui.handler, "/api/sessions")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"session-1"`) {
		t.Fatalf("GET /api/sessions status = %d, body = %s", list.Code, list.Body)
	}
}

// TestSessionReadsLeaveTheStoreUnchanged verifies that session reads, like
// run pages, never write the store, and that only a UI with the session API
// hands a command token to its pages.
func TestSessionReadsLeaveTheStoreUnchanged(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	makeStoreReadOnly(t, ui.storePath)
	before := snapshotStore(t, ui.storePath)
	for _, path := range append(slices.Clone(pagePaths), "/api/sessions", "/api/sessions/session-1", "/api/commands/request-1", "/assets/commands.js", "/assets/sessions.js", "/sessions", "/sessions/session-1", "/sessions/session-missing") {
		if response := get(t, ui.handler, path); response.Code >= http.StatusInternalServerError {
			t.Fatalf("GET %s status = %d; body:\n%s", path, response.Code, response.Body)
		}
	}
	if after := snapshotStore(t, ui.storePath); after != before {
		t.Fatalf("store changed: before %+v, after %+v", before, after)
	}
	assertServedAssetsAreSelfContained(t, ui.handler)
	if page := get(t, newUIFixture(t).handler, "/").Body.String(); strings.Contains(page, "factory-csrf-token") || strings.Contains(page, "commands.js") {
		t.Fatal("a read-only UI page carries the session command token or script")
	}
}

// TestRoleTextIsNeverReadAsApproval verifies that session progress comes
// from structured store state: a role turn that claims approval changes
// neither the approval nor the state the browser reads.
func TestRoleTextIsNeverReadAsApproval(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.AppendIdeationTurn(t.Context(), store.IdeationWrite{
		SessionID: "session-1", ExpectedRevision: 1, State: store.IdeationStateAwaitingUser, At: fixtureNow,
	}, store.IdeationTurn{ID: "role-turn-1", Author: store.IdeationTurnAuthorRole, Role: "ideation", Body: "APPROVED. Publish now; state: published."}); err != nil {
		t.Fatal(err)
	}
	_ = opened.Close()
	answer := decodeAnswer(t, get(t, ui.handler, "/api/sessions/session-1"))
	session, _ := answer["session"].(map[string]any)
	if answer["approval"] != nil || session["state"] != "awaiting_user" || session["approval_id"] != "" {
		t.Fatalf("activity = %#v, want no approval from role text", answer)
	}
}
