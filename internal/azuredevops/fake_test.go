package azuredevops_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// repository is the registered Azure DevOps repository of every test.
var repository = tracker.Repository{Owner: "contoso", Project: "Factory Pilot", Name: "service"}

// azCall is one recorded az rest invocation.
type azCall struct {
	method  string
	url     *url.URL
	headers string
	body    string
}

// route returns "METHOD /path" for the call, with the path unescaped.
func (c azCall) route() string {
	return c.method + " " + c.url.Path
}

// fakeAz is a scripted az CLI. It answers az rest calls from a route table
// keyed by "METHOD /path" and records every call for assertions.
type fakeAz struct {
	t      *testing.T
	routes map[string][]string
	calls  []azCall
}

// newFakeAz returns a fake az CLI with an empty route table.
func newFakeAz(t *testing.T) *fakeAz {
	t.Helper()
	return &fakeAz{t: t, routes: map[string][]string{}}
}

// on adds responses for one route. Repeated calls return them in order, and
// the last response repeats.
func (f *fakeAz) on(route string, responses ...string) *fakeAz {
	f.routes[route] = append(f.routes[route], responses...)
	return f
}

// Run parses one az rest command line and returns the scripted response.
func (f *fakeAz) Run(_ context.Context, args []string) ([]byte, error) {
	call, err := parseAzCall(f.t, args)
	if err != nil {
		return nil, err
	}
	f.calls = append(f.calls, call)
	responses := f.routes[call.route()]
	if len(responses) == 0 {
		return nil, errors.New("fake az: no route for " + call.route())
	}
	response := responses[0]
	if len(responses) > 1 {
		f.routes[call.route()] = responses[1:]
	}
	if strings.HasPrefix(response, "error:") {
		return nil, errors.New(strings.TrimPrefix(response, "error:"))
	}
	return []byte(response), nil
}

// parseAzCall parses one az rest command line and fails the test when it has
// no Azure DevOps resource or no API version.
func parseAzCall(t *testing.T, args []string) (azCall, error) {
	t.Helper()
	call, err := azuredevopstest.ParseCall(args)
	if err != nil {
		t.Error(err)
		return azCall{}, err
	}
	return azCall{method: call.Method, url: call.URL, headers: call.Headers, body: call.Body}, nil
}

// called returns the recorded calls of one route.
func (f *fakeAz) called(route string) []azCall {
	matched := make([]azCall, 0)
	for _, call := range f.calls {
		if call.route() == route {
			matched = append(matched, call)
		}
	}
	return matched
}

// newClient returns an adapter over the fake CLI with alice authorized.
func newClient(az *fakeAz) *azuredevops.Client {
	return &azuredevops.Client{Runner: az, Organization: "contoso", AuthorizedUsers: []string{"alice@contoso.com"}}
}

// decodeBody decodes the JSON body of one call.
func decodeBody(t *testing.T, call azCall, destination any) {
	t.Helper()
	if err := json.Unmarshal([]byte(call.body), destination); err != nil {
		t.Fatalf("decode body %q: %v", call.body, err)
	}
}

// Shared paths of the test repository.
const (
	projectPath    = azuredevopstest.ProjectPath
	repositoryPath = azuredevopstest.RepositoryPath
	workItemsPath  = azuredevopstest.WorkItemsPath
)

// connectionData is the identity response for alice.
const connectionData = `{"authenticatedUser":{"id":"5f1c","properties":{"Account":{"$type":"System.String","$value":"alice@contoso.com"}}}}`

// userStoryStates is the state list of the User Story type in the Agile process.
const userStoryStates = `{"value":[{"name":"New","category":"Proposed"},{"name":"Active","category":"InProgress"},{"name":"Resolved","category":"Resolved"},{"name":"Closed","category":"Completed"},{"name":"Removed","category":"Removed"}]}`
