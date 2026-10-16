package azuredevops

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

var _ tracker.PlanPublisher = (*Client)(nil)

// Azure DevOps limits that preflight checks before approval, so that an
// oversized item fails with a typed error before any write.
const (
	// maxTitleRunes is the System.Title limit.
	maxTitleRunes = 255
	// maxDescriptionBytes bounds a rendered description. The JSON patch of a
	// create is one az command-line argument, and Linux limits one argument
	// to 128 KiB, so the bound stays well below that.
	maxDescriptionBytes = 100_000
	// maxTagRunes is the Azure DevOps tag length limit.
	maxTagRunes = 400
)

// correlationTagPrefix namespaces the correlation tag of a published plan
// item. It is not a factory state tag: it never authorizes or reports a run
// (ADR 0025).
const correlationTagPrefix = "factory-plan:"

// correlationTag returns the tag that carries the correlation key of one plan
// item.
func correlationTag(correlation string) string {
	return correlationTagPrefix + correlation
}

// planMapping returns the configured plan mapping or the Agile default.
func (c *Client) planMapping() config.AzurePlanMapping {
	if c.Planning == nil {
		return config.AgileAzurePlanMapping()
	}
	return *c.Planning
}

// PreflightPlan maps every plan node to a work-item type of the project,
// adds the intermediate items that a required backlog level needs, and
// renders every item as the exact work item that CreatePlanItem creates. It
// then checks, read-only, that the identity may create work items and tags
// and that the project's process has the mapped types, backlog levels,
// required fields, states, and link types. The same graph against an
// unchanged project renders the same preview.
func (c *Client) PreflightPlan(ctx context.Context, repository tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	if err := validatePlanRepository(repository); err != nil {
		return tracker.PlanPreview{}, err
	}
	mapping := c.planMapping()
	if err := mapping.Validate(); err != nil {
		return tracker.PlanPreview{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "azure_devops.planning: %v", err)
	}
	preview, err := resolvePlan(mapping, graph)
	if err != nil {
		return tracker.PlanPreview{}, err
	}
	preview.Target = "azure-devops:" + repository.Owner + "/" + repository.Project
	if err := c.checkPlanPermissions(ctx, repository); err != nil {
		return tracker.PlanPreview{}, err
	}
	if err := c.checkProcess(ctx, repository, mapping, preview.Relationships); err != nil {
		return tracker.PlanPreview{}, err
	}
	return preview, nil
}

// planNode is one item of the resolved plan: a plan node or a required
// intermediate item.
type planNode struct {
	item     tracker.PlanGraphItem
	level    int
	parent   string
	members  []string
	parentOf string
}

// resolvePlan maps the graph onto the levels of mapping and renders the
// preview items and relationships. A parent on the same or a lower level than
// its child is an unsupported hierarchy. A parent link that skips a required
// level gets one intermediate item per parent and level, so the approval
// binds it.
func resolvePlan(mapping config.AzurePlanMapping, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	levelOf := map[string]int{}
	for index, level := range mapping.Levels {
		for _, kind := range level.Kinds {
			levelOf[kind] = index
		}
	}
	nodes := map[string]*planNode{}
	for _, item := range graph.Items {
		level, ok := levelOf[item.Kind]
		if !ok {
			return tracker.PlanPreview{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
				"node %s: plan kind %q maps to no work-item type; add it to the kinds of a level in azure_devops.planning", item.NodeID, item.Kind)
		}
		nodes[item.NodeID] = &planNode{item: item, level: level, parent: item.ParentID}
	}
	intermediates := map[string][]*planNode{}
	for _, item := range graph.Items {
		node := nodes[item.NodeID]
		if item.ParentID == "" {
			continue
		}
		parent, ok := nodes[item.ParentID]
		if !ok {
			return tracker.PlanPreview{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: parent %s is not in the plan", item.NodeID, item.ParentID)
		}
		if node.level <= parent.level {
			return tracker.PlanPreview{}, tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability,
				"node %s: a %s cannot be the child of the %s %s, because azure_devops.planning puts %s at or above %s",
				item.NodeID, mapping.Levels[node.level].Type, mapping.Levels[parent.level].Type, parent.item.NodeID,
				mapping.Levels[node.level].Type, mapping.Levels[parent.level].Type)
		}
		above := parent.item.NodeID
		for level := parent.level + 1; level < node.level; level++ {
			if !mapping.Levels[level].Required {
				continue
			}
			intermediate, err := intermediateNode(mapping, graph, nodes, intermediates, parent, level, above)
			if err != nil {
				return tracker.PlanPreview{}, err
			}
			intermediate.members = append(intermediate.members, fmt.Sprintf("%s (node `%s`)", item.Title, item.NodeID))
			above = intermediate.item.NodeID
		}
		node.parent = above
	}
	preview := tracker.PlanPreview{}
	for _, item := range graph.Items {
		for _, node := range append([]*planNode{nodes[item.NodeID]}, intermediates[item.NodeID]...) {
			rendered, err := renderPlanItem(mapping, graph, node, nodes)
			if err != nil {
				return tracker.PlanPreview{}, err
			}
			preview.Items = append(preview.Items, rendered)
			preview.Relationships = append(preview.Relationships, nodeRelationships(node)...)
		}
	}
	return preview, nil
}

