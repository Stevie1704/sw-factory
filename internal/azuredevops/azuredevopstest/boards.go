// Package azuredevopstest holds a stateful scripted az CLI for one Azure
// Boards project. Tests use it to drive the real Azure DevOps planning
// adapter end to end without a network or an Azure DevOps organization: the
// adapter tests in internal/azuredevops and the integrated acceptance
// scenarios that publish a reviewed plan.
package azuredevopstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Field is one field of a work-item type in the scripted process.
type Field struct {
	ReferenceName  string   `json:"referenceName"`
	Name           string   `json:"name"`
	AlwaysRequired bool     `json:"alwaysRequired"`
	DefaultValue   any      `json:"defaultValue"`
	AllowedValues  []string `json:"allowedValues"`
}

// State is one state of a work-item type in the scripted process.
type State struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

// WorkItemType is one work-item type of the scripted process. Rank is its
// backlog level (task 0, requirement 1, portfolio 2 and up), or -1 when the
// type is on no backlog.
type WorkItemType struct {
	Name     string
	Rank     int
	Disabled bool
	Fields   []Field
	States   []State
}

// StandardFields are the fields every scripted type has.
func StandardFields() []Field {
	return []Field{
		{ReferenceName: "System.Title", Name: "Title", AlwaysRequired: true},
		{ReferenceName: "System.State", Name: "State", AlwaysRequired: true, DefaultValue: "New"},
		{ReferenceName: "System.Description", Name: "Description"},
		{ReferenceName: "System.Tags", Name: "Tags"},
		{ReferenceName: "System.AreaPath", Name: "Area Path", AlwaysRequired: true},
		{ReferenceName: "Microsoft.VSTS.Common.Priority", Name: "Priority", DefaultValue: 2, AllowedValues: []string{"1", "2", "3", "4"}},
	}
}

// StandardStates are the states every scripted type has.
func StandardStates() []State {
	return []State{{"New", "Proposed"}, {"Active", "InProgress"}, {"Closed", "Completed"}, {"Removed", "Removed"}}
}

// AgileProcess returns the scripted Agile process: Epic, Feature, User Story,
// and Task.
func AgileProcess() []WorkItemType {
	return []WorkItemType{
		{Name: "Epic", Rank: 3, Fields: StandardFields(), States: StandardStates()},
		{Name: "Feature", Rank: 2, Fields: StandardFields(), States: StandardStates()},
		{Name: "User Story", Rank: 1, Fields: StandardFields(), States: StandardStates()},
		{Name: "Task", Rank: 0, Fields: StandardFields(), States: StandardStates()},
		{Name: "Issue", Rank: -1, Fields: StandardFields(), States: StandardStates()},
	}
}

// ScrumProcess returns the scripted Scrum process with Product Backlog Item
// in place of User Story.
func ScrumProcess() []WorkItemType {
	return []WorkItemType{
		{Name: "Epic", Rank: 3, Fields: StandardFields(), States: StandardStates()},
		{Name: "Feature", Rank: 2, Fields: StandardFields(), States: StandardStates()},
		{Name: "Product Backlog Item", Rank: 1, Fields: StandardFields(), States: StandardStates()},
		{Name: "Task", Rank: 0, Fields: StandardFields(), States: StandardStates()},
	}
}

// Relation is one relation of a scripted work item.
type Relation struct {
	Rel string `json:"rel"`
	URL string `json:"url"`
}

// Item is one work item of the scripted project.
type Item struct {
	ID        int
	Type      string
	Fields    map[string]any
	Formats   map[string]string
	Relations []Relation
}

