package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// LeaseMilestoneTitle is the repository-unique title of the closed milestone
// whose description projects the coordinator lease. GitHub enforces unique
// milestone titles, so at most one lease projection can exist per repository.
const LeaseMilestoneTitle = "factory coordinator lease"

const (
	// leaseBlockBegin opens the factory-owned part of the milestone description.
	leaseBlockBegin = "<!-- factory-lease:begin -->"
	// leaseBlockEnd closes the factory-owned part of the milestone description.
	leaseBlockEnd = "<!-- factory-lease:end -->"
)

var _ tracker.LeaseClient = (*GhClient)(nil)

// leaseCache keeps the coordinator login and the owned milestone number so a
// steady-state renewal costs one read and one edit.
type leaseCache struct {
	mu        sync.Mutex
	login     string
	milestone int
}

// RenewLease rewrites the factory block of the owned lease milestone. The
// first renewal creates the closed milestone; later renewals, including those
// after a restart or a lost response, discover and edit the same milestone.
// Text outside the factory block is preserved. A stale expiry stays visible
// after the host process disappears.
func (c *GhClient) RenewLease(ctx context.Context, repository tracker.Repository, lease tracker.Lease) error {
	if err := validateLease(lease); err != nil {
		return err
	}
	c.lease.mu.Lock()
	defer c.lease.mu.Unlock()
	if err := c.renewLease(ctx, repository, lease); err != nil {
		c.lease.milestone = 0
		return fmt.Errorf("renew coordinator lease: %w", err)
	}
	return nil
}

// renewLease edits the cached milestone, or discovers or creates it.
func (c *GhClient) renewLease(ctx context.Context, repository tracker.Repository, lease tracker.Lease) error {
	login, err := c.leaseLogin(ctx)
	if err != nil {
		return err
	}
	milestone, err := c.ownedLeaseMilestone(ctx, repository, login)
	if err != nil {
		return err
	}
	if milestone.Number == 0 {
		return c.createLeaseMilestone(ctx, repository, lease)
	}
	c.lease.milestone = milestone.Number
	description := replaceLeaseBlock(milestone.Description, leaseBlock(lease))
	path := fmt.Sprintf("repos/%s/milestones/%d", repository.String(), milestone.Number)
	if err := c.call(ctx, []string{"api", path, "--method", "PATCH"}, map[string]string{"description": description}); err != nil {
		return fmt.Errorf("edit lease milestone #%d: %w", milestone.Number, err)
	}
	return nil
}

// leaseLogin returns the cached coordinator account login.
func (c *GhClient) leaseLogin(ctx context.Context) (string, error) {
	if c.lease.login == "" {
		login, err := c.AuthenticatedLogin(ctx)
		if err != nil {
			return "", err
		}
		c.lease.login = login
	}
	return c.lease.login, nil
}

// ownedLeaseMilestone returns the owned lease milestone, or a zero milestone
// when none exists yet. It refuses a lease-titled milestone that another
// author created or that lacks the factory block.
func (c *GhClient) ownedLeaseMilestone(ctx context.Context, repository tracker.Repository, login string) (milestoneResponse, error) {
	if c.lease.milestone != 0 {
		var milestone milestoneResponse
		path := fmt.Sprintf("repos/%s/milestones/%d", repository.String(), c.lease.milestone)
		// A cached milestone that was deleted, renamed, or changed falls back
		// to discovery in the same renewal.
		err := c.callJSON(ctx, []string{"api", path, "--method", "GET"}, nil, &milestone)
		if err == nil && milestone.Title == LeaseMilestoneTitle && milestone.adoptableBy(login) {
			return milestone, nil
		}
	}
	output, err := c.callBytes(ctx, []string{"api", fmt.Sprintf("repos/%s/milestones", repository.String()), "--method", "GET", "--paginate", "--slurp", "-f", "state=all"}, nil)
	if err != nil {
		return milestoneResponse{}, fmt.Errorf("list milestones: %w", err)
	}
	milestones, err := decodeJSONPages[milestoneResponse](output)
	if err != nil {
		return milestoneResponse{}, fmt.Errorf("decode milestones: %w", err)
	}
	for _, milestone := range milestones {
		if milestone.Title != LeaseMilestoneTitle {
			continue
		}
		if err := milestone.adoptionError(login); err != nil {
			return milestoneResponse{}, err
		}
		return milestone, nil
	}
	return milestoneResponse{}, nil
}