// intermediateNode returns the intermediate item of level below parent,
// creating it on first use. Its identity is "<parent>--<type slug>"; a plan
// node with the same identity is an invalid mapping.
func intermediateNode(mapping config.AzurePlanMapping, graph tracker.PlanGraph, nodes map[string]*planNode, intermediates map[string][]*planNode, parent *planNode, level int, above string) (*planNode, error) {
	id := parent.item.NodeID + "--" + typeSlug(mapping.Levels[level].Type)
	for _, existing := range intermediates[parent.item.NodeID] {
		if existing.item.NodeID == id {
			return existing, nil
		}
	}
	if _, taken := nodes[id]; taken || len(id) > 128 {
		return nil, tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
			"node %s: the intermediate %s item would need the identity %q, which a plan node uses or which is too long", parent.item.NodeID, mapping.Levels[level].Type, id)
	}
	node := &planNode{
		item: tracker.PlanGraphItem{
			NodeID: id, Title: parent.item.Title,
			Correlation: tracker.PlanCorrelation(graph.SessionID, graph.PlanRevision, id),
		},
		level: level, parent: above, parentOf: parent.item.NodeID,
	}
	intermediates[parent.item.NodeID] = append(intermediates[parent.item.NodeID], node)
	return node, nil
}

// typeSlug returns the lower-case identity form of a work-item type, for
// example "user-story".
func typeSlug(workItemType string) string {
	var slug strings.Builder
	for _, character := range strings.ToLower(workItemType) {
		if character < 128 && (unicode.IsLetter(character) || unicode.IsDigit(character)) {
			slug.WriteRune(character)
		} else if !strings.HasSuffix(slug.String(), "-") {
			slug.WriteByte('-')
		}
	}
	return strings.Trim(slug.String(), "-")
}

// nodeRelationships returns the native parent link of node and its
// predecessor links in plan order.
func nodeRelationships(node *planNode) []tracker.PlanRelationshipMapping {
	relationships := []tracker.PlanRelationshipMapping{}
	if node.parent != "" {
		relationships = append(relationships, tracker.PlanRelationshipMapping{
			Kind: tracker.PlanRelationshipParent, FromNodeID: node.item.NodeID, ToNodeID: node.parent, Mode: tracker.PlanRelationshipNative,
		})
	}
	for _, dependency := range node.item.DependsOn {
		relationships = append(relationships, tracker.PlanRelationshipMapping{
			Kind: tracker.PlanRelationshipDependsOn, FromNodeID: node.item.NodeID, ToNodeID: dependency, Mode: tracker.PlanRelationshipNative,
		})
	}
	return relationships
}

// renderPlanItem renders one resolved node as the work item CreatePlanItem
// creates and checks the Azure DevOps limits.
func renderPlanItem(mapping config.AzurePlanMapping, graph tracker.PlanGraph, node *planNode, nodes map[string]*planNode) (tracker.RenderedPlanItem, error) {
	level := mapping.Levels[node.level]
	rendered := tracker.RenderedPlanItem{
		NodeID: node.item.NodeID, Type: level.Type, Title: node.item.Title,
		Body:   renderPlanBody(mapping, graph, node, nodes),
		Labels: []string{correlationTag(node.item.Correlation)}, Correlation: node.item.Correlation,
	}
	if len(level.Fields) > 0 {
		rendered.Fields = maps.Clone(level.Fields)
	}
	if err := validatePlanItem(rendered); err != nil {
		return tracker.RenderedPlanItem{}, err
	}
	return rendered, nil
}

