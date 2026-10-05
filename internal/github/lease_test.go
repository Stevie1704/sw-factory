package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestRenewLeaseKeepsOneMilestoneAcrossManyIdleRenewals verifies more than
// 1,000 renewals edit one owned milestone and never write a Commit Status,
// even when the legacy lease status context is already at GitHub's cap.
func TestRenewLeaseKeepsOneMilestoneAcrossManyIdleRenewals(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	server.legacyStatusCapReached = true
	client := &github.GhClient{Runner: server}
	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	for renewal := 0; renewal < 1200; renewal++ {
		lease := testLease(renewed.Add(time.Duration(renewal) * 30 * time.Second))
		if err := client.RenewLease(context.Background(), testRepository, lease); err != nil {
			t.Fatalf("RenewLease() renewal %d error = %v", renewal, err)
		}
	}

	if len(server.milestones) != 1 {
		t.Fatalf("milestones = %d, want one bounded lease projection", len(server.milestones))
	}
	if server.statusWrites != 0 {
		t.Fatalf("Commit Status writes = %d, want none", server.statusWrites)
	}
	last := renewed.Add(1199 * 30 * time.Second)
	description := server.milestones[0].Description
	if !strings.Contains(description, "expires: "+last.Add(time.Minute).Format(time.RFC3339)) {
		t.Fatalf("lease description = %q, want the latest expiry", description)
	}
}

// TestRenewLeaseShowsLongIdentitiesAndCompleteTimestamps verifies long valid
// identifiers cannot truncate the renewal or expiry information.
func TestRenewLeaseShowsLongIdentitiesAndCompleteTimestamps(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	client := &github.GhClient{Runner: server}
	lease := testLease(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	lease.Coordinator = "host-" + strings.Repeat("c", 300)
	lease.RunID = "run-" + strings.Repeat("r", 200)

	if err := client.RenewLease(context.Background(), testRepository, lease); err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}

	description := server.milestones[0].Description
	for _, expected := range []string{
		"coordinator: " + lease.Coordinator,
		"run: " + lease.RunID,
		"renewed: 2026-08-26T12:00:00Z",
		"expires: 2026-08-26T12:01:00Z",
	} {
		if !strings.Contains(description, expected) {
			t.Errorf("lease description does not contain %q", expected)
		}
	}
	if server.milestones[0].State != "closed" || server.milestones[0].Title != github.LeaseMilestoneTitle {
		t.Fatalf("milestone = %#v, want the closed lease milestone", server.milestones[0])
	}
}

// TestRenewLeaseShowsAnIdleCoordinator verifies a renewal without a run says
// so explicitly instead of leaving an empty field.
func TestRenewLeaseShowsAnIdleCoordinator(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	client := &github.GhClient{Runner: server}
	lease := testLease(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	lease.RunID = ""

	if err := client.RenewLease(context.Background(), testRepository, lease); err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}
	if !strings.Contains(server.milestones[0].Description, "run: none") {
		t.Fatalf("lease description = %q, want run: none", server.milestones[0].Description)
	}
}

// TestRenewLeaseReusesTheMilestoneAfterRestart verifies a new adapter process
// discovers and edits the existing owned projection.
func TestRenewLeaseReusesTheMilestoneAfterRestart(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := (&github.GhClient{Runner: server}).RenewLease(context.Background(), testRepository, testLease(renewed)); err != nil {
		t.Fatalf("first RenewLease() error = %v", err)
	}
	if err := (&github.GhClient{Runner: server}).RenewLease(context.Background(), testRepository, testLease(renewed.Add(time.Hour))); err != nil {
		t.Fatalf("restarted RenewLease() error = %v", err)
	}

	if len(server.milestones) != 1 {
		t.Fatalf("milestones = %d, want the restarted adapter to reuse one", len(server.milestones))
	}
	if !strings.Contains(server.milestones[0].Description, "renewed: 2026-08-26T13:00:00Z") {
		t.Fatalf("lease description = %q, want the restarted renewal", server.milestones[0].Description)
	}
}

