package azuredevops

// This file holds the shared seam of the Azure DevOps CLI extension route
// (ADR 0030): az boards, az repos, and az devops invoke authenticated by the
// CLI-managed az devops login. The route never runs az rest and never asks
// for an Entra ID token; Factory never reads, collects, or stores the PAT.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// ErrExtensionOperationUnavailable reports an adapter operation that the
// extension route does not implement yet. The route fails closed: it never
// falls back to az rest or an Entra ID token.
var ErrExtensionOperationUnavailable = errors.New("this Azure DevOps operation is not available on the devops_extension route; Factory does not fall back to az rest")

// LoginRequiredError reports that the Azure DevOps CLI extension has no
// usable credential for the organization: no az devops login, or an expired
// or revoked PAT. It never contains CLI output.
type LoginRequiredError struct {
	// OrganizationURL is the organization the command targeted.
	OrganizationURL string
}

// Error returns the repair instruction for the operator.
func (e *LoginRequiredError) Error() string {
	return "Azure DevOps CLI sign-in is missing or expired for " + e.OrganizationURL + ": run az devops login --organization " + e.OrganizationURL
}

// NewClientForRoute returns a client for one organization on the selected
// route. The az rest route matches NewClient.
func NewClientForRoute(organization string, authorizedUsers []string, route config.AzureRoute) *Client {
	client := NewClient(organization, authorizedUsers)
	client.Route = route
	if route == config.AzureRouteDevOpsExtension {
		client.Runner = extensionRunner{timeout: CommandTimeout}
	}
	return client
}

// extensionRoute reports whether the client uses the extension route.
func (c *Client) extensionRoute() bool {
	return c.Route == config.AzureRouteDevOpsExtension
}

// extensionEnvironment makes every extension command non-interactive and
// quiet: no prompt, no dynamic extension install, errors only, no color, and
// no telemetry. A missing extension then fails as an unknown command.
var extensionEnvironment = []string{
	"AZURE_CORE_ONLY_SHOW_ERRORS=1",
	"AZURE_CORE_NO_COLOR=1",
	"AZURE_CORE_COLLECT_TELEMETRY=0",
	"AZURE_CORE_DISABLE_CONFIRM_PROMPT=1",
	"AZURE_EXTENSION_USE_DYNAMIC_INSTALL=no",
}

// extensionRunner executes extension-route az commands with a deadline, an
// empty standard input, and sanitized errors.
type extensionRunner struct {
	timeout time.Duration
}

// Run executes az and returns its standard output. A missing or expired
// login becomes a LoginRequiredError; any other failure returns at most one
// bounded, single-line, secret-redacted line of standard error.
func (r extensionRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	output, err := hostcmd.Run(ctx, hostcmd.Command{
		Operation: extensionOperation(args),
		Name:      "az",
		Args:      args,
		Env:       extensionEnvironment,
		Timeout:   r.timeout,
	})
	if err == nil {
		return output.Stdout, nil
	}
	var timeout *hostcmd.TimeoutError
	var limit *hostcmd.OutputLimitError
	if errors.As(err, &timeout) || errors.As(err, &limit) || ctx.Err() != nil {
		return nil, err
	}
	stderr := string(output.Stderr)
	if loginRequired(stderr) {
		return nil, &LoginRequiredError{OrganizationURL: flagValue(args, "--organization")}
	}
	message := sanitizeCLIMessage(stderr)
	if message == "" {
		return nil, fmt.Errorf("%s: %w", extensionOperation(args), err)
	}
	return nil, fmt.Errorf("%s: %s: %w", extensionOperation(args), message, err)
}

// extensionOperation names a command by its group and subcommand, for
// example "az repos show", never by its values.
func extensionOperation(args []string) string {
	words := []string{"az"}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") || len(words) == 4 {
			break
		}
		words = append(words, arg)
	}
	return strings.Join(words, " ")
}

// loginMessages are the extension's texts for a missing credential and for
// HTTP 401 (azext_devops services.py and devops_sdk client.py).
var loginMessages = []string{
	"you need to run the login command",
	"az devops login",
	"requires user authentication",
	"TF400813",
}

