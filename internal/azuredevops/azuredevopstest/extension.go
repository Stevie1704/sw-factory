package azuredevopstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ExtensionCall is one recorded command of the azure-devops CLI extension
// route (ADR 0030): az boards, az repos, or az devops invoke.
type ExtensionCall struct {
	// Command is the command path before the first flag, for example
	// ["repos", "show"] or ["devops", "invoke"].
	Command []string
	// Flags maps each flag to its values in order. A flag without a value
	// maps to an empty list.
	Flags map[string][]string
	// Body is the content of the --in-file request body of az devops
	// invoke, read while the call runs, or "" without one.
	Body string
}

// Flag returns the single value of a flag, or "" when it is absent.
func (c ExtensionCall) Flag(name string) string {
	values := c.Flags[name]
	if len(values) == 0 {
		return ""
	}
	return strings.Join(values, " ")
}

// Route returns the routing key of the call: the command path, and for az
// devops invoke the method, area, and resource, for example "repos show" or
// "devops invoke GET git/repositories".
func (c ExtensionCall) Route() string {
	route := strings.Join(c.Command, " ")
	if route != "devops invoke" {
		return route
	}
	method := c.Flag("--http-method")
	if method == "" {
		method = "GET"
	}
	return route + " " + strings.ToUpper(method) + " " + c.Flag("--area") + "/" + c.Flag("--resource")
}

// Parameters returns the key=value pairs of a multi-value flag such as
// --route-parameters, or nil when a value has no '='.
func (c ExtensionCall) Parameters(name string) map[string]string {
	parameters := map[string]string{}
	for _, value := range c.Flags[name] {
		key, parameter, ok := strings.Cut(value, "=")
		if !ok {
			return nil
		}
		parameters[key] = parameter
	}
	return parameters
}

// extensionGroups are the command groups the extension route may run.
var extensionGroups = map[string]bool{"boards": true, "repos": true, "devops": true, "factory-devops": true}

// ParseExtensionCall parses one extension-route command line and reads the
// --in-file body. It refuses az rest, an unknown command group, and any
// call without explicit organization targeting, disabled organization
// detection, and JSON output, so a test fails when the adapter would rely on
// ambient CLI defaults.
func ParseExtensionCall(args []string) (ExtensionCall, error) {
	if len(args) == 0 || !extensionGroups[args[0]] {
		return ExtensionCall{}, fmt.Errorf("fake az extension route does not accept %q", strings.Join(firstToken(args), " "))
	}
	call := ExtensionCall{Flags: map[string][]string{}}
	flag := ""
	bound := false
	for _, arg := range args {
		if err := refuseFileReference(arg); err != nil {
			return ExtensionCall{}, err
		}
		switch {
		case strings.HasPrefix(arg, "--"):
			name, value, inline := strings.Cut(arg, "=")
			flag = name
			if _, seen := call.Flags[flag]; seen {
				return ExtensionCall{}, fmt.Errorf("fake az: flag %s repeats", flag)
			}
			call.Flags[flag] = []string{}
			if inline {
				// --flag=value binds exactly one value, as argparse does.
				call.Flags[flag] = []string{value}
			}
			bound = inline
		case bound:
			return ExtensionCall{}, fmt.Errorf("fake az: value %q follows a --flag=value argument", arg)
		case flag == "":
			call.Command = append(call.Command, arg)
		default:
			call.Flags[flag] = append(call.Flags[flag], arg)
		}
	}
	if !strings.HasPrefix(call.Flag("--organization"), "https://dev.azure.com/") {
		return ExtensionCall{}, fmt.Errorf("fake az: %s has no explicit --organization URL", strings.Join(call.Command, " "))
	}
	if call.Flag("--detect") != "false" || call.Flag("--output") != "json" {
		return ExtensionCall{}, fmt.Errorf("fake az: %s must pass --detect false and --output json", strings.Join(call.Command, " "))
	}
	if path := call.Flag("--in-file"); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return ExtensionCall{}, fmt.Errorf("fake az: read --in-file: %w", err)
		}
		call.Body = string(body)
	}
	return call, nil
}

