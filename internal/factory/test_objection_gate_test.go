package factory

import (
	"context"
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestAutomatedTestObjectionGateRequiresTheLatestAuthorizedProceedDecision
// verifies that a configuration flag cannot bypass the measured-pilot gate.
func TestAutomatedTestObjectionGateRequiresTheLatestAuthorizedProceedDecision(t *testing.T) {
	reader := &pilotGateCommentReader{comments: []github.Comment{
		{ID: "2", Author: "alice", Body: "Decision: stop"},
		{ID: "1", Author: "alice", Body: "Decision: proceed"},
		{ID: "3", Author: "bob", Body: "Decision: proceed"},
	}}
	service := &Service{deps: Dependencies{Comments: reader}}
	registration := config.RepositoryRegistration{
		GitHub:          config.GitHubConfig{Owner: "example", Repository: "factory"},
		AuthorizedUsers: []string{"alice"},
	}
	packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{
		TestPolicy: config.TestPolicy{AllowAutomatedObjections: true},
	}}

	allowed, reason := service.automatedTestObjectionGate(context.Background(), registration, packet)
	if allowed || reason == "" {
		t.Fatalf("gate with unauthorized/latest revise decision = allowed=%v reason=%q, want denied with reason", allowed, reason)
	}

	reader.comments = append(reader.comments, github.Comment{ID: "4", Author: "alice", Body: "<!-- factory-pilot-decision: proceed -->"})
	allowed, reason = service.automatedTestObjectionGate(context.Background(), registration, packet)
	if !allowed || reason != "" {
		t.Fatalf("gate with authorized proceed decision = allowed=%v reason=%q, want allowed", allowed, reason)
	}

	reader.comments = append(reader.comments, github.Comment{ID: "5", Author: "alice", Body: "Decision: stop"})
	allowed, reason = service.automatedTestObjectionGate(context.Background(), registration, packet)
	if allowed || reason == "" {
		t.Fatalf("gate after later stop decision = allowed=%v reason=%q, want denied", allowed, reason)
	}
}

// TestAutomatedTestObjectionGateFailsClosedWhenDecisionCannotBeRead verifies
// missing and failing comment readers keep the objection human-supervised.
func TestAutomatedTestObjectionGateFailsClosedWhenDecisionCannotBeRead(t *testing.T) {
	registration := config.RepositoryRegistration{AuthorizedUsers: []string{"alice"}}
	packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{
		TestPolicy: config.TestPolicy{AllowAutomatedObjections: true},
	}}

	service := &Service{deps: Dependencies{Comments: &pilotGateCommentReader{err: errors.New("unavailable")}}}
	allowed, reason := service.automatedTestObjectionGate(context.Background(), registration, packet)
	if allowed || reason == "" {
		t.Fatalf("gate with comment reader error = allowed=%v reason=%q, want denied", allowed, reason)
	}

	service = &Service{}
	allowed, reason = service.automatedTestObjectionGate(context.Background(), registration, packet)
	if allowed || reason == "" {
		t.Fatalf("gate without comment reader = allowed=%v reason=%q, want denied", allowed, reason)
	}
}

// pilotGateCommentReader is the bounded comment reader used by pilot-gate
// tests to model GitHub's stable issue-comment projection.
type pilotGateCommentReader struct {
	comments []github.Comment
	err      error
}

// IssueComments returns the configured pilot decision comments.
func (r *pilotGateCommentReader) IssueComments(context.Context, github.Repository, int) ([]github.Comment, error) {
	if r.err != nil {
		return nil, r.err
	}
	return append([]github.Comment(nil), r.comments...), nil
}
