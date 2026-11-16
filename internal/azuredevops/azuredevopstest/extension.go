package azuredevopstest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
var extensionGroups = map[string]bool{"boards": true, "repos": true, "devops": true}

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
func (b *Boards) runExtension(args []string) ([]byte, error) {
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
	response, err := b.answerExtension(call)
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
func (b *Boards) answerExtension(call ExtensionCall) (any, error) {
	if call.Route() == "repos show" {
		return RepositoryResponse(call)
	}
	return nil, errNoExtensionRoute(call)
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
