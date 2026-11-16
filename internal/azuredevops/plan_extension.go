package azuredevops

// This file holds the plan publication rules of the Azure DevOps CLI
// extension route (ADR 0030): explicit Epic/PBI mapping, per-PBI repository
// targets, and the argument rules of az boards work-item create.

import (
	"context"
	"fmt"
	"html"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// checkRouteSupportsPlan refuses, before any call, what the client's route
// cannot publish: the extension route needs an explicit planning mapping,
// because its default would substitute types; the az rest route has no
// repository assignment, so a plan target would be dropped silently.
func (c *Client) checkRouteSupportsPlan(graph tracker.PlanGraph) error {
	if c.extensionRoute() {
		if c.Planning == nil {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
				"the devops_extension route needs an explicit azure_devops.planning mapping, for example Epic for kind parent and Product Backlog Item for kinds task and investigation; Factory does not substitute a default mapping")
		}
		return nil
	}
	for _, item := range graph.Items {
		if item.TargetRepository != "" {
			return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability,
				"node %s names target repository %q, but repository targets need azure_devops.route devops_extension", item.NodeID, item.TargetRepository)
		}
	}
	return nil
}

// assignPlanTargets gives every executable item (the lowest mapped level)
// its target repository: the node's explicit target, or the registered
// repository when the node names none. A target is never taken from the
// parent. Each executable item gets exactly one factory-repo tag beside its
// correlation tag. A target on another level is refused, and every item must
// fit the arguments of az boards work-item create.
func (c *Client) assignPlanTargets(ctx context.Context, repository tracker.Repository, mapping config.AzurePlanMapping, graph tracker.PlanGraph, preview *tracker.PlanPreview) error {
	executable := mapping.Levels[len(mapping.Levels)-1].Type
	explicit := map[string]string{}
	for _, item := range graph.Items {
		explicit[item.NodeID] = item.TargetRepository
	}
	resolved := map[string]tracker.PlanTargetRepository{}
	for index := range preview.Items {
		item := &preview.Items[index]
		name := explicit[item.NodeID]
		if item.Type != executable && name != "" {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
				"node %s: a %s is not executable, so it takes no target repository; set the target on each of its %s items", item.NodeID, item.Type, executable)
		}
		if item.Type == executable {
			if name == "" {
				name = repository.Name
			}
			target, err := c.planTarget(ctx, repository, name, resolved)
			if err != nil {
				return err
			}
			item.TargetRepository = &target
			item.Labels = append(item.Labels, RepositoryAssignmentTag(target.ID))
		}
		if err := validateExtensionPlanItem(*item); err != nil {
			return err
		}
	}
	return nil
}

// planTarget resolves one registered target repository to its GUID with az
// repos show, once per name. A name that is neither the registered
// repository nor one of PlanRepositories is refused.
func (c *Client) planTarget(ctx context.Context, repository tracker.Repository, name string, resolved map[string]tracker.PlanTargetRepository) (tracker.PlanTargetRepository, error) {
	allowed := append([]string{repository.Name}, c.PlanRepositories...)
	index := slices.IndexFunc(allowed, func(candidate string) bool { return strings.EqualFold(candidate, name) })
	if index < 0 {
		return tracker.PlanTargetRepository{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping,
			"target repository %q is not registered; use %s or a repository of azure_devops.plan_repositories", name, repository.Name)
	}
	registered := allowed[index]
	if target, ok := resolved[strings.ToLower(registered)]; ok {
		return target, nil
	}
	identity, err := c.ResolveRepository(ctx, tracker.Repository{Owner: repository.Owner, Project: repository.Project, Name: registered})
	if err != nil {
		return tracker.PlanTargetRepository{}, classifyPlanError(err)
	}
	target := tracker.PlanTargetRepository{Name: registered, ID: identity.ID}
	resolved[strings.ToLower(registered)] = target
	return target, nil
}

// validateExtensionPlanItem returns a typed invalid-mapping error when item
// cannot be passed to az boards work-item create: a value that Azure CLI
// would read from a file, or a description that does not fit one
// command-line argument after HTML encoding.
func validateExtensionPlanItem(item tracker.RenderedPlanItem) error {
	if _, err := extensionCreateArgs(tracker.Repository{}, item); err != nil {
		return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "node %s: %v", item.NodeID, err)
	}
	return nil
}

