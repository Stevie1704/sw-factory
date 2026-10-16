package azuredevops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// boardsField is one field of a work-item type in the scripted process.
type boardsField struct {
	ReferenceName  string   `json:"referenceName"`
	Name           string   `json:"name"`
	AlwaysRequired bool     `json:"alwaysRequired"`
	DefaultValue   any      `json:"defaultValue"`
	AllowedValues  []string `json:"allowedValues"`
}

// boardsState is one state of a work-item type in the scripted process.
type boardsState struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

// boardsType is one work-item type of the scripted process. Rank is its
// backlog level (task 0, requirement 1, portfolio 2 and up), or -1 when the
// type is on no backlog.
type boardsType struct {
	Name     string
	Rank     int
	Disabled bool
	Fields   []boardsField
	States   []boardsState
}

// standardFields are the fields every scripted type has.
func standardFields() []boardsField {
	return []boardsField{
		{ReferenceName: "System.Title", Name: "Title", AlwaysRequired: true},
		{ReferenceName: "System.State", Name: "State", AlwaysRequired: true, DefaultValue: "New"},
		{ReferenceName: "System.Description", Name: "Description"},
		{ReferenceName: "System.Tags", Name: "Tags"},
		{ReferenceName: "System.AreaPath", Name: "Area Path", AlwaysRequired: true},
		{ReferenceName: "Microsoft.VSTS.Common.Priority", Name: "Priority", DefaultValue: 2, AllowedValues: []string{"1", "2", "3", "4"}},
	}
}

// standardStates are the states every scripted type has.
func standardStates() []boardsState {
	return []boardsState{{"New", "Proposed"}, {"Active", "InProgress"}, {"Closed", "Completed"}, {"Removed", "Removed"}}
}

// agileProcess returns the scripted Agile process: Epic, Feature, User Story,
// and Task.
func agileProcess() []boardsType {
	return []boardsType{
		{Name: "Epic", Rank: 3, Fields: standardFields(), States: standardStates()},
		{Name: "Feature", Rank: 2, Fields: standardFields(), States: standardStates()},
		{Name: "User Story", Rank: 1, Fields: standardFields(), States: standardStates()},
		{Name: "Task", Rank: 0, Fields: standardFields(), States: standardStates()},
		{Name: "Issue", Rank: -1, Fields: standardFields(), States: standardStates()},
	}
}

// scrumProcess returns the scripted Scrum process with Product Backlog Item
// in place of User Story.
func scrumProcess() []boardsType {
	return []boardsType{
		{Name: "Epic", Rank: 3, Fields: standardFields(), States: standardStates()},
		{Name: "Feature", Rank: 2, Fields: standardFields(), States: standardStates()},
		{Name: "Product Backlog Item", Rank: 1, Fields: standardFields(), States: standardStates()},
		{Name: "Task", Rank: 0, Fields: standardFields(), States: standardStates()},
	}
}

// boardsRelation is one relation of a scripted work item.
type boardsRelation struct {
	Rel string `json:"rel"`
	URL string `json:"url"`
}

// boardsItem is one work item of the scripted project.
type boardsItem struct {
	ID        int
	Type      string
	Fields    map[string]any
	Formats   map[string]string
	Relations []boardsRelation
}

// tags returns the tags of the item.
func (i boardsItem) tags() []string {
	value, _ := i.Fields["System.Tags"].(string)
	tags := []string{}
	for _, tag := range strings.Split(value, ";") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// boardsFault fails the next write or read with a key once.
type boardsFault struct {
	message    string
	afterWrite bool
}

// scriptedBoards is a stateful scripted az CLI for one Azure Boards project.
// It answers the az rest calls of the planning adapter from an in-memory
// process and work-item store, records every call, and fails a selected
// call once, optionally after applying a write, as a lost response does. An
// unexpected route fails the call with a "no route" error.
type scriptedBoards struct {
	t       *testing.T
	mu      sync.Mutex
	process []boardsType
	// denied lists the permission bits the identity lacks, by
	// "<namespace>/<bit>".
	denied map[string]bool
	items  []boardsItem
	faults map[string]boardsFault
	calls  []azCall
}

// newScriptedBoards returns an empty project with the given process and
// every permission granted.
func newScriptedBoards(t *testing.T, process []boardsType) *scriptedBoards {
	return &scriptedBoards{t: t, process: process, denied: map[string]bool{}, faults: map[string]boardsFault{}}
}

// failOnce fails the next call with key once: "create:<title>",
// "link:<work item id>", "wiql", or "GET <path>" for a read.
func (b *scriptedBoards) failOnce(key, message string, afterWrite bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults[key] = boardsFault{message: message, afterWrite: afterWrite}
}

// deny removes one permission bit of the identity.
func (b *scriptedBoards) deny(namespace string, bit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.denied[namespace+"/"+strconv.Itoa(bit)] = true
}

// seed adds a work item without a call, for example one that another writer
// created.
func (b *scriptedBoards) seed(workItemType, title string, tags ...string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.add(workItemType, map[string]any{"System.Title": title, "System.Tags": strings.Join(tags, "; ")}, nil)
}

// snapshot returns a copy of every work item.
func (b *scriptedBoards) snapshot() []boardsItem {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.items)
}