// Tags returns the tags of the item.
func (i Item) Tags() []string {
	value, _ := i.Fields["System.Tags"].(string)
	tags := []string{}
	for _, tag := range strings.Split(value, ";") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// fault fails the next write or read with a key once.
type fault struct {
	message    string
	afterWrite bool
}

// Boards is a stateful scripted az CLI for one Azure Boards project.
// It answers the az rest calls of the planning adapter from an in-memory
// process and work-item store, records every call, and fails a selected
// call once, optionally after applying a write, as a lost response does. An
// unexpected route fails the call with a "no route" error.
type Boards struct {
	t       testing.TB
	mu      sync.Mutex
	process []WorkItemType
	// denied lists the permission bits the identity lacks, by
	// "<namespace>/<bit>".
	denied map[string]bool
	items  []Item
	faults map[string]fault
	held   map[string]bool
	calls  []Call
}

// NewBoards returns an empty project with the given process and
// every permission granted.
func NewBoards(t testing.TB, process []WorkItemType) *Boards {
	return &Boards{t: t, process: process, denied: map[string]bool{}, faults: map[string]fault{}, held: map[string]bool{}}
}

// FailOnce fails the next call with key once: "create:<title>",
// "link:<work item id>", "wiql", or "GET <path>" for a read.
func (b *Boards) FailOnce(key, message string, afterWrite bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults[key] = fault{message: message, afterWrite: afterWrite}
}

// HoldAfterWrite applies the next create of the work item titled title and
// then holds its response until the caller's context ends, as a process that
// stops while the request is in flight. The item stays created; the caller
// gets the context error.
func (b *Boards) HoldAfterWrite(title string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held["create:"+title] = true
}

// Deny removes one permission bit of the identity.
func (b *Boards) Deny(namespace string, bit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.denied[namespace+"/"+strconv.Itoa(bit)] = true
}

// Seed adds a work item without a call, for example one that another writer
// created.
func (b *Boards) Seed(workItemType, title string, tags ...string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.add(workItemType, map[string]any{"System.Title": title, "System.Tags": strings.Join(tags, "; ")}, nil)
}

// Snapshot returns a copy of every work item.
func (b *Boards) Snapshot() []Item {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.items)
}

// Writes returns every recorded call that is not a read.
func (b *Boards) Writes() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	writes := []Call{}
	for _, call := range b.calls {
		if call.Method == "PATCH" || (call.Method == "POST" && !strings.HasSuffix(call.URL.Path, "/wit/wiql")) {
			writes = append(writes, call)
		}
	}
	return writes
}

// Creates returns the number of work item create requests.
func (b *Boards) Creates() int {
	count := 0
	for _, call := range b.Writes() {
		if call.Method == "POST" {
			count++
		}
	}
	return count
}

// add stores one work item under the next id.
func (b *Boards) add(workItemType string, fields map[string]any, formats map[string]string) int {
	id := 100 + len(b.items) + 1
	b.items = append(b.items, Item{ID: id, Type: workItemType, Fields: fields, Formats: formats})
	return id
}

// Calls returns every recorded call in order.
func (b *Boards) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// Run answers one az rest invocation.
func (b *Boards) Run(ctx context.Context, args []string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	call, err := ParseCall(args)
	if err != nil {
		b.t.Errorf("scripted az: %v", err)
		return nil, err
	}
	b.calls = append(b.calls, call)
	if fault, ok := b.faults["GET "+call.URL.Path]; ok && call.Method == "GET" {
		delete(b.faults, "GET "+call.URL.Path)
		return nil, errors.New(fault.message)
	}
	response, err := b.answer(ctx, call)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		b.t.Errorf("encode scripted response: %v", err)
	}
	return encoded, nil
}

// Identity of the scripted organization, project, and repository.
const (
	// Organization is the scripted organization.
	Organization = "contoso"
	// Project is the scripted project.
	Project = "Factory Pilot"
	// Repository is the scripted Git repository.
	Repository = "service"
)

// Paths of the scripted organization and project.
const (
	organizationPath = "/" + Organization + "/_apis"
	// ProjectPath is the REST path prefix of the scripted project.
	ProjectPath = "/" + Organization + "/" + Project + "/_apis"
	// RepositoryPath is the REST path of the scripted Git repository.
	RepositoryPath = ProjectPath + "/git/repositories/" + Repository
	// WorkItemsPath is the REST path of the project's work items.
	WorkItemsPath = ProjectPath + "/wit/workitems"
)

