package github

import (
	"context"
	"errors"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// errAccountUnavailable is the bounded diagnosis returned when the
// authenticated GitHub account cannot be established. It never contains gh
// output, so no credential material can reach an operator message.
var errAccountUnavailable = errors.New("the authenticated GitHub account could not be read: run gh auth login for the account that supervises the repository")

// AuthenticatedLogin returns the login of the account gh is authenticated as.
func (c *GhClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	var response accountResponse
	if err := c.callJSON(ctx, []string{"api", "user"}, nil, &response); err != nil {
		return "", errAccountUnavailable
	}
	login := strings.TrimSpace(response.Login)
	if login == "" || strings.ContainsAny(login, "\x00\r\n/") {
		return "", errAccountUnavailable
	}
	return login, nil
}

// accountResponse is the credential-free account projection used to resolve
// the default authorized user.
type accountResponse struct {
	Login string `json:"login"`
}

var _ tracker.AccountReader = (*GhClient)(nil)
