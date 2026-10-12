package cli

import (
	"context"
	"errors"
	"io"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// configValidateSchema is the version of the config validate document.
// Increment it when a field changes meaning or is removed.
const configValidateSchema = 1

// configValidateDocument is the JSON result of config validate.
type configValidateDocument struct {
	SchemaVersion int    `json:"schema_version"`
	File          string `json:"file"`
	config.RepositoryDiagnosis
}

// runConfig dispatches the config subcommands. It needs no host
// configuration.
func runConfig(_ context.Context, args []string, _ string, output, errorsOutput io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		writeError(errorsOutput, errors.New("config needs a subcommand: validate"))
		return 2
	}
	return runConfigValidate(args[1:], output, errorsOutput)
}

// runConfigValidate validates one repository configuration file with the
// production parser and validator. It exits 0 for a valid file and 1 for an
// invalid one. It needs no host configuration, Docker, tracker, or network.
func runConfigValidate(args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("config validate", errorsOutput)
	file := flags.String("file", "", "repository configuration file to validate, such as factory.yaml")
	asJSON := flags.Bool("json", false, "write the diagnosis as one schema-versioned JSON document; required")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("config validate does not accept positional arguments"))
		return 2
	}
	if *file == "" || !*asJSON {
		writeError(errorsOutput, errors.New("config validate needs --file <factory.yaml> and --json"))
		return 2
	}
	diagnosis := config.DiagnoseRepositoryFile(*file)
	if !writeJSONDocument(output, errorsOutput, configValidateDocument{SchemaVersion: configValidateSchema, File: *file, RepositoryDiagnosis: diagnosis}) {
		return 1
	}
	if !diagnosis.Valid {
		return 1
	}
	return 0
}