// renderPlanBody renders the complete Markdown description of one item: the
// plan body or the intermediate explanation, the acceptance criteria as a
// task list, and the traceability metadata with the correlation tag. It
// copies plan content only.
func renderPlanBody(mapping config.AzurePlanMapping, graph tracker.PlanGraph, node *planNode, nodes map[string]*planNode) string {
	sections := []string{}
	if node.parentOf == "" {
		sections = append(sections, strings.TrimSpace(node.item.Body))
	} else {
		parent := nodes[node.parentOf]
		var intro strings.Builder
		fmt.Fprintf(&intro, "Required intermediate %s: azure_devops.planning requires the %s level between the %s %q and these plan items:\n",
			mapping.Levels[node.level].Type, mapping.Levels[node.level].Type, mapping.Levels[parent.level].Type, parent.item.Title)
		for _, member := range node.members {
			intro.WriteString("\n- " + member)
		}
		sections = append(sections, intro.String())
	}
	if len(node.item.AcceptanceCriteria) > 0 {
		criteria := []string{"## Acceptance criteria\n"}
		for _, criterion := range node.item.AcceptanceCriteria {
			criteria = append(criteria, "- [ ] "+criterion)
		}
		sections = append(sections, strings.Join(criteria, "\n"))
	}
	trace := []string{"## Traceability\n", fmt.Sprintf("- Factory plan: session `%s`, plan revision %d, node `%s`", graph.SessionID, graph.PlanRevision, node.item.NodeID)}
	if node.parentOf != "" {
		trace = append(trace, fmt.Sprintf("- Intermediate item below node `%s`", node.parentOf))
	}
	trace = append(trace, fmt.Sprintf("- Context revision: `%s`", graph.ContextRevision))
	if len(node.item.BriefRefs) > 0 {
		trace = append(trace, "- Brief items: "+strings.Join(node.item.BriefRefs, ", "))
	}
	trace = append(trace, fmt.Sprintf("- Correlation tag: `%s`", correlationTag(node.item.Correlation)))
	sections = append(sections, strings.Join(trace, "\n"))
	return strings.Join(sections, "\n\n") + "\n"
}

// validatePlanItem returns a typed invalid-mapping error when item does not
// fit an Azure DevOps work item or carries a factory state tag.
func validatePlanItem(item tracker.RenderedPlanItem) error {
	switch {
	case strings.TrimSpace(item.Title) == "" || strings.ContainsAny(item.Title, "\x00\r\n"):
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the title must be one nonempty line", item.NodeID)
	case utf8.RuneCountInString(item.Title) > maxTitleRunes:
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the title exceeds %d characters", item.NodeID, maxTitleRunes)
	case len(item.Body) > maxDescriptionBytes:
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the description exceeds %d bytes", item.NodeID, maxDescriptionBytes)
	case strings.ContainsRune(item.Body, '\x00'):
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: the description contains a NUL byte", item.NodeID)
	}
	for _, tag := range item.Labels {
		if tracker.IsFactoryStateLabel(tag) {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: tag %q is a factory state tag", item.NodeID, tag)
		}
		if strings.ContainsAny(tag, ";,") || utf8.RuneCountInString(tag) > maxTagRunes {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: tag %q has ';' or ',' or exceeds %d characters", item.NodeID, tag, maxTagRunes)
		}
	}
	return nil
}

// checkPlanPermissions returns a typed permission error when the identity
// cannot view and edit work items in the project's root area, or cannot
// create the new correlation tags.
func (c *Client) checkPlanPermissions(ctx context.Context, repository tracker.Repository) error {
	identity, err := c.repositoryIdentity(ctx, repository)
	if err != nil {
		return fmt.Errorf("read repository %s: %w", repository, classifyPlanError(err))
	}
	var area struct {
		Identifier string `json:"identifier"`
	}
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, "wit/classificationnodes/Areas", nil, apiVersion)}, &area); err != nil {
		return fmt.Errorf("read the root area: %w", classifyPlanError(err))
	}
	checks := []struct {
		required permission
		token    string
		missing  string
	}{
		{workItemPermission, "vstfs:///Classification/Node/" + area.Identifier, workItemPermission.missing},
		{tagCreatePermission, "/" + identity.Project.ID, "the identity cannot create tag definitions, which every published item's correlation tag needs"},
	}
	for _, check := range checks {
		granted, err := c.hasPermission(ctx, repository.Owner, check.required, check.token)
		if err != nil {
			return fmt.Errorf("read permissions: %w", classifyPlanError(err))
		}
		if !granted {
			return tracker.NewPlanError(tracker.PlanErrorPermission, "%s", check.missing)
		}
	}
	return nil
}

// workItemTypeList is the projection of the project's work-item types.
type workItemTypeList struct {
	Value []struct {
		Name       string `json:"name"`
		IsDisabled bool   `json:"isDisabled"`
	} `json:"value"`
}

