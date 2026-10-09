// Package azuredevops is the host-side Azure DevOps Services adapter. Boards
// work items are the work tracker and Azure Repos is the code host. It
// implements the tracker and codehost ports through the locally authenticated
// Azure CLI: every call is one `az rest` command, so az resolves the
// credential (an Entra ID login, service principal, or managed identity) and
// the adapter never reads or stores it.
package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// CommandRunner is the executable seam for the local Azure CLI.
type CommandRunner interface {
	Run(context.Context, []string) ([]byte, error)
}

// CommandTimeout is the deadline for one host az call.
const CommandTimeout = 2 * time.Minute

// devOpsResource is the Entra ID application of Azure DevOps. az rest needs
// it to request a token for dev.azure.com, because az cannot derive it from
// the URL.
const devOpsResource = "499b84ac-1321-427f-aa17-267ca6975798"

// API versions of the REST endpoints. Work item comments exist only as a
// preview in 7.1.
const (
	apiVersion        = "7.1"
	commentAPIVersion = "7.1-preview.4"
)

// serviceURL is the Azure DevOps Services root.
const serviceURL = "https://dev.azure.com/"

// commandRunner executes the host az binary with a deadline and without
// prompts.
type commandRunner struct{}

// Run executes az and returns its standard output. The error names only the
// HTTP method and URL path, never a request body.
func (commandRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	output, err := hostcmd.Run(ctx, hostcmd.Command{
		Operation: "az rest",
		Name:      "az",
		Args:      args,
		Env:       []string{"AZURE_CORE_ONLY_SHOW_ERRORS=1", "AZURE_CORE_NO_COLOR=1", "AZURE_CORE_COLLECT_TELEMETRY=0"},
		Timeout:   CommandTimeout,
	})
	if err != nil {
		var timeout *hostcmd.TimeoutError
		var limit *hostcmd.OutputLimitError
		message := strings.TrimSpace(string(output.Stderr))
		if message == "" || errors.As(err, &timeout) || errors.As(err, &limit) {
			return nil, err
		}
		return nil, fmt.Errorf("az rest: %s: %w", message, err)
	}
	return output.Stdout, nil
}

// Client implements the tracker and codehost ports for one Azure DevOps
// organization through the locally authenticated az CLI.
type Client struct {
	// Runner executes az. Nil selects the host az binary.
	Runner CommandRunner
	// Organization is the organization whose identity AuthenticatedLogin
	// reads. Every other call takes the organization from its repository.
	Organization string
	// AuthorizedUsers are the logins whose agent-ready tag makes a work item
	// eligible. A tag is free text that any editor can add, so the adapter
	// checks who added it (ADR 0019).
	AuthorizedUsers []string

	// mu guards the caches below.
	mu sync.Mutex
	// login caches the authenticated identity.
	login string
	// stateCategories caches the state categories of each work item type.
	stateCategories map[string]map[string]string
}

// NewClient returns an az-backed client for one organization.
func NewClient(organization string, authorizedUsers []string) *Client {
	return &Client{Runner: commandRunner{}, Organization: organization, AuthorizedUsers: authorizedUsers}
}

var _ tracker.AccountReader = (*Client)(nil)

// runner returns the injected command runner or the production CLI runner.
func (c *Client) runner() CommandRunner {
	if c.Runner == nil {
		return commandRunner{}
	}
	return c.Runner
}

// request is one REST call. Body is encoded as JSON; ContentType defaults to
// application/json.
type request struct {
	Method      string
	URL         string
	Body        any
	ContentType string
}

// call sends one REST request through az rest and decodes the JSON response
// into destination when it is not nil.
func (c *Client) call(ctx context.Context, req request, destination any) error {
	args := []string{"rest", "--method", strings.ToLower(req.Method), "--url", req.URL, "--resource", devOpsResource}
	if req.Body != nil {
		encoded, err := json.Marshal(req.Body)
		if err != nil {
			return fmt.Errorf("encode Azure DevOps request: %w", err)
		}
		contentType := req.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		args = append(args, "--headers", "Content-Type="+contentType, "--body", string(encoded))
	}
	output, err := c.runner().Run(ctx, args)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, redactedPath(req.URL), err)
	}
	if destination == nil || len(strings.TrimSpace(string(output))) == 0 {
		return nil
	}
	if err := json.Unmarshal(output, destination); err != nil {
		return fmt.Errorf("decode Azure DevOps response of %s %s: %w", req.Method, redactedPath(req.URL), err)
	}
	return nil
}