// loginRequired reports whether standard error says that the credential is
// missing, expired, or not authorized.
func loginRequired(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, message := range loginMessages {
		if strings.Contains(lower, strings.ToLower(message)) {
			return true
		}
	}
	return false
}

// maxCLIMessageRunes bounds the CLI text that an error may carry.
const maxCLIMessageRunes = 300

// secretLike matches long opaque runs such as PATs and bearer tokens. GUIDs
// and Azure DevOps error codes are shorter and stay readable.
var secretLike = regexp.MustCompile(`[A-Za-z0-9+/=_.\-]{40,}`)

// sanitizeCLIMessage returns the first nonempty line of stderr without
// control characters, with token-like runs and the PAT environment value
// redacted, bounded to maxCLIMessageRunes.
func sanitizeCLIMessage(stderr string) string {
	line := ""
	for _, candidate := range strings.Split(stderr, "\n") {
		if strings.TrimSpace(candidate) != "" {
			line = strings.TrimSpace(candidate)
			break
		}
	}
	if pat := os.Getenv("AZURE_DEVOPS_EXT_PAT"); pat != "" {
		line = strings.ReplaceAll(line, pat, "[redacted]")
	}
	line = secretLike.ReplaceAllString(line, "[redacted]")
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	if utf8.RuneCountInString(line) > maxCLIMessageRunes {
		line = string([]rune(line)[:maxCLIMessageRunes]) + "..."
	}
	return line
}