// extensionCreateArgs returns the az boards work-item create arguments of
// item. The type, title, and description each go as one --flag=value
// argument, so a value that starts with '-' or spans lines is never parsed
// as an option. The tags and configured fields follow --fields as
// name=value pairs in a fixed order.
func extensionCreateArgs(repository tracker.Repository, item tracker.RenderedPlanItem) ([]string, error) {
	args := []string{"boards", "work-item", "create"}
	for _, flag := range [][2]string{{"--type", item.Type}, {"--title", item.Title}, {"--description", extensionDescription(item.Body)}} {
		arg, err := cliText(flag[0], flag[1])
		if err != nil {
			return nil, err
		}
		if len(arg) > maxRequestBodyBytes {
			return nil, fmt.Errorf("%s is %d bytes as one command-line argument, at most %d; shorten it", strings.TrimPrefix(flag[0], "--"), len(arg), maxRequestBodyBytes)
		}
		args = append(args, arg)
	}
	args = append(args, "--project", repository.Project, "--fields")
	pairs := [][2]string{{"System.Tags", joinTags(item.Labels)}}
	for _, name := range slices.Sorted(maps.Keys(item.Fields)) {
		pairs = append(pairs, [2]string{name, item.Fields[name]})
	}
	for _, pair := range pairs {
		if strings.HasPrefix(pair[1], "@") || strings.HasPrefix(pair[0], "-") {
			return nil, fmt.Errorf("field %s cannot start its value with '@' on the devops_extension route, because Azure CLI reads such a value from a file", pair[0])
		}
		args = append(args, pair[0]+"="+pair[1])
	}
	return args, nil
}

// extensionDescription renders the approved Markdown description as HTML
// that shows the Markdown text unchanged. az boards work-item create writes
// only /fields operations (azure-devops extension work_item.py), so it
// cannot set the Markdown format, and Azure DevOps stores a description
// created that way as HTML (ADR 0030 decision 14). Every character is
// escaped, so no Markdown text becomes markup; each line is one block and a
// blank line keeps its height; leading spaces become non-breaking spaces so
// indentation stays visible. htmlText reads it back as the Markdown text.
func extensionDescription(markdown string) string {
	var builder strings.Builder
	for _, line := range strings.Split(strings.TrimRight(markdown, "\n"), "\n") {
		if line == "" {
			builder.WriteString("<div><br></div>")
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		builder.WriteString("<div>" + strings.Repeat("&nbsp;", indent) + html.EscapeString(line[indent:]) + "</div>")
	}
	return builder.String()
}

// planRead reads one project resource for preflight: through az rest at
// path on the az rest route, or through the verified az devops invoke pair
// of the ADR 0030 inventory on the extension route.
func (c *Client) planRead(ctx context.Context, repository tracker.Repository, path string, query url.Values, read invokeRequest, destination any) error {
	if !c.extensionRoute() {
		return c.call(ctx, request{Method: "GET", URL: projectURL(repository, path, query, apiVersion)}, destination)
	}
	return c.invoke(ctx, repository.Owner, planInvokeRequest(repository, query, read), destination)
}

// planInvokeRequest completes read for the extension route: the extension
// API version, the project route when read names no route, and query as
// its query parameters. Callers pass the query only as query, never in
// read.
func planInvokeRequest(repository tracker.Repository, query url.Values, read invokeRequest) invokeRequest {
	read.APIVersion = extensionAPIVersion
	if read.Route == nil {
		read.Route = projectRoute(repository)
	}
	read.Query = queryParameters(query)
	return read
}

// queryParameters converts query to the single-valued parameters of az
// devops invoke, keeping the first value of each key. An empty query has
// no parameters.
func queryParameters(query url.Values) map[string]string {
	if len(query) == 0 {
		return nil
	}
	parameters := make(map[string]string, len(query))
	for key := range query {
		parameters[key] = query.Get(key)
	}
	return parameters
}

// linkType is the projection of one work-item link type.
type linkType struct {
	Name          string         `json:"name"`
	ReferenceName string         `json:"referenceName"`
	Attributes    map[string]any `json:"attributes"`
}

// linkTypeList reads the link types of the organization: through az rest,
// or with az boards work-item relation list-type on the extension route.
func (c *Client) linkTypeList(ctx context.Context, repository tracker.Repository) ([]linkType, error) {
	if c.extensionRoute() {
		var types []linkType
		err := c.extension(ctx, repository.Owner, []string{"boards", "work-item", "relation", "list-type"}, &types)
		return types, err
	}
	var response struct {
		Value []linkType `json:"value"`
	}
	err := c.call(ctx, request{Method: "GET", URL: organizationURL(repository.Owner, "wit/workitemrelationtypes", nil, apiVersion)}, &response)
	return response.Value, err
}

// linkTypeName returns the friendly name that az boards work-item relation
// add and show use for the link type reference, for example "Parent" for
// System.LinkTypes.Hierarchy-Reverse. The extension matches names
// case-insensitively and takes the first match (relations.py), so a name
// that two link types share is refused.
func linkTypeName(types []linkType, reference string) (string, error) {
	name := ""
	for _, value := range types {
		if value.ReferenceName == reference {
			name = value.Name
		}
	}
	if name == "" {
		return "", tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "link type %s is missing in the organization", reference)
	}
	count := 0
	for _, value := range types {
		if strings.EqualFold(value.Name, name) {
			count++
		}
	}
	if count != 1 {
		return "", tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability,
			"link type name %q of %s is not unique in the organization, so az boards work-item relation add cannot select it", name, reference)
	}
	return name, nil
}
