package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// csrfHeader carries the per-process command token on every mutation.
const csrfHeader = "X-Factory-CSRF"

// Sessions is the session command and read surface the UI serves.
// *factory.Service implements it. The UI only submits commands; the
// coordinator applies them.
type Sessions interface {
	SubmitSessionCommand(context.Context, store.IdeationCommand) (factory.SessionCommandAck, error)
	SessionCommand(context.Context, string) (factory.SessionCommandView, error)
	SessionActivity(context.Context, string) (factory.SessionActivity, error)
	SessionOverview(context.Context) (factory.SessionOverview, error)
}

// sessionRoutes serves the session command API.
type sessionRoutes struct {
	sessions Sessions
	token    string
}

// newCSRFToken returns a random per-process token. A page on another origin
// cannot read it, so it cannot send an accepted command.
func newCSRFToken() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}

// register adds the session routes to mux.
func (routes *sessionRoutes) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/commands", routes.submit)
	mux.HandleFunc("GET /api/commands/{id}", routes.command)
	mux.HandleFunc("GET /api/sessions/{id}", routes.activity)
	mux.HandleFunc("GET /api/sessions", routes.overview)
}

// answer is the JSON answer of the command API. Fields that do not apply
// are left out.
type answer struct {
	RequestID        string          `json:"request_id,omitempty"`
	SessionID        string          `json:"session_id,omitempty"`
	Kind             string          `json:"kind,omitempty"`
	State            string          `json:"state,omitempty"`
	Outcome          string          `json:"outcome,omitempty"`
	Detail           string          `json:"detail,omitempty"`
	Replayed         *bool           `json:"replayed,omitempty"`
	ExpectedRevision int64           `json:"expected_revision,omitempty"`
	CurrentRevision  int64           `json:"current_revision,omitempty"`
	SessionRevision  int64           `json:"session_revision,omitempty"`
	Attempts         int             `json:"attempts,omitempty"`
	AcceptedAt       string          `json:"accepted_at,omitempty"`
	CompletedAt      string          `json:"completed_at,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
}

// submit checks that the request comes from a UI page, parses it strictly,
// and hands it to the inbox. It never applies the command.
func (routes *sessionRoutes) submit(w http.ResponseWriter, r *http.Request) {
	if status, reason := routes.refuseForeign(r); status != 0 {
		writeJSON(w, status, answer{Outcome: "forbidden", Detail: reason})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sessioncommand.MaxRequestBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, answer{Outcome: "invalid", Detail: "the request is larger than the command limit"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, answer{Outcome: "invalid", Detail: "read request: " + err.Error()})
		return
	}
	command, err := sessioncommand.ParseRequest(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, answer{Outcome: "invalid", Detail: err.Error()})
		return
	}
	ack, err := routes.sessions.SubmitSessionCommand(r.Context(), command)
	if err != nil {
		status, refusal := submitRefusal(err, command)
		writeJSON(w, status, refusal)
		return
	}
	status := http.StatusAccepted
	if ack.Replayed {
		status = http.StatusOK
	}
	result := commandAnswer(ack.Command)
	result.Replayed = &ack.Replayed
	writeJSON(w, status, result)
}

// refuseForeign returns a non-zero status when the request does not come
// from a page of this UI: a POST must name this origin, may not be marked
// cross-site, must carry JSON, and must carry the token from a UI page.
func (routes *sessionRoutes) refuseForeign(r *http.Request) (int, string) {
	if r.Header.Get("Origin") != "http://"+r.Host {
		return http.StatusForbidden, "the request does not come from this UI origin"
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return http.StatusForbidden, "the browser marked the request as " + site
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return http.StatusUnsupportedMediaType, "the request must be application/json"
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(routes.token)) != 1 {
		return http.StatusForbidden, "the request lacks the command token of this UI process; reload the page"
	}
	return 0, ""
}

// submitRefusal maps a submission error to its status and answer.
func submitRefusal(err error, command store.IdeationCommand) (int, answer) {
	refusal := answer{RequestID: command.RequestID, SessionID: command.SessionID, Kind: command.Kind, Detail: err.Error()}
	var conflict *store.IdeationRevisionConflictError
	switch {
	case errors.Is(err, factory.ErrSupervisorUnavailable):
		refusal.Outcome = "unavailable"
		return http.StatusServiceUnavailable, refusal
	case errors.As(err, &conflict):
		refusal.Outcome, refusal.ExpectedRevision, refusal.CurrentRevision = "conflict", conflict.Expected, conflict.Actual
		refusal.Detail = "the session changed since the page was read; reload it and decide again"
		return http.StatusConflict, refusal
	case errors.Is(err, store.ErrIdeationRequestReused):
		refusal.Outcome = "request_reused"
		return http.StatusConflict, refusal
	case errors.Is(err, store.ErrIdeationSessionExists):
		refusal.Outcome = "session_exists"
		return http.StatusConflict, refusal
	case errors.Is(err, store.ErrIdeationSessionNotFound):
		refusal.Outcome = "not_found"
		return http.StatusNotFound, refusal
	default:
		refusal.Outcome = "store_unavailable"
		return http.StatusServiceUnavailable, refusal
	}
}

// command answers the state and outcome of one command.
func (routes *sessionRoutes) command(w http.ResponseWriter, r *http.Request) {
	view, err := routes.sessions.SessionCommand(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrIdeationCommandNotFound) {
		writeJSON(w, http.StatusNotFound, answer{Outcome: "not_found", Detail: "no command has this request ID"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, answer{Outcome: "store_unavailable", Detail: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, commandAnswer(view.Command))
}

// commandAnswer is the answer for one stored command.
func commandAnswer(command store.IdeationCommand) answer {
	return answer{
		RequestID: command.RequestID, SessionID: command.SessionID, Kind: command.Kind, State: string(command.State),
		Outcome: string(command.Outcome), Detail: command.Detail, ExpectedRevision: command.ExpectedRevision,
		SessionRevision: command.SessionRevision, Attempts: command.Attempts, AcceptedAt: jsonTime(command.AcceptedAt),
		CompletedAt: jsonTime(command.CompletedAt), Result: command.Result,
	}
}

// activity answers the durable state of one session.
func (routes *sessionRoutes) activity(w http.ResponseWriter, r *http.Request) {
	activity, err := routes.sessions.SessionActivity(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrIdeationSessionNotFound) {
		writeJSON(w, http.StatusNotFound, answer{Outcome: "not_found", Detail: "no session has this identity"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, answer{Outcome: "store_unavailable", Detail: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, newActivityJSON(activity))
}

// overview answers the session list.
func (routes *sessionRoutes) overview(w http.ResponseWriter, r *http.Request) {
	overview, err := routes.sessions.SessionOverview(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, answer{Outcome: "store_unavailable", Detail: err.Error()})
		return
	}
	sessions := make([]sessionJSON, 0, len(overview.Sessions))
	for _, session := range overview.Sessions {
		sessions = append(sessions, newSessionJSON(session))
	}
	writeJSON(w, http.StatusOK, struct {
		Supervisor supervisorJSON `json:"supervisor"`
		Sessions   []sessionJSON  `json:"sessions"`
	}{Supervisor: newSupervisorJSON(overview.Supervisor), Sessions: sessions})
}

// writeJSON writes value as a JSON answer with status.
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// jsonTime renders a time in UTC RFC 3339 with nanoseconds; the zero time
// renders empty.
func jsonTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