// writes returns every recorded call that is not a read.
func (b *scriptedBoards) writes() []azCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	writes := []azCall{}
	for _, call := range b.calls {
		if call.method == "PATCH" || (call.method == "POST" && !strings.HasSuffix(call.url.Path, "/wit/wiql")) {
			writes = append(writes, call)
		}
	}
	return writes
}

// creates returns the number of work item create requests.
func (b *scriptedBoards) creates() int {
	count := 0
	for _, call := range b.writes() {
		if call.method == "POST" {
			count++
		}
	}
	return count
}

// add stores one work item under the next id.
func (b *scriptedBoards) add(workItemType string, fields map[string]any, formats map[string]string) int {
	id := 100 + len(b.items) + 1
	b.items = append(b.items, boardsItem{ID: id, Type: workItemType, Fields: fields, Formats: formats})
	return id
}

// Run answers one az rest invocation.
func (b *scriptedBoards) Run(_ context.Context, args []string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	call, err := parseAzCall(b.t, args)
	if err != nil {
		return nil, err
	}
	b.calls = append(b.calls, call)
	if fault, ok := b.faults["GET "+call.url.Path]; ok && call.method == "GET" {
		delete(b.faults, "GET "+call.url.Path)
		return nil, errors.New(fault.message)
	}
	response, err := b.answer(call)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		b.t.Fatalf("encode scripted response: %v", err)
	}
	return encoded, nil
}

// Paths of the scripted organization and project.
const (
	organizationPath = "/contoso/_apis"
	workItemsPath    = projectPath + "/wit/workitems"
)

// permissionRoute matches a permission evaluation.
var permissionRoute = regexp.MustCompile(`^/contoso/_apis/permissions/([0-9a-f-]+)/(\d+)$`)

// answer returns the scripted response of one call.
func (b *scriptedBoards) answer(call azCall) (any, error) {
	path := call.url.Path
	switch {
	case call.method == "GET" && path == repositoryPath:
		return map[string]any{"id": "repo-id", "project": map[string]string{"id": "project-id"}}, nil
	case call.method == "GET" && path == projectPath+"/wit/classificationnodes/Areas":
		return map[string]string{"identifier": "area-id"}, nil
	case call.method == "GET" && permissionRoute.MatchString(path):
		match := permissionRoute.FindStringSubmatch(path)
		return map[string][]bool{"value": {!b.denied[match[1]+"/"+match[2]]}}, nil
	case call.method == "GET" && path == projectPath+"/wit/workitemtypes":
		types := []map[string]any{}
		for _, value := range b.process {
			types = append(types, map[string]any{"name": value.Name, "isDisabled": value.Disabled})
		}
		return map[string]any{"count": len(types), "value": types}, nil
	case call.method == "GET" && path == projectPath+"/work/backlogconfiguration":
		return b.backlogConfiguration(), nil
	case call.method == "GET" && path == organizationPath+"/wit/workitemrelationtypes":
		return map[string]any{"value": []map[string]any{
			{"referenceName": "System.LinkTypes.Hierarchy-Reverse", "name": "Parent", "attributes": map[string]any{"enabled": true}},
			{"referenceName": "System.LinkTypes.Hierarchy-Forward", "name": "Child", "attributes": map[string]any{"enabled": true}},
			{"referenceName": "System.LinkTypes.Dependency-Reverse", "name": "Predecessor", "attributes": map[string]any{"enabled": true}},
			{"referenceName": "System.LinkTypes.Dependency-Forward", "name": "Successor", "attributes": map[string]any{"enabled": true}},
		}}, nil
	case call.method == "GET" && strings.HasPrefix(path, projectPath+"/wit/workitemtypes/"):
		return b.typeResource(strings.TrimPrefix(path, projectPath+"/wit/workitemtypes/"))
	case call.method == "POST" && strings.HasPrefix(path, workItemsPath+"/$"):
		return b.create(call, strings.TrimPrefix(path, workItemsPath+"/$"))
	case call.method == "POST" && path == projectPath+"/wit/wiql":
		return b.query(call)
	case call.method == "GET" && path == workItemsPath:
		return b.batch(call)
	case strings.HasPrefix(path, workItemsPath+"/"):
		id, err := strconv.Atoi(strings.TrimPrefix(path, workItemsPath+"/"))
		if err != nil {
			break
		}
		if call.method == "PATCH" {
			return b.link(call, id)
		}
		if call.method == "GET" {
			return b.read(id)
		}
	}
	return nil, errors.New("fake az: no route for " + call.route())
}