// TestRenewLeaseRecoversALostCreationResponse verifies a creation that GitHub
// accepted without returning a response is discovered, not duplicated.
func TestRenewLeaseRecoversALostCreationResponse(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	server.dropCreateResponse = true
	client := &github.GhClient{Runner: server}
	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed)); err == nil {
		t.Fatal("RenewLease() error = nil, want the lost response to surface as a retryable failure")
	}
	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed.Add(time.Minute))); err != nil {
		t.Fatalf("retried RenewLease() error = %v", err)
	}

	if len(server.milestones) != 1 || server.createCalls != 1 {
		t.Fatalf("milestones/creates = %d/%d, want one milestone and one creation", len(server.milestones), server.createCalls)
	}
	if !strings.Contains(server.milestones[0].Description, "renewed: 2026-08-26T12:01:00Z") {
		t.Fatalf("lease description = %q, want the retried renewal", server.milestones[0].Description)
	}
}

// TestRenewLeaseRecoversALostUpdateResponse verifies a renewal whose edit was
// applied without a response simply edits the same projection again.
func TestRenewLeaseRecoversALostUpdateResponse(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	client := &github.GhClient{Runner: server}
	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed)); err != nil {
		t.Fatalf("first RenewLease() error = %v", err)
	}
	server.dropUpdateResponse = true
	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed.Add(time.Minute))); err == nil {
		t.Fatal("RenewLease() error = nil, want the lost update response to surface")
	}
	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed.Add(2*time.Minute))); err != nil {
		t.Fatalf("retried RenewLease() error = %v", err)
	}

	if len(server.milestones) != 1 || server.createCalls != 1 {
		t.Fatalf("milestones/creates = %d/%d, want one reused milestone", len(server.milestones), server.createCalls)
	}
}

// TestRenewLeasePreservesHumanAuthoredText verifies an operator note outside
// the factory block survives renewal.
func TestRenewLeasePreservesHumanAuthoredText(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	client := &github.GhClient{Runner: server}
	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed)); err != nil {
		t.Fatalf("first RenewLease() error = %v", err)
	}
	server.milestones[0].Description = "Operator note above.\n\n" + server.milestones[0].Description + "\n\nOperator note below."

	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed.Add(time.Minute))); err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}

	description := server.milestones[0].Description
	if !strings.HasPrefix(description, "Operator note above.\n\n") || !strings.HasSuffix(description, "\n\nOperator note below.") {
		t.Fatalf("lease description = %q, want human text preserved", description)
	}
	if strings.Contains(description, "renewed: 2026-08-26T12:00:00Z") || !strings.Contains(description, "renewed: 2026-08-26T12:01:00Z") {
		t.Fatalf("lease description = %q, want only the latest renewal", description)
	}
}

// TestRenewLeaseLeavesForeignMilestonesUntouched verifies a milestone with the
// lease title is never adopted unless the coordinator account created it and
// it carries the factory block.
func TestRenewLeaseLeavesForeignMilestonesUntouched(t *testing.T) {
	t.Parallel()

	tests := map[string]milestoneRecord{
		"another author with a copied marker": {Creator: "alice", Description: "<!-- factory-lease:begin -->\nforged\n<!-- factory-lease:end -->"},
		"coordinator account without marker":  {Creator: "factory-bot", Description: "release planning"},
	}
	for name, existing := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := newMilestoneServer("factory-bot")
			existing.Number = 3
			existing.Title = github.LeaseMilestoneTitle
			existing.State = "open"
			server.milestones = []milestoneRecord{existing}
			client := &github.GhClient{Runner: server}

			err := client.RenewLease(context.Background(), testRepository, testLease(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)))
			if err == nil || !strings.Contains(err.Error(), "rename or delete") {
				t.Fatalf("RenewLease() error = %v, want an actionable ownership refusal", err)
			}
			if server.milestones[0] != existing || server.updateCalls != 0 || server.createCalls != 0 {
				t.Fatalf("milestones = %#v updates=%d creates=%d, want the foreign milestone untouched", server.milestones, server.updateCalls, server.createCalls)
			}
		})
	}
}