// refuseFileReference refuses an argument that Azure CLI would replace by a
// file's content: one whose value, after the first '=' when there is one,
// starts with '@' (azure-cli core _expand_file_prefixed_files).
func refuseFileReference(arg string) error {
	value := arg
	if _, after, ok := strings.Cut(arg, "="); ok {
		value = after
	}
	if strings.HasPrefix(value, "@") {
		return fmt.Errorf("fake az: argument %q would be read as a file by Azure CLI", arg)
	}
	return nil
}

// firstToken returns the first argument, or a placeholder for an empty
// command line.
func firstToken(args []string) []string {
	if len(args) == 0 {
		return []string{"<empty>"}
	}
	return args[:1]
}

// IsExtensionCall reports whether args are an extension-route command line
// rather than az rest.
func IsExtensionCall(args []string) bool {
	return len(args) > 0 && args[0] != "rest"
}

// errNoExtensionRoute reports an extension command the scripted CLI does not
// answer.
func errNoExtensionRoute(call ExtensionCall) error {
	return errors.New("fake az: no route for " + call.Route())
}

// Stable identities of the scripted project and its two Git repositories.
// Two repositories share the project, so tests can prove that each
// registration claims only the PBIs assigned to its own repository.
const (
	// ProjectID is the scripted project GUID.
	ProjectID = "6f9b0c5e-2a41-4d8e-9a43-1c2f5e7d8a90"
	// RepositoryID is the GUID of the registered repository Repository.
	RepositoryID = "3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b"
	// WebRepository is the second Git repository of the scripted project.
	WebRepository = "web"
	// WebRepositoryID is the GUID of WebRepository.
	WebRepositoryID = "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
)

// repositoryIDs maps each scripted repository name to its GUID.
var repositoryIDs = map[string]string{Repository: RepositoryID, WebRepository: WebRepositoryID}

// runExtension records and answers one extension-route command. The caller
// holds b.mu. FailOnce with a route key, for example "repos show", fails the
// next matching command once.
func (b *Boards) runExtension(ctx context.Context, args []string) ([]byte, error) {
	call, err := ParseExtensionCall(args)
	if err != nil {
		b.t.Errorf("scripted az: %v", err)
		return nil, err
	}
	b.extensionCalls = append(b.extensionCalls, call)
	if fault, ok := b.faults[call.Route()]; ok {
		delete(b.faults, call.Route())
		return nil, errors.New(fault.message)
	}
	response, err := b.answerExtension(ctx, call)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		b.t.Errorf("encode scripted response: %v", err)
	}
	return encoded, nil
}