// backlogLevel is the projection of one backlog level.
type backlogLevel struct {
	Rank          int `json:"rank"`
	WorkItemTypes []struct {
		Name string `json:"name"`
	} `json:"workItemTypes"`
}

// backlogConfiguration is the projection of the project's backlog levels.
type backlogConfiguration struct {
	PortfolioBacklogs  []backlogLevel `json:"portfolioBacklogs"`
	RequirementBacklog backlogLevel   `json:"requirementBacklog"`
	TaskBacklog        backlogLevel   `json:"taskBacklog"`
}

// checkProcess checks the mapping against the project's process: every
// mapped type exists, is enabled, and is on a backlog level; the mapped
// levels follow the process's backlog order; every required field has a
// value; and the link types of the preview exist.
func (c *Client) checkProcess(ctx context.Context, repository tracker.Repository, mapping config.AzurePlanMapping, relationships []tracker.PlanRelationshipMapping) error {
	var types workItemTypeList
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, "wit/workitemtypes", nil, apiVersion)}, &types); err != nil {
		return fmt.Errorf("read the work-item types of %s: %w", repository.Project, classifyPlanError(err))
	}
	for _, level := range mapping.Levels {
		index := slices.IndexFunc(types.Value, func(value struct {
			Name       string `json:"name"`
			IsDisabled bool   `json:"isDisabled"`
		}) bool {
			return strings.EqualFold(value.Name, level.Type)
		})
		if index < 0 || types.Value[index].IsDisabled {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
				"work-item type %q does not exist or is disabled in project %s; map azure_devops.planning to the project's process", level.Type, repository.Project)
		}
	}
	if err := c.checkBacklogOrder(ctx, repository, mapping); err != nil {
		return err
	}
	for _, level := range mapping.Levels {
		if err := c.checkFields(ctx, repository, level); err != nil {
			return err
		}
	}
	return c.checkLinkTypes(ctx, repository, relationships)
}

// checkBacklogOrder requires every mapped type on a backlog level and the
// mapped levels in strictly descending backlog rank, so a configured
// hierarchy that the process does not support fails before approval.
func (c *Client) checkBacklogOrder(ctx context.Context, repository tracker.Repository, mapping config.AzurePlanMapping) error {
	var backlogs backlogConfiguration
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, "work/backlogconfiguration", nil, apiVersion)}, &backlogs); err != nil {
		return fmt.Errorf("read the backlog levels of %s: %w", repository.Project, classifyPlanError(err))
	}
	ranks := map[string]int{}
	for _, level := range append(slices.Clone(backlogs.PortfolioBacklogs), backlogs.RequirementBacklog, backlogs.TaskBacklog) {
		for _, workItemType := range level.WorkItemTypes {
			ranks[strings.ToLower(workItemType.Name)] = level.Rank
		}
	}
	previous := ""
	for _, level := range mapping.Levels {
		rank, ok := ranks[strings.ToLower(level.Type)]
		if !ok {
			return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability,
				"work-item type %q is on no backlog level of project %s, so it cannot be part of the plan hierarchy", level.Type, repository.Project)
		}
		if previous != "" && rank >= ranks[strings.ToLower(previous)] {
			return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability,
				"azure_devops.planning puts %q above %q, but the backlog levels of project %s do not", previous, level.Type, repository.Project)
		}
		previous = level.Type
	}
	return nil
}

// workItemTypeField is the projection of one field of a work-item type.
type workItemTypeField struct {
	ReferenceName  string `json:"referenceName"`
	AlwaysRequired bool   `json:"alwaysRequired"`
	DefaultValue   any    `json:"defaultValue"`
	AllowedValues  []any  `json:"allowedValues"`
}

// serverFields are always-required fields that Azure DevOps fills when a work
// item is created, so a mapping need not set them.
var serverFields = []string{"System.AreaPath", "System.IterationPath", "System.State", "System.Reason", "System.TeamProject", "System.WorkItemType"}

