package runextensionapi

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// ListSessionFilesOptions selects one bounded page of direct workspace
// children. A zero Limit lets the Runtime Server apply its default page size.
type ListSessionFilesOptions struct {
	Directory string
	Limit     int
	PageToken string
}

// ListSessionFiles returns one bounded page of direct children below a
// workspace-relative path.
func (c *Client) ListSessionFiles(ctx context.Context, namespace, runName string, options *ListSessionFilesOptions) (*SessionFilePage, error) {
	if options == nil {
		options = &ListSessionFilesOptions{}
	}
	if options.Limit < 0 {
		return nil, errors.New("file page limit cannot be negative")
	}
	if err := requireRun(namespace, runName); err != nil {
		return nil, err
	}
	request := c.sessionFilesRequest(namespace, runName)
	if options.Directory != "" {
		request.Param("path", options.Directory)
	}
	if options.Limit > 0 {
		request.Param("limit", strconv.Itoa(options.Limit))
	}
	if options.PageToken != "" {
		request.Param("pageToken", options.PageToken)
	}
	var page SessionFilePage
	if err := c.execute(ctx, "GET", request.URL().String(), nil, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// ReadSessionFile returns bounded content at a workspace-relative path.
func (c *Client) ReadSessionFile(ctx context.Context, namespace, runName, path string, maxBytes int64) ([]byte, bool, error) {
	if path == "" {
		return nil, false, errors.New("Session file path is required")
	}
	if err := requireRun(namespace, runName); err != nil {
		return nil, false, err
	}
	request := c.sessionFilesRequest(namespace, runName)
	if maxBytes > 0 {
		request.Param("maxBytes", strconv.FormatInt(maxBytes, 10))
	}
	endpointURL := request.URL()
	// Append the workspace-relative path verbatim: path.Join would collapse ".."
	// so the Runtime could no longer reject a path that escapes the workspace.
	endpointURL.Path = strings.TrimRight(endpointURL.Path, "/") + "/" + path
	var response struct {
		Contents  []byte `json:"contents"`
		Truncated bool   `json:"truncated"`
	}
	if err := c.execute(ctx, "GET", endpointURL.String(), nil, nil, &response); err != nil {
		return nil, false, err
	}
	return response.Contents, response.Truncated, nil
}
