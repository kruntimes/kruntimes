package dashboard

import (
	"context"
	"errors"
	"net/http"

	"github.com/kruntimes/kruntimes/internal/logapi"
)

// RunLogClient reads one Run's logs through the Kubernetes aggregation API.
// Dashboard forwards the caller token only to the API server.
type RunLogClient interface {
	RunLogs(context.Context, string, string, string, int64, bool) (*http.Response, error)
}

type AggregatedRunLogClient struct {
	client *logapi.RESTClient
}

func NewAggregatedRunLogClient(client *logapi.RESTClient) (*AggregatedRunLogClient, error) {
	if client == nil {
		return nil, errors.New("Dashboard aggregated Run log API client is not configured")
	}
	return &AggregatedRunLogClient{client: client}, nil
}

func (c *AggregatedRunLogClient) RunLogs(ctx context.Context, token, namespace, runName string, tailLines int64, follow bool) (*http.Response, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("Dashboard aggregated Run log API client is not configured")
	}
	stream, err := c.client.WithBearerToken(token).GetLogs(namespace, runName, &logapi.RunLogOptions{
		TailLines: tailLines,
		Follow:    follow,
	}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	contentType := "application/json"
	if follow {
		contentType = "application/x-ndjson"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: stream}, nil
}
