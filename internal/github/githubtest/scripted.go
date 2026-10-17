// Package githubtest holds a stateful scripted gh CLI. Tests use it to drive
// the real GitHub planning adapter end to end without a network or a GitHub
// account: the adapter tests in internal/github and the integrated
// acceptance scenarios that publish a reviewed plan.
package githubtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Owner and Name identify the only repository the scripted gh answers for.
const (
	Owner = "example"
	Name  = "project"
)

// Login is the authenticated account of the scripted gh.
const Login = "factory-bot"

// WritableRepository is the repository response of a target where the
// account can open issues.
const WritableRepository = `{"has_issues":true,"archived":false,"permissions":{"pull":true,"push":true,"triage":true}}`

// NativeCapabilities is the GraphQL introspection answer of a GitHub host
// with native sub-issues and blocking dependencies.
const NativeCapabilities = `{"data":{"__type":{"fields":[{"name":"title"},{"name":"subIssues"},{"name":"parent"},{"name":"blockedBy"},{"name":"blocking"}]}}}`

// ReadOnlyRepository is the repository response of a target where the
// account has read access only: it can open issues but cannot link them as
// sub-issues or blocking dependencies.
const ReadOnlyRepository = `{"has_issues":true,"archived":false,"permissions":{"pull":true,"triage":false,"push":false,"maintain":false,"admin":false}}`

// LegacyCapabilities is the introspection answer of a GitHub host without
// either relationship.
const LegacyCapabilities = `{"data":{"__type":{"fields":[{"name":"title"},{"name":"body"}]}}}`

// Issue is one issue of the scripted repository.
type Issue struct {
	// Number is the issue number; the database ID is 5000 + Number.
	Number int
	// Title is the issue title.
	Title string
	// Body is the issue body.
	Body string
	// Labels are the labels the create request asked for.
	Labels []string
}

// fault fails the next write with a key once.
type fault struct {
	err        error
	afterWrite bool
}

// Scripted is a stateful scripted gh for end-to-end publication tests. It
// answers the gh api calls of the planning adapter from an in-memory
// repository and fails a selected write once, optionally after applying it,
// as a lost response does. An unexpected call fails the test and returns an
// error, so it is safe to call from a background goroutine.
type Scripted struct {
	t      testing.TB
	mu     sync.Mutex
	native bool
	// repository is the repository response; empty means WritableRepository.
	repository string
	issues     []Issue
	subIssues  map[int][]int
	blockedBy  map[int][]int
	comments   map[int][]string
	faults     map[string]fault
	writes     []string
	held       map[string]bool
}

// New returns an empty repository; native selects whether the host offers
// sub-issues and blocking dependencies.
func New(t testing.TB, native bool) *Scripted {
	return &Scripted{
		t: t, native: native, subIssues: map[int][]int{}, blockedBy: map[int][]int{},
		comments: map[int][]string{}, faults: map[string]fault{}, held: map[string]bool{},
	}
}

// SetNative changes whether the host offers the native relationships, as a
// host upgrade or a feature flag change does.
func (g *Scripted) SetNative(native bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.native = native
}

// SetRepository replaces the repository response, for example with
// ReadOnlyRepository for an account without triage access.
func (g *Scripted) SetRepository(response string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repository = response
}

// FailOnce fails the next write with key: "create:<title>",
// "sub_issue:<parent>", "blocked_by:<dependent>", or "comment:<issue>".
// With afterWrite the write is applied first, as a lost response does.
func (g *Scripted) FailOnce(key string, err error, afterWrite bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.faults[key] = fault{err: err, afterWrite: afterWrite}
}

// HoldAfterWrite applies the next write with key and then holds its response
// until the caller's context ends, as a process that stops while the request
// is in flight. The write stays applied; the caller gets the context error.
func (g *Scripted) HoldAfterWrite(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[key] = true
}

// Creates returns the number of issue create requests.
func (g *Scripted) Creates() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := 0
	for _, write := range g.writes {
		if strings.HasPrefix(write, "create:") {
			count++
		}
	}
	return count
}