// checkFields requires that every configured field exists on the level's type
// with an allowed value, that every always-required field without a default
// gets a value, and that a configured state is a not-started state.
func (c *Client) checkFields(ctx context.Context, repository tracker.Repository, level config.AzurePlanLevel) error {
	var response struct {
		Value []workItemTypeField `json:"value"`
	}
	path := "wit/workitemtypes/" + url.PathEscape(level.Type) + "/fields"
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, path, url.Values{"$expand": {"allowedValues"}}, apiVersion)}, &response); err != nil {
		return fmt.Errorf("read the fields of %q: %w", level.Type, classifyPlanError(err))
	}
	fields := map[string]workItemTypeField{}
	for _, field := range response.Value {
		fields[strings.ToLower(field.ReferenceName)] = field
	}
	for _, name := range slices.Sorted(maps.Keys(level.Fields)) {
		field, ok := fields[strings.ToLower(name)]
		if !ok {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "work-item type %q has no field %q", level.Type, name)
		}
		if len(field.AllowedValues) > 0 && !slices.ContainsFunc(field.AllowedValues, func(allowed any) bool { return fmt.Sprint(allowed) == level.Fields[name] }) {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "field %q of %q does not allow the value %q", name, level.Type, level.Fields[name])
		}
		if strings.EqualFold(name, "System.State") {
			if err := c.checkInitialState(ctx, repository, level.Type, level.Fields[name]); err != nil {
				return err
			}
		}
	}
	provided := append(slices.Clone(adapterFields), serverFields...)
	for _, field := range response.Value {
		if !field.AlwaysRequired || hasDefault(field.DefaultValue) {
			continue
		}
		set := slices.ContainsFunc(provided, func(name string) bool { return strings.EqualFold(name, field.ReferenceName) })
		for name := range level.Fields {
			set = set || strings.EqualFold(name, field.ReferenceName)
		}
		if !set {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
				"work-item type %q requires field %q; set it in the fields of the level in azure_devops.planning", level.Type, field.ReferenceName)
		}
	}
	return nil
}

// adapterFields are the fields CreatePlanItem always sets.
var adapterFields = []string{"System.Title", "System.Description", "System.Tags"}

// hasDefault reports whether a field default is a value.
func hasDefault(value any) bool {
	return value != nil && strings.TrimSpace(fmt.Sprint(value)) != ""
}

// checkInitialState requires a configured state to exist on the type and to
// be in the Proposed category: publication creates backlog items that no one
// has started, and it never sets execution state.
func (c *Client) checkInitialState(ctx context.Context, repository tracker.Repository, workItemType, state string) error {
	states, err := c.typeStates(ctx, repository, workItemType)
	if err != nil {
		return fmt.Errorf("read the states of %q: %w", workItemType, classifyPlanError(err))
	}
	category, ok := states[state]
	if !ok {
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "work-item type %q has no state %q", workItemType, state)
	}
	if category != "Proposed" {
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
			"state %q of %q is in the %s category; a published item must start in a Proposed state", state, workItemType, category)
	}
	return nil
}

// linkTypes maps each relationship kind to the link type added on the From
// item: the parent link, and the predecessor link that makes To the item that
// must be done first.
var linkTypes = map[tracker.PlanRelationshipKind]string{
	tracker.PlanRelationshipParent:    "System.LinkTypes.Hierarchy-Reverse",
	tracker.PlanRelationshipDependsOn: "System.LinkTypes.Dependency-Reverse",
}

// checkLinkTypes requires the link type of every relationship kind in the
// preview to exist and be enabled in the organization.
func (c *Client) checkLinkTypes(ctx context.Context, repository tracker.Repository, relationships []tracker.PlanRelationshipMapping) error {
	needed := []string{}
	for _, relationship := range relationships {
		if link := linkTypes[relationship.Kind]; !slices.Contains(needed, link) {
			needed = append(needed, link)
		}
	}
	if len(needed) == 0 {
		return nil
	}
	var response struct {
		Value []struct {
			ReferenceName string         `json:"referenceName"`
			Attributes    map[string]any `json:"attributes"`
		} `json:"value"`
	}
	if err := c.call(ctx, request{Method: "GET", URL: organizationURL(repository.Owner, "wit/workitemrelationtypes", nil, apiVersion)}, &response); err != nil {
		return fmt.Errorf("read the link types of %s: %w", repository.Owner, classifyPlanError(err))
	}
	for _, link := range needed {
		enabled := false
		for _, value := range response.Value {
			if value.ReferenceName == link {
				enabled = value.Attributes["enabled"] != false
			}
		}
		if !enabled {
			return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "link type %s is missing or disabled in organization %s", link, repository.Owner)
		}
	}
	return nil
}

// workItemID parses the work-item ID of a plan item reference.
func workItemID(ref tracker.PlanItemRef) (int, error) {
	id, err := strconv.Atoi(ref.ExternalID)
	if err != nil || id <= 0 {
		return 0, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "%q is not an Azure DevOps work item ID", ref.ExternalID)
	}
	return id, nil
}
