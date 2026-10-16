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

// GhClient publishes approved plans to GitHub issues (ADR 0022).
var _ tracker.PlanPublisher = (*GhClient)(nil)

// planIssueType is the rendered type of every published plan item: GitHub
// has one work-item type, the issue.
const planIssueType = "issue"

// planMarker returns the hidden body marker that carries the correlation key
// of one published plan item (ADR 0022).
func planMarker(correlation string) string {
	return "<!-- factory-plan: " + correlation + " -->"
}

// validatePlanRepository is validateRepository for the planning port: it
// returns the typed invalid-mapping error that every plan method reports.
func validatePlanRepository(repository tracker.Repository) error {
	if err := validateRepository(repository); err != nil {
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "%v", err)
	}
	return nil
}

// PreflightPlan resolves the repository access and the relationship
// capabilities of the target and renders every plan item as the exact issue
// that CreatePlanItem will create. It only reads from GitHub, and the same
// graph against an unchanged repository renders the same preview.
func (c *GhClient) PreflightPlan(ctx context.Context, repository tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	if err := validatePlanRepository(repository); err != nil {
		return tracker.PlanPreview{}, err
	}
	if err := c.checkIssueCreation(ctx, repository); err != nil {
		return tracker.PlanPreview{}, err
	}
	modes, err := c.relationshipModes(ctx, graph)
	if err != nil {
		return tracker.PlanPreview{}, err
	}
	relationships, err := planRelationships(graph, modes)
	if err != nil {
		return tracker.PlanPreview{}, err
	}
	if err := checkSubIssueLimits(graph, modes); err != nil {
		return tracker.PlanPreview{}, err
	}
	titles := make(map[string]string, len(graph.Items))
	for _, item := range graph.Items {
		titles[item.NodeID] = item.Title
	}
	preview := tracker.PlanPreview{Target: "github:" + repository.String(), Relationships: relationships}
	for _, item := range graph.Items {
		rendered := tracker.RenderedPlanItem{
			NodeID: item.NodeID, Type: planIssueType, Title: item.Title,
			Body: renderPlanBody(graph, item, textualRelationshipLines(item.NodeID, relationships, titles)), Correlation: item.Correlation,
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
// body, the acceptance criteria as a task list, the approved textual
// relationship fallback, the traceability metadata, and the hidden
// correlation marker. It copies plan content only.
func renderPlanBody(graph tracker.PlanGraph, item tracker.PlanGraphItem, textual []string) string {
	var body strings.Builder
	body.WriteString(strings.TrimSpace(item.Body))
	if len(item.AcceptanceCriteria) > 0 {
		body.WriteString("\n\n## Acceptance criteria\n\n")
		for _, criterion := range item.AcceptanceCriteria {
			fmt.Fprintf(&body, "- [ ] %s\n", criterion)
		}
	}
	if len(textual) > 0 {
		body.WriteString("\n## Plan relationships\n\n")
		body.WriteString("GitHub native sub-issues or dependencies are unavailable for this repository, so these\n" +
			"relationships use the approved textual fallback. After creation, a factory comment on the\n" +
			"sub-issue or the dependent issue links the issue numbers.\n\n")
		for _, line := range textual {
			body.WriteString(line + "\n")
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

// issueCapabilityQuery asks the GitHub GraphQL schema which fields an issue
// has. It is read-only and independent of the repository content.
const issueCapabilityQuery = `query { __type(name: "Issue") { fields { name } } }`

// capabilityResponse is the introspection projection of the issue fields.
type capabilityResponse struct {
	Data struct {
		Type struct {
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"__type"`
	} `json:"data"`
}

// relationshipModes resolves the representation of each relationship kind:
// native where the GitHub host offers sub-issues (field subIssues) or
// blocking dependencies (field blockedBy), and textual otherwise. A graph
// without relationships needs no probe.
func (c *GhClient) relationshipModes(ctx context.Context, graph tracker.PlanGraph) (map[tracker.PlanRelationshipKind]tracker.PlanRelationshipMode, error) {
	needed := false
	for _, item := range graph.Items {
		needed = needed || item.ParentID != "" || len(item.DependsOn) > 0
	}
	modes := map[tracker.PlanRelationshipKind]tracker.PlanRelationshipMode{
		tracker.PlanRelationshipParent:    tracker.PlanRelationshipTextual,
		tracker.PlanRelationshipDependsOn: tracker.PlanRelationshipTextual,
	}
	if !needed {
		return modes, nil
	}
	var response capabilityResponse
	if err := c.callJSON(ctx, []string{"api", "graphql", "-f", "query=" + issueCapabilityQuery}, nil, &response); err != nil {
		return nil, fmt.Errorf("read GitHub issue relationship capabilities: %w", classifyPlanError(err))
	}
	for _, field := range response.Data.Type.Fields {
		switch field.Name {
		case "subIssues":
			modes[tracker.PlanRelationshipParent] = tracker.PlanRelationshipNative
		case "blockedBy":
			modes[tracker.PlanRelationshipDependsOn] = tracker.PlanRelationshipNative
		}
	}
	return modes, nil
}

// planRelationships lists the relationships of graph in item order, each
// item's parent before its dependencies, with the resolved mode.
func planRelationships(graph tracker.PlanGraph, modes map[tracker.PlanRelationshipKind]tracker.PlanRelationshipMode) ([]tracker.PlanRelationshipMapping, error) {
	known := make(map[string]bool, len(graph.Items))
	for _, item := range graph.Items {
		known[item.NodeID] = true
	}
	var relationships []tracker.PlanRelationshipMapping
	add := func(kind tracker.PlanRelationshipKind, from, to string) error {
		if !known[to] || to == from {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: %s target %q is not another plan node", from, kind, to)
		}
		relationships = append(relationships, tracker.PlanRelationshipMapping{Kind: kind, FromNodeID: from, ToNodeID: to, Mode: modes[kind]})
		return nil
	}
	for _, item := range graph.Items {
		if item.ParentID != "" {
			if err := add(tracker.PlanRelationshipParent, item.NodeID, item.ParentID); err != nil {
				return nil, err
			}
		}
		for _, dependency := range item.DependsOn {
			if err := add(tracker.PlanRelationshipDependsOn, item.NodeID, dependency); err != nil {
				return nil, err
			}
		}
	}
	return relationships, nil
}

// GitHub sub-issue limits: at most 100 sub-issues per parent and eight
// levels of nested sub-issues.
const (
	maxSubIssuesPerParent = 100
	maxSubIssueDepth      = 8
)

// checkSubIssueLimits returns a typed unsupported-capability error when a
// native parent mapping exceeds a GitHub sub-issue limit.
func checkSubIssueLimits(graph tracker.PlanGraph, modes map[tracker.PlanRelationshipKind]tracker.PlanRelationshipMode) error {
	if modes[tracker.PlanRelationshipParent] != tracker.PlanRelationshipNative {
		return nil
	}
	parents := make(map[string]string, len(graph.Items))
	children := map[string]int{}
	for _, item := range graph.Items {
		if item.ParentID != "" {
			parents[item.NodeID] = item.ParentID
			children[item.ParentID]++
		}
	}
	for _, item := range graph.Items {
		if children[item.NodeID] > maxSubIssuesPerParent {
			return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "node %s has %d sub-issues; GitHub allows %d", item.NodeID, children[item.NodeID], maxSubIssuesPerParent)
		}
		depth := 0
		for node := item.NodeID; parents[node] != "" && depth <= len(graph.Items); node = parents[node] {
			depth++
		}
		if depth > maxSubIssueDepth {
			return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "node %s is nested %d levels deep; GitHub allows %d", item.NodeID, depth, maxSubIssueDepth)
		}
	}
	return nil
}

// textualRelationshipLines renders the textual fallback lines of node: its
// parent, its dependencies, and, for a parent, a task list of its
// sub-issues. Native relationships render no line.
func textualRelationshipLines(node string, relationships []tracker.PlanRelationshipMapping, titles map[string]string) []string {
	var lines []string
	for _, relationship := range relationships {
		if relationship.Mode != tracker.PlanRelationshipTextual {
			continue
		}
		switch {
		case relationship.FromNodeID == node && relationship.Kind == tracker.PlanRelationshipParent:
			lines = append(lines, fmt.Sprintf("- Part of: %s (`%s`)", titles[relationship.ToNodeID], relationship.ToNodeID))
		case relationship.FromNodeID == node && relationship.Kind == tracker.PlanRelationshipDependsOn:
			lines = append(lines, fmt.Sprintf("- Depends on: %s (`%s`)", titles[relationship.ToNodeID], relationship.ToNodeID))
		case relationship.ToNodeID == node && relationship.Kind == tracker.PlanRelationshipParent:
			lines = append(lines, fmt.Sprintf("- [ ] Sub-issue: %s (`%s`)", titles[relationship.FromNodeID], relationship.FromNodeID))
		}
	}
	return lines
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
	if err := validatePlanRepository(repository); err != nil {
		return tracker.PlanItemRef{}, err
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

// Correlation lookup bound: the lookup reads at most correlationPages pages
// of correlationPageSize issues, newest first.
const (
	correlationPageSize = 100
	correlationPages    = 10
)

// planIssueListing is the listing projection of one issue for the
// correlation lookup.
type planIssueListing struct {
	Number      int       `json:"number"`
	HTMLURL     string    `json:"html_url"`
	Body        string    `json:"body"`
	PullRequest *struct{} `json:"pull_request"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

// FindPlanItems returns every issue, in any state, that the authenticated
// account created and whose body carries the exact correlation marker. It
// lists the account's issues newest first through the issues endpoint,
// which sees a just-created issue, instead of the search index, which lags.
// The listing is bounded to the 1,000 newest issues of the account. An issue
// beyond the bound is not found; the publication ledger never creates after
// an unknown outcome without an operator decision, so the bound cannot cause
// a blind duplicate.
func (c *GhClient) FindPlanItems(ctx context.Context, repository tracker.Repository, correlation string) ([]tracker.PlanItemRef, error) {
	if err := validatePlanRepository(repository); err != nil {
		return nil, err
	}
	if strings.TrimSpace(correlation) == "" {
		return nil, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "a correlation key is required")
	}
	login, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return nil, err
	}
	marker := planMarker(correlation)
	found := []tracker.PlanItemRef{}
	for page := 1; page <= correlationPages; page++ {
		var issues []planIssueListing
		args := []string{
			"api", "repos/" + repository.String() + "/issues", "--method", "GET",
			"-f", "state=all", "-f", "creator=" + login, "-f", "sort=created", "-f", "direction=desc",
			"-f", "per_page=" + strconv.Itoa(correlationPageSize), "-f", "page=" + strconv.Itoa(page),
		}
		if err := c.callJSON(ctx, args, nil, &issues); err != nil {
			return nil, fmt.Errorf("list issues for correlation %s: %w", correlation, classifyPlanError(err))
		}
		for _, issue := range issues {
			if issue.PullRequest == nil && strings.EqualFold(issue.User.Login, login) && strings.Contains(issue.Body, marker) {
				found = append(found, tracker.PlanItemRef{ExternalID: strconv.Itoa(issue.Number), URL: issue.HTMLURL})
			}
		}
		if len(issues) < correlationPageSize {
			break
		}
	}
	return found, nil
}

// relationshipIssues resolves the issue numbers of both ends of
// relationship. An unusable identity is refused before any call.
func relationshipIssues(relationship tracker.PlanRelationship) (int, int, error) {
	from, fromErr := strconv.Atoi(relationship.From.ExternalID)
	to, toErr := strconv.Atoi(relationship.To.ExternalID)
	if fromErr != nil || toErr != nil || from <= 0 || to <= 0 || from == to {
		return 0, 0, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "relationship %s needs two different issue numbers, got %q and %q",
			relationship.Kind, relationship.From.ExternalID, relationship.To.ExternalID)
	}
	return from, to, nil
}

// nativeEndpoint returns the issue that the native link list names and the
// list path: a sub-issue is listed under its parent, and a dependency under
// the dependent issue as a blocker.
func nativeEndpoint(repository tracker.Repository, kind tracker.PlanRelationshipKind, from, to int) (member int, path string, err error) {
	switch kind {
	case tracker.PlanRelationshipParent:
		return from, fmt.Sprintf("repos/%s/issues/%d/sub_issues", repository, to), nil
	case tracker.PlanRelationshipDependsOn:
		return to, fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by", repository, from), nil
	}
	return 0, "", tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "unknown relationship kind %q", kind)
}

// CreatePlanRelationship creates one approved relationship between two
// created issues in its approved mode. It adds no label and edits no issue
// body.
func (c *GhClient) CreatePlanRelationship(ctx context.Context, repository tracker.Repository, relationship tracker.PlanRelationship) error {
	if err := validatePlanRepository(repository); err != nil {
		return err
	}
	from, to, err := relationshipIssues(relationship)
	if err != nil {
		return err
	}
	switch relationship.Mode {
	case tracker.PlanRelationshipTextual:
		return c.createBacklink(ctx, repository, relationship.Kind, from, to)
	case tracker.PlanRelationshipNative:
	default:
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "unknown relationship mode %q", relationship.Mode)
	}
	member, path, err := nativeEndpoint(repository, relationship.Kind, from, to)
	if err != nil {
		return err
	}
	var issue planIssueResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/%d", repository, member)}, nil, &issue); err != nil {
		return fmt.Errorf("read issue #%d: %w", member, classifyPlanError(err))
	}
	if issue.ID <= 0 {
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "issue #%d has no database identity", member)
	}
	payload := map[string]int64{"issue_id": issue.ID}
	if relationship.Kind == tracker.PlanRelationshipParent {
		payload = map[string]int64{"sub_issue_id": issue.ID}
	}
	if err := c.call(ctx, []string{"api", path, "--method", "POST"}, payload); err != nil {
		return fmt.Errorf("create %s link #%d -> #%d: %w", relationship.Kind, from, to, classifyPlanError(err))
	}
	return nil
}

// FindPlanRelationship reports whether the relationship already exists in
// its approved mode, so that a retry does not create a second link.
func (c *GhClient) FindPlanRelationship(ctx context.Context, repository tracker.Repository, relationship tracker.PlanRelationship) (bool, error) {
	if err := validatePlanRepository(repository); err != nil {
		return false, err
	}
	from, to, err := relationshipIssues(relationship)
	if err != nil {
		return false, err
	}
	switch relationship.Mode {
	case tracker.PlanRelationshipTextual:
		return c.findBacklink(ctx, repository, relationship.Kind, from, to)
	case tracker.PlanRelationshipNative:
	default:
		return false, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "unknown relationship mode %q", relationship.Mode)
	}
	member, path, err := nativeEndpoint(repository, relationship.Kind, from, to)
	if err != nil {
		return false, err
	}
	output, err := c.callBytes(ctx, []string{"api", path, "--method", "GET", "--paginate", "--slurp"}, nil)
	if err != nil {
		return false, fmt.Errorf("list %s links of the plan issue: %w", relationship.Kind, classifyPlanError(err))
	}
	linked, err := decodeJSONPages[planIssueResponse](output)
	if err != nil {
		return false, fmt.Errorf("decode %s links: %w", relationship.Kind, err)
	}
	for _, issue := range linked {
		if issue.Number == member {
			return true, nil
		}
	}
	return false, nil
}

// backlinkBody returns the textual fallback comment of one relationship. The
// visible line names the other issue, so GitHub also shows a backlink on it;
// the hidden marker lets FindPlanRelationship recognize the comment.
func backlinkBody(kind tracker.PlanRelationshipKind, from, to int) (string, error) {
	var line string
	switch kind {
	case tracker.PlanRelationshipParent:
		line = fmt.Sprintf("Part of #%d", to)
	case tracker.PlanRelationshipDependsOn:
		line = fmt.Sprintf("Depends on #%d", to)
	default:
		return "", tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "unknown relationship kind %q", kind)
	}
	return fmt.Sprintf("%s\n\n<!-- factory-plan-link: %s #%d -> #%d -->", line, kind, from, to), nil
}

// createBacklink posts the textual fallback comment on the sub-issue or the
// dependent issue.
func (c *GhClient) createBacklink(ctx context.Context, repository tracker.Repository, kind tracker.PlanRelationshipKind, from, to int) error {
	body, err := backlinkBody(kind, from, to)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("repos/%s/issues/%d/comments", repository, from)
	if err := c.call(ctx, []string{"api", path, "--method", "POST"}, map[string]string{"body": body}); err != nil {
		return fmt.Errorf("comment %s backlink on #%d: %w", kind, from, classifyPlanError(err))
	}
	return nil
}

// findBacklink reports whether the authenticated account already posted the
// textual fallback comment of one relationship.
func (c *GhClient) findBacklink(ctx context.Context, repository tracker.Repository, kind tracker.PlanRelationshipKind, from, to int) (bool, error) {
	body, err := backlinkBody(kind, from, to)
	if err != nil {
		return false, err
	}
	marker := body[strings.Index(body, "<!--"):]
	login, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return false, err
	}
	comments, err := c.IssueComments(ctx, repository, from)
	if err != nil {
		return false, classifyPlanError(err)
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) && strings.EqualFold(comment.Author, login) {
			return true, nil
		}
	}
	return false, nil
}