// Writes returns the key of every write request in order.
func (g *Scripted) Writes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.writes)
}

// Issues returns a copy of every issue in creation order.
func (g *Scripted) Issues() []Issue {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.issues)
}

// SubIssues returns the sub-issue numbers of every parent issue.
func (g *Scripted) SubIssues() map[int][]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneLinks(g.subIssues)
}

// BlockedBy returns the blocking issue numbers of every dependent issue.
func (g *Scripted) BlockedBy() map[int][]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneLinks(g.blockedBy)
}

// Comments returns the comment bodies of every commented issue.
func (g *Scripted) Comments() map[int][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	comments := map[int][]string{}
	for key, value := range g.comments {
		comments[key] = slices.Clone(value)
	}
	return comments
}

// IssueURL returns the address of one scripted issue.
func IssueURL(number int) string {
	return "https://github.com/" + Owner + "/" + Name + "/issues/" + strconv.Itoa(number)
}

// Run answers one gh invocation.
func (g *Scripted) Run(ctx context.Context, args []string, input []byte) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(args) < 2 || args[0] != "api" {
		return g.unexpected(args)
	}
	method := "GET"
	if index := slices.Index(args, "--method"); index >= 0 && index+1 < len(args) {
		method = args[index+1]
	}
	path := strings.TrimPrefix(args[1], "repos/"+Owner+"/"+Name)
	switch {
	case args[1] == "user":
		return []byte(`{"login":"` + Login + `"}`), nil
	case args[1] == "graphql":
		if g.native {
			return []byte(NativeCapabilities), nil
		}
		return []byte(LegacyCapabilities), nil
	case path == "" && g.repository != "":
		return []byte(g.repository), nil
	case path == "":
		return []byte(WritableRepository), nil
	case path == "/issues" && method == "GET":
		return g.listIssues(args)
	case path == "/issues" && method == "POST":
		return g.write(ctx, input, g.createIssue)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/issues/"), "/")
	number, err := strconv.Atoi(parts[0])
	if err != nil || number < 1 || number > len(g.issues) {
		return g.unexpected(args)
	}
	switch rest := strings.Join(parts[1:], "/"); {
	case rest == "" && method == "GET":
		return json.Marshal(map[string]int{"id": 5000 + number, "number": number})
	case rest == "sub_issues" && method == "GET":
		return issueNumbers(g.subIssues[number]), nil
	case rest == "dependencies/blocked_by" && method == "GET":
		return issueNumbers(g.blockedBy[number]), nil
	case rest == "comments" && method == "GET":
		return g.listComments(number), nil
	case rest == "sub_issues" && method == "POST":
		return g.write(ctx, input, func(body map[string]any) (string, []byte) {
			g.subIssues[number] = append(g.subIssues[number], databaseNumber(body["sub_issue_id"]))
			return "sub_issue:" + strconv.Itoa(number), []byte(`{}`)
		})
	case rest == "dependencies/blocked_by" && method == "POST":
		return g.write(ctx, input, func(body map[string]any) (string, []byte) {
			g.blockedBy[number] = append(g.blockedBy[number], databaseNumber(body["issue_id"]))
			return "blocked_by:" + strconv.Itoa(number), []byte(`{}`)
		})
	case rest == "comments" && method == "POST":
		return g.write(ctx, input, func(body map[string]any) (string, []byte) {
			text, _ := body["body"].(string)
			g.comments[number] = append(g.comments[number], text)
			return "comment:" + strconv.Itoa(number), []byte(`{"id":1}`)
		})
	}
	return g.unexpected(args)
}

// unexpected fails the test for a call the script does not know.
func (g *Scripted) unexpected(args []string) ([]byte, error) {
	g.t.Errorf("unexpected gh call %#v", args)
	return nil, fmt.Errorf("scripted gh: unexpected call %v", args)
}

