package vulnscan

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// reviewDateLayout is the calendar-date format of a suppression's review_by.
const reviewDateLayout = "2006-01-02"

// advisoryPattern accepts only concrete advisory identifiers, so a
// suppression can never name a whole package ecosystem.
var advisoryPattern = regexp.MustCompile(`^(GO-\d{4}-\d+|CVE-\d{4}-\d+|GHSA(-[23456789cfghjmpqrvwx]{4}){3})$`)

// packagePattern rejects glob characters in a suppression's package.
var packagePattern = regexp.MustCompile(`^[^*?\[\]\s]+$`)

// Suppression accepts one reviewed advisory for one package of one artifact
// until its review date.
type Suppression struct {
	Advisory string `yaml:"advisory" json:"advisory"`
	Artifact string `yaml:"artifact" json:"artifact"`
	Package  string `yaml:"package" json:"package"`
	Reason   string `yaml:"reason" json:"reason"`
	Owner    string `yaml:"owner" json:"owner"`
	ReviewBy string `yaml:"review_by" json:"review_by"`
}

// suppressionFile is the reviewed suppression policy document.
type suppressionFile struct {
	SchemaVersion int           `yaml:"schema_version"`
	Suppressions  []Suppression `yaml:"suppressions"`
}

// LoadSuppressions reads and validates a suppression policy document.
func LoadSuppressions(r io.Reader) ([]Suppression, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	var file suppressionFile
	if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse suppressions: %w", err)
	}
	if file.SchemaVersion != 1 {
		return nil, fmt.Errorf("suppressions schema_version must be 1, got %d", file.SchemaVersion)
	}
	for index, suppression := range file.Suppressions {
		if err := suppression.validate(); err != nil {
			return nil, fmt.Errorf("suppression %d: %w", index+1, err)
		}
	}
	return file.Suppressions, nil
}

// validate requires every field that makes a suppression narrow and
// reviewable.
func (s Suppression) validate() error {
	if !advisoryPattern.MatchString(s.Advisory) {
		return fmt.Errorf("advisory %q is not a GO, CVE, or GHSA identifier", s.Advisory)
	}
	if s.Artifact == "" {
		return errors.New("artifact is required")
	}
	if !packagePattern.MatchString(s.Package) {
		return fmt.Errorf("package %q must name exactly one package", s.Package)
	}
	if s.Reason == "" {
		return errors.New("reason is required")
	}
	if s.Owner == "" {
		return errors.New("owner is required")
	}
	if _, err := time.Parse(reviewDateLayout, s.ReviewBy); err != nil {
		return fmt.Errorf("review_by %q must be a YYYY-MM-DD date", s.ReviewBy)
	}
	return nil
}

// expired reports whether today is after the suppression's review date.
func (s Suppression) expired(today time.Time) bool {
	reviewBy, err := time.Parse(reviewDateLayout, s.ReviewBy)
	if err != nil {
		return true
	}
	return today.UTC().Format(reviewDateLayout) > reviewBy.Format(reviewDateLayout)
}
