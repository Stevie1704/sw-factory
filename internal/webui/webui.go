// Package webui serves the local, read-only web view of factory runs. It
// renders pages from a Reader, loads no external resource, and answers only
// requests addressed to the loopback host.
package webui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/Stevie1704/sw-factory/internal/factory"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed assets
var assetFiles embed.FS

// contentSecurityPolicy allows only same-origin styles, scripts, and fetches,
// so a rendered page cannot load or send anything to another origin.
const contentSecurityPolicy = "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Reader is the read-only run view the UI renders. *factory.Service
// implements it.
type Reader interface {
	RunOverview(context.Context) (factory.RunOverview, error)
	RunDetail(context.Context, string) (factory.RunDetail, error)
	RunEvaluation(context.Context, string) (factory.RunEvaluation, error)
}

// Options configure page behavior.
type Options struct {
	// RefreshInterval is how often a page reloads its content; 0 disables
	// automatic refresh.
	RefreshInterval time.Duration
}

// page names one template set: the layout plus one page body.
type page string

const (
	pageRuns       page = "runs"
	pageRun        page = "run"
	pageEvaluation page = "evaluation"
	pageError      page = "error"
)

// errorPage is the model of a page that explains why no content is shown.
type errorPage struct {
	Title   string
	Message string
	// Supervisor is nil because no store read succeeded.
	Supervisor *factory.SupervisorView
}

// layoutData is what the layout renders around one page model.
type layoutData struct {
	// RefreshSeconds is the automatic refresh interval; 0 turns it off.
	RefreshSeconds int
	// Page is the page model; it has a Supervisor field.
	Page any
}

// server renders the UI pages from one Reader.
type server struct {
	reader  Reader
	options Options
	pages   map[page]*template.Template
}

// NewHandler returns the UI routes behind the loopback Host check and the
// security headers. It panics when an embedded template does not parse,
// because that is a build defect, not a runtime condition.
func NewHandler(reader Reader, options Options) http.Handler {
	ui := &server{reader: reader, options: options, pages: parsePages()}
	assets, err := fs.Sub(assetFiles, "assets")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", ui.runList)
	mux.HandleFunc("GET /runs/{id}", ui.runDetail)
	mux.HandleFunc("GET /runs/{id}/evaluation", ui.runEvaluation)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	return protect(mux)
}

// parsePages parses the layout with each page body separately, so every page
// can define its own "title" and "content" templates.
func parsePages() map[page]*template.Template {
	functions := template.FuncMap{
		"formatTime": formatTime,
		"testPolicy": factory.TestPolicyDescription,
		"micros":     formatMicros,
	}
	pages := map[page]*template.Template{}
	for _, name := range []page{pageRuns, pageRun, pageEvaluation, pageError} {
		pages[name] = template.Must(template.New("").Funcs(functions).ParseFS(templateFiles, "templates/layout.html", "templates/"+string(name)+".html"))
	}
	return pages
}

// runList renders every persisted run.
func (ui *server) runList(w http.ResponseWriter, r *http.Request) {
	overview, err := ui.reader.RunOverview(r.Context())
	if err != nil {
		ui.renderStoreError(w, err)
		return
	}
	ui.render(w, http.StatusOK, pageRuns, overview)
}

// runDetail renders one run, or a not-found page for an unknown identity.
func (ui *server) runDetail(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !validRunID(runID) {
		ui.render(w, http.StatusBadRequest, pageError, errorPage{Title: "Invalid run", Message: "The run identity is empty or contains control characters."})
		return
	}
	detail, err := ui.reader.RunDetail(r.Context(), runID)
	if errors.Is(err, factory.ErrRunNotFound) {
		ui.render(w, http.StatusNotFound, pageError, errorPage{Title: "Run not found", Message: "No persisted run has the identity " + runID + "."})
		return
	}
	if err != nil {
		ui.renderStoreError(w, err)
		return
	}
	ui.render(w, http.StatusOK, pageRun, detail)
}

// runEvaluation renders one run's evaluation summary. It shows the summary
// alone when cleanup removed the run, and a not-found page when neither the
// run nor a summary exists.
func (ui *server) runEvaluation(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !validRunID(runID) {
		ui.render(w, http.StatusBadRequest, pageError, errorPage{Title: "Invalid run", Message: "The run identity is empty or contains control characters."})
		return
	}
	evaluation, err := ui.reader.RunEvaluation(r.Context(), runID)
	if errors.Is(err, factory.ErrRunNotFound) {
		ui.render(w, http.StatusNotFound, pageError, errorPage{Title: "Run not found", Message: "No persisted run or evaluation summary has the identity " + runID + "."})
		return
	}
	if err != nil {
		ui.renderStoreError(w, err)
		return
	}
	ui.render(w, http.StatusOK, pageEvaluation, evaluation)
}

// renderStoreError explains a failed store read. A refresh retries the read,
// so the status is 503 rather than 500.
func (ui *server) renderStoreError(w http.ResponseWriter, err error) {
	ui.render(w, http.StatusServiceUnavailable, pageError, errorPage{Title: "Operational store unavailable", Message: err.Error()})
}

// render executes a page into a buffer first, so a template error yields a
// clean 500 instead of a half-written page.
func (ui *server) render(w http.ResponseWriter, status int, name page, data any) {
	var body bytes.Buffer
	if err := ui.pages[name].ExecuteTemplate(&body, "layout", layoutData{RefreshSeconds: refreshSeconds(ui.options.RefreshInterval), Page: data}); err != nil {
		http.Error(w, "render page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = body.WriteTo(w)
}

// protect sets the security headers on every response and refuses a request
// whose Host is not the loopback name. The Host check stops a DNS-rebinding
// page from reading run data, including gate output, through the browser.
func protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-store")
		if !loopbackHost(r.Host) {
			http.Error(w, "the factory UI answers only on 127.0.0.1 or localhost", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHost reports whether a Host header names 127.0.0.1 or localhost,
// with or without a port.
func loopbackHost(host string) bool {
	name := host
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		name = hostname
	}
	return name == "127.0.0.1" || name == "localhost"
}

// validRunID rejects an empty identity or one with control characters before
// it reaches the store or a page.
func validRunID(runID string) bool {
	return runID != "" && !strings.ContainsFunc(runID, unicode.IsControl)
}

// refreshSeconds converts the refresh interval to whole seconds for the
// browser. A positive interval below one second becomes one second, so it
// does not turn refresh off.
func refreshSeconds(interval time.Duration) int {
	if interval <= 0 {
		return 0
	}
	return max(1, int(interval.Round(time.Second)/time.Second))
}

// formatTime renders a time in UTC RFC 3339; the zero time renders empty.
func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// formatMicros renders an amount in millionths of a currency unit as a
// decimal with six fraction digits, for example 1250000 as 1.250000.
func formatMicros(micros int64) string {
	return fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000)
}