// answerExtension returns the scripted response of one extension command.
// Project-scoped invoke routes must name the scripted project.
func (b *Boards) answerExtension(ctx context.Context, call ExtensionCall) (any, error) {
	route := call.Parameters("--route-parameters")
	if call.Command[0] == "devops" && route["project"] != Project {
		return nil, fmt.Errorf("ERROR: project %q of %s is not the scripted project", route["project"], call.Route())
	}
	if response, ok, err := b.answerRepos(call); ok {
		return response, err
	}
	switch call.Route() {
	case "factory-devops identity":
		return map[string]string{"login": FactoryIdentity}, nil
	case "factory-devops permission":
		return map[string]bool{"granted": true}, nil
	case "repos show":
		return RepositoryResponse(call)
	case "repos list":
		return RepositoryListResponse(call)
	case "devops invoke GET wit/workItemTypes":
		types := []map[string]any{}
		for _, value := range b.process {
			types = append(types, map[string]any{"name": value.Name, "isDisabled": value.Disabled})
		}
		return map[string]any{"count": len(types), "value": types}, nil
	case "devops invoke GET wit/classificationNodes":
		return map[string]string{"identifier": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}, nil
	case "devops invoke GET work/backlogconfiguration":
		return b.backlogConfiguration(), nil
	case "devops invoke GET wit/fields":
		return b.fieldList(), nil
	case "devops invoke GET wit/workItemTypesField":
		if call.Parameters("--query-parameters")["$expand"] != "allowedValues" {
			b.t.Errorf("%s does not expand allowed values", call.Route())
		}
		return b.typeResource(route["type"] + "/fields")
	case "devops invoke GET wit/workItemTypeStates":
		return b.typeResource(route["type"] + "/states")
	case "boards work-item relation list-type":
		return relationTypes(), nil
	case "boards work-item create":
		return b.extensionCreate(ctx, call)
	case "boards query":
		return b.extensionQuery(call)
	case "devops invoke POST wit/workItemsBatch":
		return b.extensionBatch(call)
	case "boards work-item relation add":
		return b.extensionLink(call)
	case "devops invoke GET wit/updates":
		return b.extensionUpdates(call)
	case "devops invoke GET wit/comments", "devops invoke POST wit/comments", "devops invoke PATCH wit/comments":
		return b.extensionComments(call)
	case "boards work-item update":
		return b.extensionUpdateTags(call)
	case "boards work-item relation show":
		id, _ := strconv.Atoi(call.Flag("--id"))
		return b.friendlyItem(id)
	}
	return nil, errNoExtensionRoute(call)
}

// relationTypes returns the work link types of the scripted organization,
// with the names and reference names of the Microsoft link type reference.
func relationTypes() []map[string]any {
	types := []map[string]any{}
	for _, pair := range [][2]string{
		{"Parent", "System.LinkTypes.Hierarchy-Reverse"}, {"Child", "System.LinkTypes.Hierarchy-Forward"},
		{"Predecessor", "System.LinkTypes.Dependency-Reverse"}, {"Successor", "System.LinkTypes.Dependency-Forward"},
		{"Related", "System.LinkTypes.Related"},
	} {
		types = append(types, map[string]any{"name": pair[0], "referenceName": pair[1], "attributes": map[string]any{"enabled": true, "usage": "workItemLink"}})
	}
	return types
}

// extensionCreate answers az boards work-item create. Like the extension
// (work_item.py), it writes only /fields operations, so every multiline
// field keeps the HTML default format.
func (b *Boards) extensionCreate(ctx context.Context, call ExtensionCall) (any, error) {
	if call.Flag("--project") != Project {
		return nil, fmt.Errorf("ERROR: create in project %q, want the scripted project", call.Flag("--project"))
	}
	fields := map[string]any{"System.Title": call.Flag("--title")}
	if _, ok := call.Flags["--description"]; ok {
		if len(call.Flags["--description"]) != 1 {
			b.t.Errorf("--description has %d values, want one", len(call.Flags["--description"]))
		}
		fields["System.Description"] = call.Flag("--description")
	}
	for _, pair := range call.Flags["--fields"] {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, errors.New(`ERROR: The --fields argument should consist of space separated "field=value" pairs.`)
		}
		fields[name] = value
	}
	title := call.Flag("--title")
	fault, faulted := b.faults["create:"+title]
	if faulted {
		delete(b.faults, "create:"+title)
		if !fault.afterWrite {
			return nil, errors.New(fault.message)
		}
	}
	b.add(call.Flag("--type"), fields, nil)
	if faulted {
		return nil, errors.New(fault.message)
	}
	if b.held["create:"+title] {
		delete(b.held, "create:"+title)
		b.mu.Unlock()
		<-ctx.Done()
		b.mu.Lock()
		return nil, errors.Join(errors.New("az boards work-item create: the request was interrupted"), ctx.Err())
	}
	return b.itemJSON(b.items[len(b.items)-1]), nil
}

// wiqlProject matches the explicit project condition of an extension WIQL.
var wiqlProject = regexp.MustCompile(`\[System\.TeamProject\] = '([^']*)'`)