// permissionRoute matches a permission evaluation.
var permissionRoute = regexp.MustCompile(`^/contoso/_apis/permissions/([0-9a-f-]+)/(\d+)$`)

// answer returns the scripted response of one call.
func (b *Boards) answer(ctx context.Context, call Call) (any, error) {
	path := call.URL.Path
	switch {
	case call.Method == "GET" && path == RepositoryPath:
		return map[string]any{"id": "repo-id", "project": map[string]string{"id": "project-id"}}, nil
	case call.Method == "GET" && path == ProjectPath+"/wit/classificationnodes/Areas":
		return map[string]string{"identifier": "area-id"}, nil
	case call.Method == "GET" && permissionRoute.MatchString(path):
		match := permissionRoute.FindStringSubmatch(path)
		return map[string][]bool{"value": {!b.denied[match[1]+"/"+match[2]]}}, nil
	case call.Method == "GET" && path == ProjectPath+"/wit/workitemtypes":
		types := []map[string]any{}
		for _, value := range b.process {
			types = append(types, map[string]any{"name": value.Name, "isDisabled": value.Disabled})
		}
		return map[string]any{"count": len(types), "value": types}, nil
	case call.Method == "GET" && path == ProjectPath+"/work/backlogconfiguration":
		return b.backlogConfiguration(), nil
	case call.Method == "GET" && path == organizationPath+"/wit/workitemrelationtypes":
		return map[string]any{"value": []map[string]any{
			{"referenceName": "System.LinkTypes.Hierarchy-Reverse", "name": "Parent", "attributes": map[string]any{"enabled": true}},
			{"referenceName": "System.LinkTypes.Hierarchy-Forward", "name": "Child", "attributes": map[string]any{"enabled": true}},
			{"referenceName": "System.LinkTypes.Dependency-Reverse", "name": "Predecessor", "attributes": map[string]any{"enabled": true}},
			{"referenceName": "System.LinkTypes.Dependency-Forward", "name": "Successor", "attributes": map[string]any{"enabled": true}},
		}}, nil
	case call.Method == "GET" && strings.HasPrefix(path, ProjectPath+"/wit/workitemtypes/"):
		return b.typeResource(strings.TrimPrefix(path, ProjectPath+"/wit/workitemtypes/"))
	case call.Method == "POST" && strings.HasPrefix(path, WorkItemsPath+"/$"):
		return b.create(ctx, call, strings.TrimPrefix(path, WorkItemsPath+"/$"))
	case call.Method == "POST" && path == ProjectPath+"/wit/wiql":
		return b.query(call)
	case call.Method == "GET" && path == WorkItemsPath:
		return b.batch(call)
	case strings.HasPrefix(path, WorkItemsPath+"/"):
		id, err := strconv.Atoi(strings.TrimPrefix(path, WorkItemsPath+"/"))
		if err != nil {
			break
		}
		if call.Method == "PATCH" {
			return b.link(call, id)
		}
		if call.Method == "GET" {
			return b.read(id)
		}
	}
	return nil, errors.New("fake az: no route for " + call.Route())
}

// backlogConfiguration returns the backlog levels of the process.
func (b *Boards) backlogConfiguration() map[string]any {
	level := func(rank int) map[string]any {
		types := []map[string]string{}
		for _, value := range b.process {
			if value.Rank == rank {
				types = append(types, map[string]string{"name": value.Name})
			}
		}
		return map[string]any{"name": fmt.Sprintf("Level %d", rank), "rank": rank, "workItemTypes": types}
	}
	portfolio := []map[string]any{}
	for rank := 5; rank >= 2; rank-- {
		if len(level(rank)["workItemTypes"].([]map[string]string)) > 0 {
			portfolio = append(portfolio, level(rank))
		}
	}
	return map[string]any{"taskBacklog": level(0), "requirementBacklog": level(1), "portfolioBacklogs": portfolio}
}

