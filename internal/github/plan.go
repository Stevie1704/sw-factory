package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// GitHub limits that preflight checks before approval, so that an oversized
// plan fails with a typed error before any write.
const (
	// maxIssueTitleRunes is the GitHub issue title limit.
	maxIssueTitleRunes = 256
	// maxIssueBodyRunes is the GitHub issue body limit.
	maxIssueBodyRunes = 65536
)

// planIssueType is the rendered type of every published plan item: GitHub
// has one work-item type, the issue.
const planIssueType = "issue"

// planMarker returns the hidden body marker that carries the correlation key
// of one published plan item (ADR 0022).
func planMarker(correlation string) string {
	return "<!-- factory-plan: " + correlation + " -->"
}

// PreflightPlan resolves the repository access and the relationship
// capabilities of the target and renders every plan item as the exact issue
// that CreatePlanItem will create. It only reads from GitHub, and the same
// graph against an unchanged repository renders the same preview.
func (c *GhClient) PreflightPlan(ctx context.Context, repository tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	if err := validateDoctorRepository(repository); err != nil {
		return tracker.PlanPreview{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "%v", err)
	}
	if err := c.checkIssueCreation(ctx, repository); err != nil {
		return tracker.PlanPreview{}, err
	}
	preview := tracker.PlanPreview{Target: "github:" + repository.String()}
	for _, item := range graph.Items {
		rendered := tracker.RenderedPlanItem{
			NodeID: item.NodeID, Type: planIssueType, Title: item.Title,
			Body: renderPlanBody(graph, item), Correlation: item.Correlation,
		}
		if err := validatePlanIssue(rendered); err != nil {
			return tracker.PlanPreview{}, err
		}
		preview.Items = append(preview.Items, rendered)
	}
	return preview, nil
}

// planRepositoryResponse is the repository projection that decides whether
// the authenticated account can open issues in the target.
type planRepositoryResponse struct {
	HasIssues   bool `json:"has_issues"`
	Archived    bool `json:"archived"`
	Permissions struct {
		Pull bool `json:"pull"`
	} `json:"permissions"`
}

// checkIssueCreation reads the repository and returns a typed error when the
// account cannot open issues there.
func (c *GhClient) checkIssueCreation(ctx context.Context, repository tracker.Repository) error {
	var response planRepositoryResponse
	if err := c.callJSON(ctx, []string{"api", "repos/" + repository.String()}, nil, &response); err != nil {
		return fmt.Errorf("read repository %s: %w", repository, classifyPlanError(err))
	}
	switch {
	case !response.Permissions.Pull:
		return tracker.NewPlanError(tracker.PlanErrorPermission, "the authenticated account cannot read %s", repository)
	case response.Archived:
		return tracker.NewPlanError(tracker.PlanErrorPermission, "%s is archived and read-only", repository)
	case !response.HasIssues:
		return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "%s has issues disabled", repository)
	}
	return nil
}

// renderPlanBody renders the complete Markdown body of one item: the plan
// body, the acceptance criteria as a task list, the traceability metadata,
// and the hidden correlation marker. It copies plan content only.
func renderPlanBody(graph tracker.PlanGraph, item tracker.PlanGraphItem) string {
	var body strings.Builder
	body.WriteString(strings.TrimSpace(item.Body))
	if len(item.AcceptanceCriteria) > 0 {
		body.WriteString("\n\n## Acceptance criteria\n\n")
		for _, criterion := range item.AcceptanceCriteria {
			fmt.Fprintf(&body, "- [ ] %s\n", criterion)
		}
	}
	body.WriteString("\n## Traceability\n\n")
	fmt.Fprintf(&body, "- Factory plan: session `%s`, plan revision %d, node `%s`\n", graph.SessionID, graph.PlanRevision, item.NodeID)
	fmt.Fprintf(&body, "- Context revision: `%s`\n", graph.ContextRevision)
	if len(item.BriefRefs) > 0 {
		fmt.Fprintf(&body, "- Brief items: %s\n", strings.Join(item.BriefRefs, ", "))
	}
	body.WriteString("\n" + planMarker(item.Correlation) + "\n")
	return body.String()
}