// wiqlCursor is the exclusive lower ID bound used to page capped CLI queries.
var wiqlCursor = regexp.MustCompile(`\[System\.Id\] > (\d+)`)

// extensionQuery answers az boards query: every item whose tags contain the
// text of every CONTAINS condition, as Azure DevOps matches substrings.
func (b *Boards) extensionQuery(call ExtensionCall) (any, error) {
	if fault, ok := b.faults["wiql"]; ok {
		delete(b.faults, "wiql")
		return nil, errors.New(fault.message)
	}
	wiql := call.Flag("--wiql")
	if match := wiqlProject.FindStringSubmatch(wiql); match == nil || match[1] != Project || call.Flag("--project") != Project {
		b.t.Errorf("wiql %q does not name the scripted project explicitly", wiql)
	}
	conditions := wiqlTag.FindAllStringSubmatch(wiql, -1)
	cursor := 0
	if match := wiqlCursor.FindStringSubmatch(wiql); match != nil {
		cursor, _ = strconv.Atoi(match[1])
	}
	found := []map[string]any{}
	for _, item := range b.items {
		matches := len(conditions) > 0 && item.ID > cursor
		for _, condition := range conditions {
			matches = matches && slices.ContainsFunc(item.Tags(), func(tag string) bool {
				return strings.Contains(strings.ToLower(tag), strings.ToLower(condition[1]))
			})
		}
		if matches {
			found = append(found, map[string]any{"id": item.ID, "fields": map[string]any{"System.Id": item.ID, "System.Tags": strings.Join(item.Tags(), "; ")}})
		}
	}
	slices.SortFunc(found, func(a, b map[string]any) int { return a["id"].(int) - b["id"].(int) })
	return found[:min(1000, len(found))], nil
}

// extensionBatch answers the workItemsBatch read of the scripted project.
func (b *Boards) extensionBatch(call ExtensionCall) (any, error) {
	var body struct {
		IDs []int `json:"ids"`
	}
	if err := json.Unmarshal([]byte(call.Body), &body); err != nil {
		b.t.Errorf("decode workItemsBatch body %q: %v", call.Body, err)
	}
	values := []map[string]any{}
	for _, id := range body.IDs {
		for _, item := range b.items {
			if item.ID == id {
				values = append(values, b.itemJSON(item))
			}
		}
	}
	return map[string]any{"count": len(values), "value": values}, nil
}

// itemJSON renders one item as Azure DevOps returns it: its fields with the
// project and type, and the multiline formats that differ from HTML.
func (b *Boards) itemJSON(item Item) map[string]any {
	// Azure DevOps fills the project, the type, and the type's initial
	// state on create; the stored item keeps only what the writer sent.
	fields := map[string]any{"System.TeamProject": Project, "System.WorkItemType": item.Type, "System.State": "New"}
	for name, value := range item.Fields {
		fields[name] = value
	}
	response := map[string]any{"id": item.ID, "fields": fields}
	if len(item.Formats) > 0 {
		response["multilineFieldsFormat"] = item.Formats
	}
	return response
}

// relationURL is the API URL of a work item that a relation names.
func relationURL(id int) string {
	return "https://dev.azure.com/" + Organization + "/_apis/wit/workItems/" + strconv.Itoa(id)
}

