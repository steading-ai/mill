package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ivy/mill/internal/forge"
)

// contentFile is GitHub's answer to one file's contents, under the default
// branch since no ref is named.
type contentFile struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// ReadFile reads one file off the repository's default branch through the
// Contents API — no clone, no checkout. A repository with no such file
// answers 404, which is [forge.ErrNoSuchFile]; anything else GitHub refused
// is classified the way [Client.Probe] classifies a refused read, so a
// credential problem is reported as one.
func (c *Client) ReadFile(ctx context.Context, path string) (string, error) {
	var file contentFile
	err := c.call(ctx, http.MethodGet, c.path("contents", path), nil, nil, &file)
	var failure *apiError
	if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
		return "", forge.ErrNoSuchFile
	}
	if err != nil {
		return "", c.classify(err)
	}
	if file.Encoding != "base64" {
		return "", fmt.Errorf("github: %s: unexpected encoding %q", path, file.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("github: %s: decode content: %w", path, err)
	}
	return string(decoded), nil
}

// ContentReader is the interface this implements.
var _ forge.ContentReader = (*Client)(nil)
