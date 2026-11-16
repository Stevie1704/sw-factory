package azuredevopstest

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PullRequest is one pull request of a scripted Git repository. It belongs
// to exactly one repository, named by its GUID, and lists the work items
// that are natively linked to it.
type PullRequest struct {
	ID           int
	RepositoryID string
	Source       string
	Target       string
	Title        string
	Description  string
	Draft        bool
	// WorkItems are the IDs of the natively linked work items.
	WorkItems []int
	// threads are the pull request threads as az devops invoke returns them.
	threads []map[string]any
	// reviewers are the reviewers as az repos pr reviewer list returns them.
	reviewers []map[string]any
}

// PullRequests returns a copy of every pull request in creation order.
func (b *Boards) PullRequests() []PullRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.pullRequests)
}

// RequestChanges records a review by reviewer on pull request number, as
// Azure Repos stores one: an active text thread with the finding, then a
// vote thread with vote -5 ("waiting for author") that names the voter only
// by identity ID, and the voter in the reviewer list.
func (b *Boards) RequestChanges(number int, reviewer, finding string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	index := b.pullRequestIndex(number)
	if index < 0 {
		b.t.Fatalf("RequestChanges: pull request %d does not exist", number)
	}
	pullRequest := &b.pullRequests[index]
	identity := fmt.Sprintf("a1a1a1a1-0000-0000-0000-%012d", len(pullRequest.reviewers)+1)
	at := time.Date(2026, 11, 16, 10, len(pullRequest.threads), 0, 0, time.UTC)
	pullRequest.threads = append(pullRequest.threads,
		map[string]any{
			"id": len(pullRequest.threads) + 1, "publishedDate": at, "status": "active",
			"comments": []map[string]any{{"id": 1, "author": map[string]string{"uniqueName": reviewer}, "content": finding, "commentType": "text", "publishedDate": at}},
		},
		map[string]any{
			"id": len(pullRequest.threads) + 2, "publishedDate": at.Add(time.Second),
			"properties": map[string]any{
				"CodeReviewThreadType":  map[string]string{"$type": "System.String", "$value": "VoteUpdate"},
				"CodeReviewVoteResult":  map[string]string{"$type": "System.String", "$value": "-5"},
				"CodeReviewVotedByTfId": map[string]string{"$type": "System.String", "$value": identity},
			},
			"comments": []map[string]any{{"id": 1, "author": map[string]string{"uniqueName": FactoryIdentity}, "content": "voted -5", "commentType": "system", "publishedDate": at.Add(time.Second)}},
		})
	pullRequest.reviewers = append(pullRequest.reviewers, map[string]any{"id": identity, "uniqueName": reviewer, "vote": -5})
}

// pullRequestIndex returns the index of a pull request, or -1. The caller
// holds b.mu.
func (b *Boards) pullRequestIndex(number int) int {
	return slices.IndexFunc(b.pullRequests, func(pullRequest PullRequest) bool { return pullRequest.ID == number })
}

// repositoryName returns the scripted repository name of a GUID.
func repositoryName(id string) string {
	for name, value := range repositoryIDs {
		if value == id {
			return name
		}
	}
	return ""
}

// repositoryIDOf returns the GUID of a scripted repository named by name or
// GUID, or "".
func repositoryIDOf(nameOrID string) string {
	for name, id := range repositoryIDs {
		if strings.EqualFold(nameOrID, name) || strings.EqualFold(nameOrID, id) {
			return id
		}
	}
	return ""
}

