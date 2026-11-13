package webui

import (
	"context"
	"net/http"
	"slices"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// LocalOperator is retained local backlog, approval and supported Run input.
// Reads perform no Git/network effects; every mutation uses Sessions' inbox.
type LocalOperator interface {
	LocalBacklog(context.Context) (factory.LocalBacklogView, error)
	LocalExecution(context.Context) (factory.LocalExecutionOverview, error)
	LocalRunOperator(context.Context, string) (factory.LocalRunOperatorView, error)
}

const pageLocal page = "local"

// localItemView keeps source waiting state separate from an active frozen Run.
type localItemView struct {
	store.LocalBacklogItem
	Body       string
	Executable bool
}

// executionIssueView is exact preview source plus the explicit selected subset.
type executionIssueView struct {
	store.LocalExecutionIssue
	Title, Body string
	Selected    bool
}

// executionPreviewView is one durable, content-bound preview ready for approval.
type executionPreviewView struct {
	store.LocalExecutionPreview
	Issues []executionIssueView
	Stale  bool
}

// localPage holds only retained coordinator projections, including while stopped.
type localPage struct {
	Supervisor *factory.SupervisorView
	Projection store.LocalBacklogProjection
	Items      []localItemView
	Item       *localItemView
	Execution  factory.LocalExecutionOverview
	Preview    *executionPreviewView
	CanPreview bool
}

// localIssues renders the backlog or one stable identity without refreshing Git.
func (ui *server) localIssues(w http.ResponseWriter, r *http.Request) {
	backlog, err := ui.options.Local.LocalBacklog(r.Context())
	if err != nil {
		ui.renderStoreError(w, err)
		return
	}
	execution, err := ui.options.Local.LocalExecution(r.Context())
	if err != nil {
		ui.renderStoreError(w, err)
		return
	}
	model := localPage{Supervisor: &backlog.Supervisor, Projection: backlog.Projection, Execution: execution}
	id := r.PathValue("id")
	if id != "" && !localissues.ValidID(id) {
		ui.render(w, http.StatusBadRequest, pageError, errorPage{Title: "Invalid local issue", Message: "A local issue uses its stable local identity."})
		return
	}
	for _, item := range backlog.Projection.Items {
		view := localItemView{LocalBacklogItem: item, Body: string(item.Issue.Bytes), Executable: item.Issue.Kind == localissues.KindTask}
		model.Items = append(model.Items, view)
		if view.Executable && (id == "" || id == view.Issue.ID) {
			model.CanPreview = true
		}
		if item.Issue.ID == id {
			copy := view
			model.Item = &copy
		}
	}
	if id != "" && model.Item == nil {
		ui.render(w, http.StatusNotFound, pageError, errorPage{Title: "Local issue not observed", Message: "No retained source has this identity. Start the coordinator to observe committed issues; files alone grant no execution authority."})
		return
	}
	if preview := execution.Preview; preview != nil && (id == "" || slices.Contains(preview.Binding.SelectedIssueIDs, id)) {
		view := executionPreviewView{LocalExecutionPreview: *preview}
		view.Stale = backlog.Projection.Error != "" || backlog.Projection.GraphDigest != "" && backlog.Projection.GraphDigest != preview.Binding.GraphDigest
		for _, issue := range preview.Binding.Issues {
			title := issue.ID
			if parsed, err := localissues.Parse(issue.Path, issue.Bytes); err == nil {
				title = parsed.Title
			}
			view.Issues = append(view.Issues, executionIssueView{LocalExecutionIssue: issue, Title: title, Body: string(issue.Bytes), Selected: slices.Contains(preview.Binding.SelectedIssueIDs, issue.ID)})
		}
		model.Preview = &view
	}
	ui.render(w, http.StatusOK, pageLocal, model)
}

// localRunPage adds supported protected input without replacing Run inspection.
type localRunPage struct {
	factory.RunDetail
	Operator      *factory.LocalRunOperatorView
	OperatorError string
}