// write decodes one request, applies it, and fails it when a fault matches
// its key. A fault without afterWrite fails before the write is applied. A
// held write keeps its response until ctx ends.
func (g *Scripted) write(ctx context.Context, input []byte, apply func(map[string]any) (string, []byte)) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(input, &body); err != nil {
		g.t.Errorf("decode request body: %v", err)
		return nil, err
	}
	snapshot := g.snapshot()
	key, output := apply(body)
	g.writes = append(g.writes, key)
	if g.held[key] {
		delete(g.held, key)
		g.mu.Unlock()
		<-ctx.Done()
		g.mu.Lock()
		return nil, errors.Join(errors.New("gh api: the request was interrupted"), ctx.Err())
	}
	if fault, ok := g.faults[key]; ok {
		delete(g.faults, key)
		if !fault.afterWrite {
			g.restore(snapshot)
		}
		return nil, fault.err
	}
	return output, nil
}

// createIssue adds one issue from a create request.
func (g *Scripted) createIssue(body map[string]any) (string, []byte) {
	title, _ := body["title"].(string)
	text, _ := body["body"].(string)
	issue := Issue{Number: len(g.issues) + 1, Title: title, Body: text}
	if labels, ok := body["labels"].([]any); ok {
		for _, label := range labels {
			issue.Labels = append(issue.Labels, fmt.Sprint(label))
		}
	}
	g.issues = append(g.issues, issue)
	output, _ := json.Marshal(map[string]any{
		"id": 5000 + issue.Number, "number": issue.Number, "html_url": IssueURL(issue.Number),
	})
	return "create:" + issue.Title, output
}

// listIssues answers the account's issue listing, newest first.
func (g *Scripted) listIssues(args []string) ([]byte, error) {
	if !slices.Contains(args, "creator="+Login) || !slices.Contains(args, "page=1") {
		return []byte(`[]`), nil
	}
	listed := []map[string]any{}
	for index := len(g.issues) - 1; index >= 0; index-- {
		issue := g.issues[index]
		listed = append(listed, map[string]any{
			"number": issue.Number, "html_url": IssueURL(issue.Number), "body": issue.Body,
			"user": map[string]string{"login": Login},
		})
	}
	return json.Marshal(listed)
}

// listComments answers one issue's comment listing.
func (g *Scripted) listComments(number int) []byte {
	comments := []map[string]any{}
	for index, body := range g.comments[number] {
		comments = append(comments, map[string]any{"id": index + 1, "body": body, "user": map[string]string{"login": Login}})
	}
	output, _ := json.Marshal([][]map[string]any{comments})
	return output
}

// state is a copy of the scripted repository state.
type state struct {
	issues    []Issue
	subIssues map[int][]int
	blockedBy map[int][]int
	comments  map[int][]string
}

// snapshot copies the repository state.
func (g *Scripted) snapshot() state {
	comments := map[int][]string{}
	for key, value := range g.comments {
		comments[key] = slices.Clone(value)
	}
	return state{issues: slices.Clone(g.issues), subIssues: cloneLinks(g.subIssues), blockedBy: cloneLinks(g.blockedBy), comments: comments}
}

// restore resets the repository state.
func (g *Scripted) restore(saved state) {
	g.issues, g.subIssues, g.blockedBy, g.comments = saved.issues, saved.subIssues, saved.blockedBy, saved.comments
}

// cloneLinks deep-copies a link map.
func cloneLinks(source map[int][]int) map[int][]int {
	copied := maps.Clone(source)
	for key, value := range copied {
		copied[key] = slices.Clone(value)
	}
	return copied
}

// databaseNumber converts a database ID in a request body to its issue
// number.
func databaseNumber(value any) int {
	id, _ := value.(float64)
	return int(id) - 5000
}

// issueNumbers renders a slurped issue listing of numbers.
func issueNumbers(numbers []int) []byte {
	issues := []map[string]int{}
	for _, number := range numbers {
		issues = append(issues, map[string]int{"id": 5000 + number, "number": number})
	}
	output, _ := json.Marshal([][]map[string]int{issues})
	return output
}