// TestRenewLeaseIgnoresUnrelatedMilestones verifies ordinary milestones are
// neither adopted nor edited.
func TestRenewLeaseIgnoresUnrelatedMilestones(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	unrelated := milestoneRecord{Number: 1, Title: "v1.0", State: "open", Creator: "factory-bot", Description: "<!-- factory-lease:begin -->\n<!-- factory-lease:end -->"}
	server.milestones = []milestoneRecord{unrelated}
	client := &github.GhClient{Runner: server}

	if err := client.RenewLease(context.Background(), testRepository, testLease(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}
	if len(server.milestones) != 2 || server.milestones[0] != unrelated {
		t.Fatalf("milestones = %#v, want the unrelated milestone untouched and one lease created", server.milestones)
	}
}

// TestRenewLeaseRediscoversADeletedMilestone verifies a cached projection that
// disappeared is replaced by exactly one new owned milestone in the same
// renewal.
func TestRenewLeaseRediscoversADeletedMilestone(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	client := &github.GhClient{Runner: server}
	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed)); err != nil {
		t.Fatalf("first RenewLease() error = %v", err)
	}
	server.milestones = nil

	if err := client.RenewLease(context.Background(), testRepository, testLease(renewed.Add(time.Minute))); err != nil {
		t.Fatalf("rediscovering RenewLease() error = %v", err)
	}
	if len(server.milestones) != 1 {
		t.Fatalf("milestones = %d, want one recreated projection", len(server.milestones))
	}
}

// TestRenewLeaseReturnsTransportFailures verifies a GitHub outage surfaces as
// an error so the coordinator applies its bounded lease backoff.
func TestRenewLeaseReturnsTransportFailures(t *testing.T) {
	t.Parallel()

	server := newMilestoneServer("factory-bot")
	server.unavailable = true
	client := &github.GhClient{Runner: server}

	err := client.RenewLease(context.Background(), testRepository, testLease(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)))
	if err == nil || !strings.Contains(err.Error(), "coordinator lease") {
		t.Fatalf("RenewLease() error = %v, want a diagnosable lease transport failure", err)
	}
}

// TestRenewLeaseRejectsInvalidValues verifies malformed leases never reach
// GitHub.
func TestRenewLeaseRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tests := map[string]github.Lease{
		"empty coordinator":     {Coordinator: " ", RenewedAt: renewed, ExpiresAt: renewed.Add(time.Minute)},
		"multiline coordinator": {Coordinator: "a\nb", RenewedAt: renewed, ExpiresAt: renewed.Add(time.Minute)},
		"expiry before renewal": {Coordinator: "host", RenewedAt: renewed, ExpiresAt: renewed},
	}
	for name, lease := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := newMilestoneServer("factory-bot")
			if err := (&github.GhClient{Runner: server}).RenewLease(context.Background(), testRepository, lease); err == nil {
				t.Fatal("RenewLease() error = nil, want validation failure")
			}
			if server.calls != 0 {
				t.Fatalf("GitHub calls = %d, want none", server.calls)
			}
		})
	}
}

// testRepository is the fake repository served by milestoneServer.
var testRepository = github.Repository{Owner: "example", Name: "project"}

// testLease returns a valid one-minute lease renewed at the supplied time.
func testLease(renewed time.Time) github.Lease {
	return github.Lease{Coordinator: "coordinator-test", RunID: "run-42", RenewedAt: renewed, ExpiresAt: renewed.Add(time.Minute)}
}

// milestoneRecord is the fake GitHub milestone state.
type milestoneRecord struct {
	Number      int
	Title       string
	State       string
	Description string
	Creator     string
}