// redactedPath returns the URL path without the query, for error messages.
func redactedPath(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "the Azure DevOps API"
	}
	return parsed.Path
}

// organizationURL returns an organization-level API URL.
func organizationURL(organization, path string, query url.Values) string {
	return buildURL(url.PathEscape(organization)+"/_apis/"+path, query, apiVersion)
}

// projectURL returns a project-level API URL.
func projectURL(repository tracker.Repository, path string, query url.Values, version string) string {
	return buildURL(url.PathEscape(repository.Owner)+"/"+url.PathEscape(repository.Project)+"/_apis/"+path, query, version)
}

// repositoryURL returns a URL below the registered Git repository.
func repositoryURL(repository tracker.Repository, path string, query url.Values) string {
	return projectURL(repository, "git/repositories/"+url.PathEscape(repository.Name)+path, query, apiVersion)
}

// buildURL joins the service URL, an escaped path, and a query that always
// carries the API version.
func buildURL(escapedPath string, query url.Values, version string) string {
	values := url.Values{}
	for key, value := range query {
		values[key] = value
	}
	values.Set("api-version", version)
	return serviceURL + escapedPath + "?" + values.Encode()
}

// validateRepository rejects an incomplete repository identity before any
// call. The config validator already rejects path and query characters.
func validateRepository(repository tracker.Repository) error {
	if strings.TrimSpace(repository.Owner) == "" || strings.TrimSpace(repository.Project) == "" || strings.TrimSpace(repository.Name) == "" {
		return errors.New("Azure DevOps organization, project, and repository are required")
	}
	return nil
}

// identityRef is the identity projection of a work item or pull-request
// author.
type identityRef struct {
	UniqueName string `json:"uniqueName"`
}

// connectionDataResponse is the identity projection of the connection data
// endpoint.
type connectionDataResponse struct {
	AuthenticatedUser struct {
		Properties struct {
			Account struct {
				Value string `json:"$value"`
			} `json:"Account"`
		} `json:"properties"`
	} `json:"authenticatedUser"`
}

// errAccountUnavailable is the bounded diagnosis returned when the
// authenticated Azure DevOps identity cannot be read. It never contains az
// output.
var errAccountUnavailable = errors.New("the authenticated Azure DevOps identity could not be read: run az login for the identity that supervises the repository")

// AuthenticatedLogin returns the account name (user principal name) of the
// identity az is signed in as. Comment and update authors carry the same
// name in their uniqueName field.
func (c *Client) AuthenticatedLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.login
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if strings.TrimSpace(c.Organization) == "" {
		return "", errors.New("Azure DevOps organization is required to read the authenticated identity")
	}
	var response connectionDataResponse
	if err := c.call(ctx, request{Method: "GET", URL: organizationURL(c.Organization, "connectionData", nil)}, &response); err != nil {
		var timeout *hostcmd.TimeoutError
		if errors.As(err, &timeout) {
			return "", fmt.Errorf("%w: %w", errAccountUnavailable, timeout)
		}
		return "", errAccountUnavailable
	}
	login := strings.TrimSpace(response.AuthenticatedUser.Properties.Account.Value)
	if login == "" || strings.ContainsAny(login, "\x00\r\n/") {
		return "", errAccountUnavailable
	}
	c.mu.Lock()
	c.login = login
	c.mu.Unlock()
	return login, nil
}

// authorized reports whether login is one of the authorized users.
func (c *Client) authorized(login string) bool {
	login = strings.TrimSpace(login)
	for _, candidate := range c.AuthorizedUsers {
		if login != "" && strings.EqualFold(strings.TrimSpace(candidate), login) {
			return true
		}
	}
	return false
}