// flagValue returns the value after flag in args, or "".
func flagValue(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

// organizationURL returns the explicit --organization value of one
// organization.
func extensionOrganizationURL(organization string) string {
	return serviceURL + organization
}

// extension runs one dedicated az boards, az repos, or az devops command for
// organization and decodes its JSON output into destination when it is not
// nil. It adds explicit organization targeting, disables detection from Git
// remotes and CLI defaults, and selects JSON output. Callers pass --project
// and --repository explicitly wherever the command accepts them.
func (c *Client) extension(ctx context.Context, organization string, args []string, destination any) error {
	if !c.extensionRoute() {
		return errors.New("Azure DevOps CLI extension commands require route devops_extension")
	}
	if strings.TrimSpace(organization) == "" || !plainSegment(organization) {
		return errors.New("Azure DevOps organization is required as a plain name")
	}
	argv := append(append([]string{}, args...), "--organization", extensionOrganizationURL(organization), "--detect", "false", "--output", "json", "--only-show-errors")
	output, err := c.runner().Run(ctx, argv)
	if err != nil {
		return err
	}
	if destination == nil || len(bytes.TrimSpace(output)) == 0 {
		return nil
	}
	if err := json.Unmarshal(output, destination); err != nil {
		return fmt.Errorf("decode the output of %s: %w", extensionOperation(argv), err)
	}
	return nil
}

// plainSegment reports whether value is safe as one name segment.
func plainSegment(value string) bool {
	return !strings.ContainsAny(value, "/\\?#%") && strings.IndexFunc(value, unicode.IsControl) < 0
}

// invokeRequest is one az devops invoke call: a REST resource that the
// extension resolves by area and resource name. ADR 0030 lists the verified
// area/resource pairs.
type invokeRequest struct {
	// Area and Resource name the resource location, for example "git" and
	// "statuses".
	Area     string
	Resource string
	// Method is the HTTP method; empty selects GET.
	Method string
	// APIVersion is "<major>.<minor>" or "<major>.<minor>-preview". The
	// extension cannot parse a resource version such as "7.1-preview.4".
	APIVersion string
	// Route and Query are the route and query parameters.
	Route map[string]string
	Query map[string]string
	// Body is encoded as JSON into a private temporary file passed as
	// --in-file. Nil sends no body.
	Body any
	// MediaType is the request content type; empty selects
	// application/json.
	MediaType string
}

// invokeAPIVersion is the API version form that az devops invoke accepts.
var invokeAPIVersion = regexp.MustCompile(`^\d+\.\d+(-preview)?$`)

// invoke runs one az devops invoke call for organization and decodes its
// JSON output into destination when it is not nil. The body never appears
// in argv or an error.
func (c *Client) invoke(ctx context.Context, organization string, req invokeRequest, destination any) error {
	if req.Area == "" || req.Resource == "" {
		return errors.New("az devops invoke needs an area and a resource")
	}
	if !invokeAPIVersion.MatchString(req.APIVersion) {
		return fmt.Errorf("az devops invoke cannot use API version %q; use <major>.<minor> or <major>.<minor>-preview", req.APIVersion)
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	args := []string{"devops", "invoke", "--area", req.Area, "--resource", req.Resource, "--http-method", method, "--api-version", req.APIVersion}
	if len(req.Route) > 0 {
		args = append(append(args, "--route-parameters"), parameterList(req.Route)...)
	}
	if len(req.Query) > 0 {
		args = append(append(args, "--query-parameters"), parameterList(req.Query)...)
	}
	if req.Body != nil {
		path, cleanup, err := writeInvokeBody(req.Body)
		if err != nil {
			return err
		}
		defer cleanup()
		mediaType := req.MediaType
		if mediaType == "" {
			mediaType = "application/json"
		}
		args = append(args, "--in-file", path, "--encoding", "utf-8", "--media-type", mediaType)
	}
	if err := c.extension(ctx, organization, args, destination); err != nil {
		return fmt.Errorf("%s %s/%s: %w", method, req.Area, req.Resource, err)
	}
	return nil
}

// parameterList renders parameters as sorted key=value arguments, so argv
// is deterministic.
func parameterList(parameters map[string]string) []string {
	keys := make([]string, 0, len(parameters))
	for key := range parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	list := make([]string, 0, len(keys))
	for _, key := range keys {
		list = append(list, key+"="+parameters[key])
	}
	return list
}

// writeInvokeBody writes body as compact JSON to a temporary file that only
// the owner can read, and returns its path and a cleanup function.
func writeInvokeBody(body any) (string, func(), error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return "", nil, fmt.Errorf("encode Azure DevOps request: %w", err)
	}
	file, err := os.CreateTemp("", "factory-az-invoke-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("create the az devops invoke request file: %w", err)
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if _, err := file.Write(bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write the az devops invoke request file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close the az devops invoke request file: %w", err)
	}
	return file.Name(), cleanup, nil
}

// RepositoryIdentity is the stable identity of one Azure Repos repository,
// resolved within its organization and project. ID is the GUID that the
// factory-repo:<GUID> assignment tag names (ADR 0030).
type RepositoryIdentity struct {
	Organization string
	ProjectID    string
	Project      string
	ID           string
	Name         string
}

// repositoryShowResponse is the projection of az repos show.
type repositoryShowResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Project *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
}

// canonicalGUID matches a GUID without braces, in either letter case.
var canonicalGUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ResolveRepository resolves the registered repository name to its stable
// identity with az repos show, targeted at the registered organization and
// project. It refuses a response for another project or repository, or one
// without GUIDs. Only the extension route supports it.
func (c *Client) ResolveRepository(ctx context.Context, repository tracker.Repository) (RepositoryIdentity, error) {
	if err := validateRepository(repository); err != nil {
		return RepositoryIdentity{}, err
	}
	var response repositoryShowResponse
	args := []string{"repos", "show", "--project", repository.Project, "--repository", repository.Name}
	if err := c.extension(ctx, repository.Owner, args, &response); err != nil {
		return RepositoryIdentity{}, fmt.Errorf("resolve Azure Repos repository %s: %w", repository, err)
	}
	if response.Project == nil || !strings.EqualFold(response.Project.Name, repository.Project) || !canonicalGUID.MatchString(response.Project.ID) {
		return RepositoryIdentity{}, fmt.Errorf("resolve Azure Repos repository %s: the response names another project", repository)
	}
	if !strings.EqualFold(response.Name, repository.Name) || !canonicalGUID.MatchString(response.ID) {
		return RepositoryIdentity{}, fmt.Errorf("resolve Azure Repos repository %s: the response names another repository or no repository GUID", repository)
	}
	return RepositoryIdentity{
		Organization: repository.Owner,
		ProjectID:    strings.ToLower(response.Project.ID),
		Project:      response.Project.Name,
		ID:           strings.ToLower(response.ID),
		Name:         response.Name,
	}, nil
}