// extensionLink answers az boards work-item relation add: it maps the
// friendly relation name to its reference name, adds the relation on --id,
// and returns the item with friendly relation names (relations.py).
func (b *Boards) extensionLink(call ExtensionCall) (any, error) {
	id, _ := strconv.Atoi(call.Flag("--id"))
	target, _ := strconv.Atoi(call.Flag("--target-id"))
	reference := ""
	for _, value := range relationTypes() {
		if strings.EqualFold(value["name"].(string), call.Flag("--relation-type")) {
			reference = value["referenceName"].(string)
		}
	}
	if reference == "" {
		return nil, errors.New(`ERROR: --relation-type is not valid. Use "az boards work-item relation list-type" command to list possible relation types in your project`)
	}
	if !slices.ContainsFunc(b.items, func(item Item) bool { return item.ID == target }) {
		return nil, errors.New("ERROR: Id(s) supplied in --target-id is not valid")
	}
	fault, faulted := b.faults["link:"+strconv.Itoa(id)]
	if faulted {
		delete(b.faults, "link:"+strconv.Itoa(id))
		if !fault.afterWrite {
			return nil, errors.New(fault.message)
		}
	}
	for index := range b.items {
		if b.items[index].ID == id {
			b.items[index].Relations = append(b.items[index].Relations, Relation{Rel: reference, URL: relationURL(target)})
			if faulted {
				return nil, errors.New(fault.message)
			}
			return b.friendlyItem(id)
		}
	}
	return nil, errors.New("ERROR: TF401232: Work item " + strconv.Itoa(id) + " does not exist")
}

// friendlyItem answers az boards work-item relation show: the item with
// each relation's reference name replaced by its friendly name.
func (b *Boards) friendlyItem(id int) (any, error) {
	for _, item := range b.items {
		if item.ID != id {
			continue
		}
		relations := []Relation{}
		for _, relation := range item.Relations {
			for _, value := range relationTypes() {
				if value["referenceName"] == relation.Rel {
					relation.Rel = value["name"].(string)
				}
			}
			relations = append(relations, relation)
		}
		response := b.itemJSON(item)
		response["relations"] = relations
		return response, nil
	}
	return nil, errors.New("ERROR: TF401232: Work item " + strconv.Itoa(id) + " does not exist")
}

// ExtensionWrites returns every recorded extension command that can change
// a work item: creates, relation adds, and updates.
func (b *Boards) ExtensionWrites() []ExtensionCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	writes := []ExtensionCall{}
	for _, call := range b.extensionCalls {
		switch call.Route() {
		case "boards work-item create", "boards work-item relation add", "boards work-item update", "devops invoke PATCH wit/workItems", "devops invoke POST wit/workItems":
			writes = append(writes, call)
		}
	}
	return writes
}

// RepositoryResponse answers az repos show for a scripted repository of the
// scripted project, by name or GUID, and fails for a missing one with the
// Azure DevOps error text.
func RepositoryResponse(call ExtensionCall) (any, error) {
	if !strings.EqualFold(call.Flag("--project"), Project) && call.Flag("--project") != ProjectID {
		return nil, errors.New("ERROR: TF200016: The following project does not exist: " + call.Flag("--project"))
	}
	wanted := call.Flag("--repository")
	for name, id := range repositoryIDs {
		if strings.EqualFold(wanted, name) || wanted == id {
			return map[string]any{
				"id":            id,
				"name":          name,
				"defaultBranch": "refs/heads/main",
				"project":       map[string]string{"id": ProjectID, "name": Project},
			}, nil
		}
	}
	return nil, errors.New("ERROR: TF401019: The Git repository with name or identifier " + wanted + " does not exist or you do not have permissions for the operation you are attempting.")
}

// RepositoryListResponse answers az repos list for the scripted project
// with a JSON array of every scripted repository, ordered by name, and
// fails for another project with the Azure DevOps error text.
func RepositoryListResponse(call ExtensionCall) (any, error) {
	if !strings.EqualFold(call.Flag("--project"), Project) && call.Flag("--project") != ProjectID {
		return nil, errors.New("ERROR: TF200016: The following project does not exist: " + call.Flag("--project"))
	}
	names := make([]string, 0, len(repositoryIDs))
	for name := range repositoryIDs {
		names = append(names, name)
	}
	slices.Sort(names)
	repositories := []map[string]any{}
	for _, name := range names {
		repositories = append(repositories, map[string]any{
			"id":            repositoryIDs[name],
			"name":          name,
			"defaultBranch": "refs/heads/main",
			"project":       map[string]string{"id": ProjectID, "name": Project},
		})
	}
	return repositories, nil
}