// backlogConfiguration returns the backlog levels of the process.
func (b *scriptedBoards) backlogConfiguration() map[string]any {
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
func (b *scriptedBoards) typeResource(rest string) (any, error) {
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
func (b *scriptedBoards) decodePatch(call azCall) []patchOperation {
	if call.headers != "Content-Type=application/json-patch+json" {
		b.t.Fatalf("%s headers = %q, want a JSON patch", call.route(), call.headers)
	}
	var operations []patchOperation
	if err := json.Unmarshal([]byte(call.body), &operations); err != nil {
		b.t.Fatalf("decode patch %q: %v", call.body, err)
	}
	return operations
}

// create answers a work item create.
func (b *scriptedBoards) create(call azCall, workItemType string) (any, error) {
	fields := map[string]any{}
	formats := map[string]string{}
	for _, operation := range b.decodePatch(call) {
		switch {
		case operation.Op != "add":
			b.t.Fatalf("create operation %#v is not add", operation)
		case strings.HasPrefix(operation.Path, "/fields/"):
			fields[strings.TrimPrefix(operation.Path, "/fields/")] = operation.Value
		case strings.HasPrefix(operation.Path, "/multilineFieldsFormat/"):
			formats[strings.TrimPrefix(operation.Path, "/multilineFieldsFormat/")] = fmt.Sprint(operation.Value)
		default:
			b.t.Fatalf("create operation %#v is not a field", operation)
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
	return map[string]any{"id": id, "fields": fields}, nil
}

// wiqlTag matches the tag condition of a correlation query.
var wiqlTag = regexp.MustCompile(`\[System\.Tags\] CONTAINS '([^']*)'`)

// query answers a WIQL query by tag.
func (b *scriptedBoards) query(call azCall) (any, error) {
	if fault, ok := b.faults["wiql"]; ok {
		delete(b.faults, "wiql")
		return nil, errors.New(fault.message)
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(call.body), &body); err != nil {
		b.t.Fatalf("decode wiql %q: %v", call.body, err)
	}
	match := wiqlTag.FindStringSubmatch(body.Query)
	if match == nil {
		b.t.Fatalf("wiql %q has no tag condition", body.Query)
	}
	found := []map[string]int{}
	for _, item := range b.items {
		if slices.ContainsFunc(item.tags(), func(tag string) bool { return strings.EqualFold(tag, match[1]) }) {
			found = append(found, map[string]int{"id": item.ID})
		}
	}
	return map[string]any{"workItems": found}, nil
}

// batch answers a batch read of work items.
func (b *scriptedBoards) batch(call azCall) (any, error) {
	values := []map[string]any{}
	for _, raw := range strings.Split(call.url.Query().Get("ids"), ",") {
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
func (b *scriptedBoards) read(id int) (any, error) {
	for _, item := range b.items {
		if item.ID == id {
			return map[string]any{"id": item.ID, "fields": item.Fields, "relations": item.Relations}, nil
		}
	}
	return nil, errors.New("az rest: Not Found({\"message\":\"TF401232: Work item does not exist\"})")
}

// link answers a work item update that adds one relation.
func (b *scriptedBoards) link(call azCall, id int) (any, error) {
	operations := b.decodePatch(call)
	if len(operations) != 1 || operations[0].Op != "add" || operations[0].Path != "/relations/-" {
		b.t.Fatalf("update of work item %d = %#v, want one added relation", id, operations)
	}
	encoded, _ := json.Marshal(operations[0].Value)
	var relation boardsRelation
	if err := json.Unmarshal(encoded, &relation); err != nil {
		b.t.Fatalf("decode relation: %v", err)
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
