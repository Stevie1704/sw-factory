package azuredevops

import (
	"context"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// requireExtensionPermission refuses a denied or unobservable effective
// permission. The CLI helper evaluates the caller, including inherited
// permissions, without enabling the administrator bypass.
func (c *Client) requireExtensionPermission(ctx context.Context, organization string, required permission, token string) error {
	granted, err := c.extensionPermission(ctx, organization, required, token)
	if err != nil {
		return fmt.Errorf("evaluate Azure DevOps permissions: %w", classifyPlanError(err))
	}
	if !granted {
		return tracker.NewPlanError(tracker.PlanErrorPermission, "%s", required.missing)
	}
	return nil
}

// extensionAreaPermission checks the actual area selected by an item's
// configured fields. Checking only the root would miss child-area denials.
func (c *Client) extensionAreaPermission(ctx context.Context, repository tracker.Repository, areaPath string) error {
	route := projectRoute(repository, "structureGroup", "areas")
	if areaPath != "" {
		parts := strings.Split(areaPath, `\`)
		if !strings.EqualFold(parts[0], repository.Project) {
			return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "area path %q is outside project %s", areaPath, repository.Project)
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." || !plainSegment(part) {
				return tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "area path %q contains an invalid segment", areaPath)
			}
		}
		if len(parts) > 1 {
			route["path"] = strings.Join(parts[1:], "/")
		}
	}
	var area struct {
		Identifier string `json:"identifier"`
	}
	if err := c.invoke(ctx, repository.Owner, invokeRequest{Area: "wit", Resource: "classificationNodes", APIVersion: extensionAPIVersion, Route: route}, &area); err != nil {
		return fmt.Errorf("read area %q: %w", areaPath, classifyPlanError(err))
	}
	if !canonicalGUID.MatchString(area.Identifier) {
		return fmt.Errorf("area %q has no valid identifier", areaPath)
	}
	required := workItemPermission
	required.missing = fmt.Sprintf("the identity cannot view and edit work items in area %q of project %s", areaPath, repository.Project)
	return c.requireExtensionPermission(ctx, repository.Owner, required, "vstfs:///Classification/Node/"+area.Identifier)
}

// extensionPlanPermissions checks every distinct destination area and the
// ability to create the plan's new correlation/assignment tags before either
// approval or publication can proceed.
func (c *Client) extensionPlanPermissions(ctx context.Context, repository tracker.Repository, items []tracker.RenderedPlanItem) error {
	identity, err := c.ResolveRepository(ctx, repository)
	if err != nil {
		return classifyPlanError(err)
	}
	areas := map[string]bool{}
	for _, item := range items {
		area := ""
		for name, value := range item.Fields {
			if strings.EqualFold(name, "System.AreaPath") {
				area = value
			}
		}
		key := strings.ToLower(area)
		if !areas[key] {
			if err := c.extensionAreaPermission(ctx, repository, area); err != nil {
				return err
			}
			areas[key] = true
		}
	}
	required := tagCreatePermission
	required.missing = "the identity cannot create the correlation and repository assignment tag definitions"
	return c.requireExtensionPermission(ctx, repository.Owner, required, "/"+identity.ProjectID)
}

// extensionGitPermissions validates repository contribution, branch creation,
// and PR contribution without requiring any Boards access.
func (c *Client) extensionGitPermissions(ctx context.Context, repository tracker.Repository) error {
	identity, err := c.ResolveRepository(ctx, repository)
	if err != nil {
		return err
	}
	token := "repoV2/" + identity.ProjectID + "/" + identity.ID
	for _, required := range gitPermissions {
		if err := c.requireExtensionPermission(ctx, repository.Owner, required, token); err != nil {
			return err
		}
	}
	return nil
}