// createLeaseMilestone creates the closed lease milestone. A lost response is
// returned as an error; the next renewal discovers the created milestone.
func (c *GhClient) createLeaseMilestone(ctx context.Context, repository tracker.Repository, lease tracker.Lease) error {
	var milestone milestoneResponse
	payload := map[string]string{"title": LeaseMilestoneTitle, "state": "closed", "description": leaseBlock(lease)}
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/milestones", repository.String()), "--method", "POST"}, payload, &milestone); err != nil {
		return fmt.Errorf("create lease milestone: %w", err)
	}
	c.lease.milestone = milestone.Number
	return nil
}

// validateLease rejects values that cannot form a readable lease block.
func validateLease(lease tracker.Lease) error {
	if strings.TrimSpace(lease.Coordinator) == "" || strings.ContainsAny(lease.Coordinator, "\x00\r\n") {
		return errors.New("lease coordinator is required and must be a single line")
	}
	if lease.RenewedAt.IsZero() || lease.ExpiresAt.IsZero() || !lease.ExpiresAt.After(lease.RenewedAt) {
		return errors.New("lease heartbeat and expiry must be valid")
	}
	return nil
}

// leaseBlock renders the complete, untruncated factory-owned lease block.
func leaseBlock(lease tracker.Lease) string {
	run := singleLine(lease.RunID)
	if run == "" {
		run = "none"
	}
	return strings.Join([]string{
		leaseBlockBegin,
		"The factory coordinator rewrites this block on every renewal. A past expiry means the coordinator stopped renewing.",
		"",
		"coordinator: " + singleLine(lease.Coordinator),
		"run: " + run,
		"renewed: " + lease.RenewedAt.UTC().Format(time.RFC3339),
		"expires: " + lease.ExpiresAt.UTC().Format(time.RFC3339),
		leaseBlockEnd,
	}, "\n")
}

// replaceLeaseBlock swaps the factory block inside description and keeps the
// surrounding human-authored text unchanged.
func replaceLeaseBlock(description, block string) string {
	begin, end, ok := leaseBlockBounds(description)
	if !ok {
		return block
	}
	return description[:begin] + block + description[end:]
}

// leaseBlockBounds returns the byte range of the factory block.
func leaseBlockBounds(description string) (int, int, bool) {
	begin := strings.Index(description, leaseBlockBegin)
	if begin < 0 {
		return 0, 0, false
	}
	end := strings.Index(description[begin:], leaseBlockEnd)
	if end < 0 {
		return 0, 0, false
	}
	return begin, begin + end + len(leaseBlockEnd), true
}

// singleLine bounds an untrusted lease value to one line.
func singleLine(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\x00", " ").Replace(value))
}

// milestoneResponse is the GitHub milestone projection needed by the lease.
type milestoneResponse struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Creator     struct {
		Login string `json:"login"`
	} `json:"creator"`
}

// adoptableBy reports whether the coordinator account created the milestone
// and the factory block is present. A copied block alone never proves
// ownership.
func (m milestoneResponse) adoptableBy(login string) bool {
	return m.adoptionError(login) == nil
}

// adoptionError explains, with a corrective action, why the coordinator must
// not adopt a milestone that has the lease title.
func (m milestoneResponse) adoptionError(login string) error {
	if !strings.EqualFold(strings.TrimSpace(m.Creator.Login), login) {
		return fmt.Errorf("milestone #%d %q was created by %q, not by the coordinator account %q: rename or delete it so the coordinator can create its own", m.Number, LeaseMilestoneTitle, m.Creator.Login, login)
	}
	if _, _, hasBlock := leaseBlockBounds(m.Description); !hasBlock {
		return fmt.Errorf("milestone #%d %q has no factory lease block: restore the block, or rename or delete the milestone so the coordinator can create its own", m.Number, LeaseMilestoneTitle)
	}
	return nil
}