// typeResource answers the fields or states of one work-item type.
func (b *Boards) typeResource(rest string) (any, error) {
	name, resource, _ := strings.Cut(rest, "/")
	for _, value := range b.process {
		if !strings.EqualFold(value.Name, name) {
			continue
		}
		switch resource {
		case "fields":
			return map[string]any{"count": len(value.Fields), "value": value.Fields}, nil
		case "states":
			return map[string]any{"count": len(value.States), "value": value.States}, nil
		}
	}
	return nil, errors.New("az rest: Not Found({\"message\":\"TF51535: Cannot find work item type " + name + "\"})")
}

// patchOperation is one JSON patch operation of a work item write.
type patchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// decodePatch decodes the JSON patch body of a write.
func (b *Boards) decodePatch(call Call) []patchOperation {
	if call.Headers != "Content-Type=application/json-patch+json" {
		b.t.Errorf("%s headers = %q, want a JSON patch", call.Route(), call.Headers)
	}
	var operations []patchOperation
	if err := json.Unmarshal([]byte(call.Body), &operations); err != nil {
		b.t.Errorf("decode patch %q: %v", call.Body, err)
	}
	return operations
}

// create answers a work item create.
func (b *Boards) create(ctx context.Context, call Call, workItemType string) (any, error) {
	fields := map[string]any{}
	formats := map[string]string{}
	for _, operation := range b.decodePatch(call) {
		switch {
		case operation.Op != "add":
			b.t.Errorf("create operation %#v is not add", operation)
		case strings.HasPrefix(operation.Path, "/fields/"):
			fields[strings.TrimPrefix(operation.Path, "/fields/")] = operation.Value
		case strings.HasPrefix(operation.Path, "/multilineFieldsFormat/"):
			formats[strings.TrimPrefix(operation.Path, "/multilineFieldsFormat/")] = fmt.Sprint(operation.Value)
		default:
			b.t.Errorf("create operation %#v is not a field", operation)
		}
	}
	title, _ := fields["System.Title"].(string)
	fault, faulted := b.faults["create:"+title]
	if faulted {
		delete(b.faults, "create:"+title)
		if !fault.afterWrite {
			return nil, errors.New(fault.message)
		}
	}
	id := b.add(workItemType, fields, formats)
	if faulted {
		return nil, errors.New(fault.message)
	}
	if b.held["create:"+title] {
		delete(b.held, "create:"+title)
		b.mu.Unlock()
		<-ctx.Done()
		b.mu.Lock()
		return nil, errors.Join(errors.New("az rest: the request was interrupted"), ctx.Err())
	}
	return map[string]any{"id": id, "fields": fields}, nil
}

// wiqlTag matches the tag condition of a correlation query.
var wiqlTag = regexp.MustCompile(`\[System\.Tags\] CONTAINS '([^']*)'`)

// query answers a WIQL query by tag.
func (b *Boards) query(call Call) (any, error) {
	if fault, ok := b.faults["wiql"]; ok {
		delete(b.faults, "wiql")
		return nil, errors.New(fault.message)
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(call.Body), &body); err != nil {
		b.t.Errorf("decode wiql %q: %v", call.Body, err)
	}
	match := wiqlTag.FindStringSubmatch(body.Query)
	if match == nil {
		b.t.Errorf("wiql %q has no tag condition", body.Query)
	}
	found := []map[string]int{}
	for _, item := range b.items {
		if slices.ContainsFunc(item.Tags(), func(tag string) bool { return strings.EqualFold(tag, match[1]) }) {
			found = append(found, map[string]int{"id": item.ID})
		}
	}
	return map[string]any{"workItems": found}, nil
}

// batch answers a batch read of work items.
func (b *Boards) batch(call Call) (any, error) {
	values := []map[string]any{}
	for _, raw := range strings.Split(call.URL.Query().Get("ids"), ",") {
		id, _ := strconv.Atoi(raw)
		for _, item := range b.items {
			if item.ID == id {
				values = append(values, map[string]any{"id": item.ID, "fields": item.Fields})
			}
		}
	}
	return map[string]any{"value": values}, nil
}