// milestoneServer is a stateful fake gh runner for the milestone API. It also
// rejects legacy Commit Status writes when the status cap is simulated.
type milestoneServer struct {
	login                  string
	milestones             []milestoneRecord
	nextNumber             int
	calls                  int
	createCalls            int
	updateCalls            int
	statusWrites           int
	legacyStatusCapReached bool
	dropCreateResponse     bool
	dropUpdateResponse     bool
	unavailable            bool
}

// newMilestoneServer returns an empty repository authenticated as login.
func newMilestoneServer(login string) *milestoneServer {
	return &milestoneServer{login: login, nextNumber: 10}
}

// Run serves one gh api invocation against the fake milestone state.
func (s *milestoneServer) Run(_ context.Context, args []string, input []byte) ([]byte, error) {
	s.calls++
	if s.unavailable {
		return nil, errors.New("gh: connection refused")
	}
	if len(args) < 2 || args[0] != "api" {
		return nil, fmt.Errorf("unexpected gh invocation %v", args)
	}
	path, method := args[1], "GET"
	for index := range args {
		if args[index] == "--method" && index+1 < len(args) {
			method = args[index+1]
		}
	}
	const milestones = "repos/example/project/milestones"
	switch {
	case path == "user":
		return json.Marshal(map[string]string{"login": s.login})
	case strings.HasPrefix(path, "repos/example/project/statuses/"):
		s.statusWrites++
		if s.legacyStatusCapReached {
			return nil, errors.New("gh: This SHA and context has reached the maximum number of statuses. (HTTP 422)")
		}
		return []byte("{}"), nil
	case path == milestones && method == "GET":
		if !hasArgs(args, "--paginate", "--slurp", "state=all") {
			return nil, fmt.Errorf("milestone listing must include closed milestones: %v", args)
		}
		page := make([]any, 0, len(s.milestones))
		for _, record := range s.milestones {
			page = append(page, record.response())
		}
		return json.Marshal([]any{page})
	case path == milestones && method == "POST":
		return s.create(input)
	case strings.HasPrefix(path, milestones+"/"):
		return s.serveMilestone(strings.TrimPrefix(path, milestones+"/"), method, input)
	}
	return nil, fmt.Errorf("unexpected gh api %s %s", method, path)
}

// create stores one milestone, enforcing GitHub's unique-title rule.
func (s *milestoneServer) create(input []byte) ([]byte, error) {
	s.createCalls++
	var request map[string]string
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, err
	}
	for _, record := range s.milestones {
		if record.Title == request["title"] {
			return nil, errors.New("gh: Validation Failed: already_exists (HTTP 422)")
		}
	}
	record := milestoneRecord{Number: s.nextNumber, Title: request["title"], State: request["state"], Description: request["description"], Creator: s.login}
	s.nextNumber++
	s.milestones = append(s.milestones, record)
	if s.dropCreateResponse {
		s.dropCreateResponse = false
		return nil, errors.New("gh: unexpected EOF")
	}
	return json.Marshal(record.response())
}

// serveMilestone reads or edits one milestone by number.
func (s *milestoneServer) serveMilestone(number, method string, input []byte) ([]byte, error) {
	for index := range s.milestones {
		if fmt.Sprint(s.milestones[index].Number) != number {
			continue
		}
		if method == "PATCH" {
			s.updateCalls++
			var request map[string]string
			if err := json.Unmarshal(input, &request); err != nil {
				return nil, err
			}
			for key := range request {
				if key != "description" {
					return nil, fmt.Errorf("lease renewal must only edit the description, got %q", key)
				}
			}
			s.milestones[index].Description = request["description"]
			if s.dropUpdateResponse {
				s.dropUpdateResponse = false
				return nil, errors.New("gh: unexpected EOF")
			}
		}
		return json.Marshal(s.milestones[index].response())
	}
	return nil, errors.New("gh: Not Found (HTTP 404)")
}

// response renders the GitHub milestone API shape.
func (r milestoneRecord) response() map[string]any {
	return map[string]any{
		"number": r.Number, "title": r.Title, "state": r.State,
		"description": r.Description, "creator": map[string]string{"login": r.Creator},
	}
}
