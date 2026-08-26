// Package ref contains validation shared by host adapters that pass Git refs
// to external commands or APIs.
package ref

import (
	"fmt"
	"strings"
)

// ValidatePart rejects ref fragments that are ambiguous or unsafe when they
// become arguments or path fragments in a Git operation.
func ValidatePart(value string) error {
	if strings.ContainsAny(value, "\r\n") || strings.HasPrefix(value, "-") || strings.Contains(value, "..") || strings.ContainsAny(value, " ~^:?*[\\") {
		return fmt.Errorf("contains characters that cannot be used safely: %q", value)
	}
	return nil
}