// branchRef returns the full ref of a branch, as az repos accepts both.
func branchRef(branch string) string {
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

// pullRequestJSON renders one pull request as az repos pr returns it.
func pullRequestJSON(pullRequest PullRequest) map[string]any {
	return map[string]any{
		"pullRequestId": pullRequest.ID, "status": "active", "isDraft": pullRequest.Draft,
		"title": pullRequest.Title, "description": pullRequest.Description,
		"sourceRefName": pullRequest.Source, "targetRefName": pullRequest.Target,
		"repository": map[string]any{
			"id": pullRequest.RepositoryID, "name": repositoryName(pullRequest.RepositoryID),
			"project": map[string]string{"id": ProjectID, "name": Project},
		},
	}
}

// answerRepos answers the az repos pr commands and the pull request thread
// invoke route. ok is false for any other command.
func (b *Boards) answerRepos(call ExtensionCall) (response any, ok bool, err error) {
	switch call.Route() {
	case "repos pr create":
		response, err = b.createPullRequest(call)
	case "repos pr list":
		response, err = b.listPullRequests(call)
	case "repos pr show", "repos pr update", "repos pr work-item list", "repos pr work-item add", "repos pr reviewer list":
		response, err = b.pullRequestByID(call)
	case "devops invoke GET git/pullRequestThreads":
		response, err = b.pullRequestThreads(call)
	default:
		return nil, false, nil
	}
	return response, true, err
}

// createPullRequest answers az repos pr create in the repository that
// --repository names, with the work items of --work-items linked.
func (b *Boards) createPullRequest(call ExtensionCall) (any, error) {
	if call.Flag("--project") != Project {
		return nil, errors.New("ERROR: TF200016: The following project does not exist: " + call.Flag("--project"))
	}
	repositoryID := repositoryIDOf(call.Flag("--repository"))
	if repositoryID == "" {
		return nil, errors.New("ERROR: TF401019: The Git repository with name or identifier " + call.Flag("--repository") + " does not exist")
	}
	pullRequest := PullRequest{
		ID: 200 + len(b.pullRequests) + 1, RepositoryID: repositoryID,
		Source: branchRef(call.Flag("--source-branch")), Target: branchRef(call.Flag("--target-branch")),
		Title: call.Flag("--title"), Description: call.Flag("--description"), Draft: call.Flag("--draft") == "true",
	}
	for _, value := range call.Flags["--work-items"] {
		id, err := strconv.Atoi(value)
		if err != nil || b.itemIndex(id) < 0 {
			return nil, errors.New("ERROR: work item " + value + " does not exist")
		}
		pullRequest.WorkItems = append(pullRequest.WorkItems, id)
	}
	b.pullRequests = append(b.pullRequests, pullRequest)
	return pullRequestJSON(pullRequest), nil
}

// listPullRequests answers az repos pr list for one repository and branch
// pair.
func (b *Boards) listPullRequests(call ExtensionCall) (any, error) {
	repositoryID := repositoryIDOf(call.Flag("--repository"))
	found := []map[string]any{}
	for _, pullRequest := range b.pullRequests {
		if pullRequest.RepositoryID == repositoryID && pullRequest.Source == branchRef(call.Flag("--source-branch")) && pullRequest.Target == branchRef(call.Flag("--target-branch")) {
			found = append(found, pullRequestJSON(pullRequest))
		}
	}
	return found, nil
}

// pullRequestByID answers the organization-scoped commands that select a
// pull request by --id only: show, update, the work-item list and add, and
// the reviewer list.
func (b *Boards) pullRequestByID(call ExtensionCall) (any, error) {
	id, _ := strconv.Atoi(call.Flag("--id"))
	index := b.pullRequestIndex(id)
	if index < 0 {
		return nil, errors.New("ERROR: TF401180: The requested pull request was not found.")
	}
	pullRequest := &b.pullRequests[index]
	switch call.Route() {
	case "repos pr update":
		if _, ok := call.Flags["--title"]; ok {
			pullRequest.Title = call.Flag("--title")
		}
		if _, ok := call.Flags["--description"]; ok {
			pullRequest.Description = call.Flag("--description")
		}
		if draft := call.Flag("--draft"); draft != "" {
			pullRequest.Draft = draft == "true"
		}
	case "repos pr work-item add":
		for _, value := range call.Flags["--work-items"] {
			linked, _ := strconv.Atoi(value)
			if !slices.Contains(pullRequest.WorkItems, linked) {
				pullRequest.WorkItems = append(pullRequest.WorkItems, linked)
			}
		}
		fallthrough
	case "repos pr work-item list":
		items := []map[string]int{}
		for _, linked := range pullRequest.WorkItems {
			items = append(items, map[string]int{"id": linked})
		}
		return items, nil
	case "repos pr reviewer list":
		return slices.Clone(pullRequest.reviewers), nil
	}
	return pullRequestJSON(*pullRequest), nil
}

// pullRequestThreads answers the thread list of a pull request of the
// repository that the route names.
func (b *Boards) pullRequestThreads(call ExtensionCall) (any, error) {
	route := call.Parameters("--route-parameters")
	id, _ := strconv.Atoi(route["pullRequestId"])
	index := b.pullRequestIndex(id)
	if index < 0 || repositoryIDOf(route["repositoryId"]) != b.pullRequests[index].RepositoryID {
		return nil, errors.New("ERROR: TF401180: The requested pull request was not found.")
	}
	return map[string]any{"value": slices.Clone(b.pullRequests[index].threads)}, nil
}
