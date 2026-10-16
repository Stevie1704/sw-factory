package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
)

// AzurePlanMapping maps the provider-neutral kinds of a plan to the work-item
// types of one Azure DevOps project. It is host configuration
// (`azure_devops.planning` of a registration), because the process of a
// project is operator-owned and differs between projects (ADR 0026).
type AzurePlanMapping struct {
	// Levels lists the backlog levels from the top level down, for example
	// Epic, Feature, and User Story. A plan item's parent must be on a higher
	// level than the item.
	Levels []AzurePlanLevel `yaml:"levels"`
}

// AzurePlanLevel is one backlog level of a plan mapping.
type AzurePlanLevel struct {
	// Type is the work-item type of the level, for example "User Story" or
	// "Product Backlog Item".
	Type string `yaml:"type"`
	// Kinds lists the neutral plan kinds that become items of this type.
	Kinds []string `yaml:"kinds,omitempty"`
	// Required makes the level mandatory between a parent and its child. When
	// a parent link skips a required level, the preview adds an intermediate
	// item of this type, so the approval binds it.
	Required bool `yaml:"required,omitempty"`
	// Fields holds field values, by reference name, that every item of this
	// type receives, for example a required custom field.
	Fields map[string]string `yaml:"fields,omitempty"`
}

// AgileAzurePlanMapping returns the mapping that applies when a registration
// configures none: the Agile process with a plan parent as an Epic, an
// optional Feature level, and plan tasks and investigations as User Stories.
func AgileAzurePlanMapping() AzurePlanMapping {
	return AzurePlanMapping{Levels: []AzurePlanLevel{
		{Type: "Epic", Kinds: []string{"parent"}},
		{Type: "Feature"},
		{Type: "User Story", Kinds: []string{"task", "investigation"}},
	}}
}

// adapterOwnedFields are the fields the adapter renders itself. A mapping must
// not set them: the title and the description come from the plan, and the
// tags carry only the correlation tag, never a factory state tag.
var adapterOwnedFields = []string{"System.Title", "System.Description", "System.Tags"}

// Validate reports the first structural problem of the mapping. Whether the
// types, fields, and states exist in the project is checked by PreflightPlan.
func (m AzurePlanMapping) Validate() error {
	if len(m.Levels) == 0 {
		return errors.New("must list at least one level")
	}
	types := map[string]bool{}
	kinds := map[string]bool{}
	for index, level := range m.Levels {
		if err := level.validate(); err != nil {
			return fmt.Errorf("levels[%d]: %w", index, err)
		}
		key := strings.ToLower(level.Type)
		if types[key] {
			return fmt.Errorf("levels[%d]: type %q repeats", index, level.Type)
		}
		types[key] = true
		for _, kind := range level.Kinds {
			if kinds[kind] {
				return fmt.Errorf("levels[%d]: kind %q is mapped twice", index, kind)
			}
			kinds[kind] = true
		}
	}
	if len(kinds) == 0 {
		return errors.New("must map at least one plan kind")
	}
	return nil
}

// validate checks one level.
func (l AzurePlanLevel) validate() error {
	if strings.TrimSpace(l.Type) == "" || l.Type != strings.TrimSpace(l.Type) || strings.IndexFunc(l.Type, unicode.IsControl) >= 0 {
		return errors.New("type must be a nonempty name without surrounding space or control characters")
	}
	for _, kind := range l.Kinds {
		if !safeKind(kind) {
			return fmt.Errorf("kind %q must be letters, digits, '.', '_', or '-'", kind)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(l.Fields)) {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/ ") {
			return fmt.Errorf("field %q must be a field reference name", name)
		}
		if slices.ContainsFunc(adapterOwnedFields, func(owned string) bool { return strings.EqualFold(owned, name) }) {
			return fmt.Errorf("field %q is rendered by the factory and cannot be configured", name)
		}
	}
	return nil
}

// safeKind reports whether kind has the shape of a plan kind identity.
func safeKind(kind string) bool {
	if kind == "" {
		return false
	}
	for _, character := range kind {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && !strings.ContainsRune("._-", character) {
			return false
		}
	}
	return true
}