// validatePlanIssue returns a typed invalid-mapping error when item does not
// fit a GitHub issue or carries a factory state label.
func validatePlanIssue(item tracker.RenderedPlanItem) error {
	title := strings.TrimSpace(item.Title)
	switch {
	case title == "" || strings.ContainsAny(item.Title, "\x00\r\n"):
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the issue title must be one nonempty line", item.NodeID)
	case utf8.RuneCountInString(item.Title) > maxIssueTitleRunes:
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the issue title exceeds %d characters", item.NodeID, maxIssueTitleRunes)
	case utf8.RuneCountInString(item.Body) > maxIssueBodyRunes:
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the issue body exceeds %d characters", item.NodeID, maxIssueBodyRunes)
	case strings.ContainsRune(item.Body, '\x00'):
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the issue body contains a NUL byte", item.NodeID)
	}
	for _, label := range item.Labels {
		if tracker.IsFactoryStateLabel(label) {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: label %q is a factory state label", item.NodeID, label)
		}
	}
	return nil
}

// planIssueCreatePayload is the GitHub issue create request. Labels are sent
// only when the approved item has them; the GitHub preview renders none.
type planIssueCreatePayload struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// planIssueResponse is the issue projection of a planning response. ID is
// the database identity that the relationship endpoints need.
type planIssueResponse struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// ref returns the neutral identity of the issue.
func (r planIssueResponse) ref() tracker.PlanItemRef {
	return tracker.PlanItemRef{ExternalID: strconv.Itoa(r.Number), URL: r.HTMLURL}
}

// CreatePlanItem creates exactly the rendered issue. It never adds a label
// of its own and refuses a factory state label before any call.
func (c *GhClient) CreatePlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem) (tracker.PlanItemRef, error) {
	if err := validateDoctorRepository(repository); err != nil {
		return tracker.PlanItemRef{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "%v", err)
	}
	if err := validatePlanIssue(item); err != nil {
		return tracker.PlanItemRef{}, err
	}
	var response planIssueResponse
	payload := planIssueCreatePayload{Title: item.Title, Body: item.Body, Labels: item.Labels}
	if err := c.callJSON(ctx, []string{"api", "repos/" + repository.String() + "/issues", "--method", "POST"}, payload, &response); err != nil {
		return tracker.PlanItemRef{}, fmt.Errorf("create issue for node %s: %w", item.NodeID, classifyPlanError(err))
	}
	if response.Number <= 0 {
		return tracker.PlanItemRef{}, tracker.NewPlanError(tracker.PlanErrorAmbiguous, "create issue for node %s: GitHub returned no issue number", item.NodeID)
	}
	return response.ref(), nil
}

// classifyPlanError maps a failed gh call to a typed planning error where
// gh's message proves the outcome. A deadline expiry is ambiguous because
// GitHub may have applied the request. An HTTP rejection proves that GitHub
// made no change. Every other failure, including a rate limit or a server
// error, is returned unchanged, which leaves the outcome unknown.
func classifyPlanError(err error) error {
	var timeout *hostcmd.TimeoutError
	if errors.As(err, &timeout) {
		return &tracker.PlanError{Kind: tracker.PlanErrorAmbiguous, Err: err}
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "rate limit") {
		return err
	}
	for _, signal := range planFailureSignals {
		if strings.Contains(message, signal.text) {
			return &tracker.PlanError{Kind: signal.kind, Err: err}
		}
	}
	return err
}

// planFailureSignals maps gh error text to the planning error kind it
// proves. The first match wins.
var planFailureSignals = []struct {
	text string
	kind tracker.PlanErrorKind
}{
	{"http 401", tracker.PlanErrorPermission},
	{"http 403", tracker.PlanErrorPermission},
	{"http 404", tracker.PlanErrorPermission},
	{"http 410", tracker.PlanErrorUnsupportedCapability},
	{"http 422", tracker.PlanErrorInvalidMapping},
}