// read answers a read of one work item with its relations.
func (b *Boards) read(id int) (any, error) {
	for _, item := range b.items {
		if item.ID == id {
			return map[string]any{"id": item.ID, "fields": item.Fields, "relations": item.Relations}, nil
		}
	}
	return nil, errors.New("az rest: Not Found({\"message\":\"TF401232: Work item does not exist\"})")
}

// link answers a work item update that adds one relation.
func (b *Boards) link(call Call, id int) (any, error) {
	operations := b.decodePatch(call)
	if len(operations) != 1 || operations[0].Op != "add" || operations[0].Path != "/relations/-" {
		b.t.Errorf("update of work item %d = %#v, want one added relation", id, operations)
	}
	encoded, _ := json.Marshal(operations[0].Value)
	var relation Relation
	if err := json.Unmarshal(encoded, &relation); err != nil {
		b.t.Errorf("decode relation: %v", err)
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
			b.items[index].Relations = append(b.items[index].Relations, relation)
			if faulted {
				return nil, errors.New(fault.message)
			}
			return map[string]any{"id": id}, nil
		}
	}
	return nil, errors.New("az rest: Not Found({\"message\":\"TF401232: Work item does not exist\"})")
}

// Call is one recorded az rest invocation.
type Call struct {
	// Method is the upper-case HTTP method.
	Method string
	// URL is the parsed request address.
	URL *url.URL
	// Headers is the --headers argument.
	Headers string
	// Body is the --body argument.
	Body string
}

// Route returns "METHOD /path" for the call, with the path unescaped.
func (c Call) Route() string {
	return c.Method + " " + c.URL.Path
}

// azureDevOpsResource is the Entra resource of Azure DevOps that every az
// rest call must name.
const azureDevOpsResource = "499b84ac-1321-427f-aa17-267ca6975798"

// ParseCall parses one az rest command line. It refuses a call that is not
// az rest, names no Azure DevOps resource, or has no API version.
func ParseCall(args []string) (Call, error) {
	if len(args) == 0 || args[0] != "rest" {
		return Call{}, errors.New("fake az supports only az rest")
	}
	call := Call{}
	resource := ""
	for index := 1; index+1 < len(args); index += 2 {
		switch args[index] {
		case "--method":
			call.Method = strings.ToUpper(args[index+1])
		case "--url":
			parsed, err := url.Parse(args[index+1])
			if err != nil {
				return Call{}, fmt.Errorf("az rest --url %q: %w", args[index+1], err)
			}
			call.URL = parsed
		case "--headers":
			call.Headers = args[index+1]
		case "--body":
			call.Body = args[index+1]
		case "--resource":
			resource = args[index+1]
		}
	}
	if resource != azureDevOpsResource {
		return Call{}, fmt.Errorf("az rest --resource = %q, want the Azure DevOps resource", resource)
	}
	if call.URL == nil || call.URL.Query().Get("api-version") == "" {
		return Call{}, fmt.Errorf("az rest call %v has no api-version", args)
	}
	return call, nil
}

// Title returns the title of the work item.
func (i Item) Title() string {
	title, _ := i.Fields["System.Title"].(string)
	return title
}

// Description returns the description of the work item.
func (i Item) Description() string {
	description, _ := i.Fields["System.Description"].(string)
	return description
}

// ItemSet is a snapshot of work items that resolves relation targets.
type ItemSet []Item

// Items wraps a snapshot so relation targets resolve to titles.
func Items(snapshot []Item) ItemSet {
	return ItemSet(snapshot)
}

// Title returns the title of the work item that a relation URL names, or ""
// when the snapshot holds no such item.
func (s ItemSet) Title(relationURL string) string {
	for _, item := range s {
		if strings.HasSuffix(relationURL, "/workItems/"+strconv.Itoa(item.ID)) {
			return item.Title()
		}
	}
	return ""
}
