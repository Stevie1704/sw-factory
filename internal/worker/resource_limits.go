package worker

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Default resource limits apply to every limit the host configuration omits,
// so no worker container ever starts without a bound.
const (
	// DefaultMemoryLimit is the worker memory limit. Swap equals it, so a
	// worker cannot extend its memory with swap.
	DefaultMemoryLimit = "8g"
	// DefaultCPULimit is the number of host CPUs a worker may use.
	DefaultCPULimit = "4"
	// DefaultPIDLimit is the most processes and threads a worker may hold.
	DefaultPIDLimit = "4096"
	// DefaultLogMaxSize is the size of one container log file.
	DefaultLogMaxSize = "10m"
	// DefaultLogMaxFiles is the number of rotated container log files.
	DefaultLogMaxFiles = "3"
	// minimumMemoryBytes is the smallest memory limit Docker accepts.
	minimumMemoryBytes = 6 << 20
)

// sizePattern is the Docker byte-size grammar the factory accepts: a positive
// whole number with an optional b, k, m, or g unit.
var sizePattern = regexp.MustCompile(`^([0-9]+)([bkmgBKMG]?)$`)

// cpuPattern accepts a decimal CPU count such as 4 or 1.5.
var cpuPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// ResourceLimits are the host-owned bounds applied to one worker container.
// The values use the Docker CLI grammar. An empty value selects its default.
type ResourceLimits struct {
	// Memory is the memory limit, for example 8g. Swap equals it.
	Memory string
	// CPUs is the CPU limit, for example 4 or 1.5.
	CPUs string
	// PIDs is the process and thread limit.
	PIDs string
	// LogMaxSize is the size of one json-file container log, for example 10m.
	LogMaxSize string
	// LogMaxFiles is the number of rotated container log files.
	LogMaxFiles string
}

// ResourceLimitError names one invalid resource limit. Field uses the host
// configuration name so a caller can report a field-level error.
type ResourceLimitError struct {
	// Field is the host configuration name of the invalid limit.
	Field string
	// Message describes the accepted values.
	Message string
}

// Error reports the field and the accepted values.
func (e *ResourceLimitError) Error() string {
	return fmt.Sprintf("worker limit %s %s", e.Field, e.Message)
}

// Effective returns the limits with the documented default in place of every
// omitted value.
func (l ResourceLimits) Effective() ResourceLimits {
	return ResourceLimits{
		Memory:      valueOrDefault(l.Memory, DefaultMemoryLimit),
		CPUs:        valueOrDefault(l.CPUs, DefaultCPULimit),
		PIDs:        valueOrDefault(l.PIDs, DefaultPIDLimit),
		LogMaxSize:  valueOrDefault(l.LogMaxSize, DefaultLogMaxSize),
		LogMaxFiles: valueOrDefault(l.LogMaxFiles, DefaultLogMaxFiles),
	}
}

// Validate rejects zero, negative, and unparseable limits. Omitted values are
// valid because they select their defaults.
func (l ResourceLimits) Validate() error {
	if l.Memory != "" {
		if size, ok := parseSize(l.Memory); !ok || size < minimumMemoryBytes {
			return &ResourceLimitError{Field: "memory", Message: "must be a size of at least 6m, such as 8g"}
		}
	}
	if l.CPUs != "" {
		if count, err := strconv.ParseFloat(l.CPUs, 64); !cpuPattern.MatchString(l.CPUs) || err != nil || count <= 0 {
			return &ResourceLimitError{Field: "cpus", Message: "must be a positive CPU count, such as 4 or 1.5"}
		}
	}
	if l.PIDs != "" && !positiveInteger(l.PIDs) {
		return &ResourceLimitError{Field: "pids", Message: "must be a positive whole number"}
	}
	if l.LogMaxSize != "" {
		if size, ok := parseSize(l.LogMaxSize); !ok || size <= 0 {
			return &ResourceLimitError{Field: "log_max_size", Message: "must be a positive size, such as 10m"}
		}
	}
	if l.LogMaxFiles != "" && !positiveInteger(l.LogMaxFiles) {
		return &ResourceLimitError{Field: "log_max_files", Message: "must be a positive whole number"}
	}
	return nil
}

// String renders the effective limits as one operator-readable summary.
func (l ResourceLimits) String() string {
	effective := l.Effective()
	return fmt.Sprintf("memory %s, swap %s, cpus %s, pids %s, log %s x %s", effective.Memory, effective.Memory, effective.CPUs, effective.PIDs, effective.LogMaxFiles, effective.LogMaxSize)
}

// dockerArguments returns the docker run flags that apply the effective limits.
func (l ResourceLimits) dockerArguments() []string {
	effective := l.Effective()
	return []string{
		"--memory", effective.Memory,
		"--memory-swap", effective.Memory,
		"--cpus", effective.CPUs,
		"--pids-limit", effective.PIDs,
		"--log-driver", "json-file",
		"--log-opt", "max-size=" + effective.LogMaxSize,
		"--log-opt", "max-file=" + effective.LogMaxFiles,
	}
}

// valueOrDefault returns value, or fallback when value is empty.
func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// parseSize converts one Docker byte size to bytes. It reports false for an
// unparseable value or a value that overflows.
func parseSize(value string) (int64, bool) {
	match := sizePattern.FindStringSubmatch(value)
	if match == nil {
		return 0, false
	}
	number, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, false
	}
	unit := map[string]int64{"": 1, "b": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30}[strings.ToLower(match[2])]
	if number > math.MaxInt64/unit {
		return 0, false
	}
	return number * unit, true
}

// positiveInteger reports whether value is a whole number greater than zero.
func positiveInteger(value string) bool {
	number, err := strconv.Atoi(value)
	return err == nil && number > 0 && strconv.Itoa(number) == value
}
