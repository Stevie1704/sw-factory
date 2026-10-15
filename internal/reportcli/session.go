package reportcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/report"
)

// sessionIdentity is the coordinator-provided session ownership of a
// session-role worker. Both fields are empty for a run invocation.
type sessionIdentity struct {
	// id is the ideation session the invocation serves.
	id string
	// revision is the session revision the invocation was pinned to.
	revision int64
}

// sessionIdentityFrom reads FACTORY_SESSION_ID and FACTORY_SESSION_REVISION.
// A session ID requires a positive revision.
func sessionIdentityFrom(lookup func(string) string) (sessionIdentity, error) {
	id := strings.TrimSpace(lookup("FACTORY_SESSION_ID"))
	if id == "" {
		return sessionIdentity{}, nil
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(lookup("FACTORY_SESSION_REVISION")), 10, 64)
	if err != nil || revision < 1 {
		return sessionIdentity{}, errors.New("FACTORY_SESSION_REVISION environment value must be a positive integer")
	}
	return sessionIdentity{id: id, revision: revision}, nil
}

// checkSessionPayloadFlags allows --ideation-file only for the ideation role
// and --plan-file only for the planning role of a session invocation.
func checkSessionPayloadFlags(session bool, role, ideationFile, planFile string) error {
	if ideationFile != "" && (!session || role != "ideation") {
		return errors.New("--ideation-file is only valid for the ideation session role")
	}
	if planFile != "" && (!session || role != "planning") {
		return errors.New("--plan-file is only valid for the planning session role")
	}
	return nil
}

// readSessionPayloads decodes the optional ideation and plan payload files
// into value. Payloads travel in files because a brief or a plan graph does
// not fit command-line flags.
func readSessionPayloads(value *report.Report, ideationFile, planFile string) error {
	if ideationFile != "" {
		value.Ideation = &report.IdeationHandoff{}
		if err := readJSONPayload(ideationFile, value.Ideation); err != nil {
			return fmt.Errorf("read ideation file: %w", err)
		}
	}
	if planFile != "" {
		value.PlanDraft = &report.PlanDraft{}
		if err := readJSONPayload(planFile, value.PlanDraft); err != nil {
			return fmt.Errorf("read plan file: %w", err)
		}
	}
	return nil
}

// readJSONPayload decodes exactly one bounded JSON value with no unknown
// fields from path into target.
func readJSONPayload(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, report.MaxReportBytes+1))
	if err != nil {
		return err
	}
	if len(data) > report.MaxReportBytes {
		return fmt.Errorf("payload exceeds %d bytes", report.MaxReportBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("payload contains more than one JSON value")
	}
	return nil
}
