package krt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/klog/v2"

	"github.com/kruntimes/kruntimes/internal/logapi"
)

const (
	defaultRunLogTailLines = 100
	maxRunLogTailLines     = 500
)

type runLogEntry struct {
	Stream  string `json:"stream"`
	Message string `json:"message"`
}

func newLogsCmd(getter genericclioptions.RESTClientGetter, _ *runtime.Scheme) *cobra.Command {
	var (
		follow    bool
		tailLines int
	)

	cmd := &cobra.Command{
		Use:   "logs <run-name>",
		Short: "Show logs from a Run through the Kubernetes API.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tailLines < 1 || tailLines > maxRunLogTailLines {
				return fmt.Errorf("tail must be an integer from 1 through %d", maxRunLogTailLines)
			}
			restConfig, err := restConfigFromConfig(getter)
			if err != nil {
				return err
			}
			namespace := namespaceFromConfig(getter)
			runName := args[0]
			klog.V(2).InfoS("using Kubernetes aggregated Run log API")
			logs, err := logapi.NewClient(restConfig)
			if err != nil {
				return err
			}
			if follow {
				return followAggregatedRunLogs(cmd.Context(), logs, namespace, runName, tailLines, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			raw, err := logs.GetLogs(namespace, runName, &logapi.RunLogOptions{TailLines: int64(tailLines)}).Do(cmd.Context()).Raw()
			if err != nil {
				return err
			}
			return writeSnapshotRunLogs(bytes.NewReader(raw), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVar(&tailLines, "tail", defaultRunLogTailLines, "Number of recent structured log records to show (1-500)")
	return cmd
}

// followAggregatedRunLogs reconnects the intentionally time-bounded
// aggregation stream. The opaque cursor means a reconnect resumes after the
// last delivered record instead of replaying the initial tail.
func followAggregatedRunLogs(ctx context.Context, api logapi.Client, namespace, runName string, tailLines int, stdout, stderr io.Writer) error {
	cursor := ""
	for {
		response, err := api.GetLogs(namespace, runName, &logapi.RunLogOptions{TailLines: int64(tailLines), Follow: true, Cursor: cursor}).Stream(ctx)
		if err != nil {
			return err
		}
		next, err := writeFollowRunLogsWithCursor(response, stdout, stderr)
		_ = response.Close()
		if err != nil {
			return err
		}
		if next != "" {
			cursor = next
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func writeSnapshotRunLogs(reader io.Reader, stdout, stderr io.Writer) error {
	var response struct {
		Items []runLogEntry `json:"items"`
	}
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		return fmt.Errorf("decode Run log API response: %w", err)
	}
	return writeRunLogEntries(response.Items, stdout, stderr)
}

func writeFollowRunLogs(reader io.Reader, stdout, stderr io.Writer) error {
	_, err := writeFollowRunLogsWithCursor(reader, stdout, stderr)
	return err
}

func writeFollowRunLogsWithCursor(reader io.Reader, stdout, stderr io.Writer) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	cursor := ""
	for scanner.Scan() {
		var entry struct {
			runLogEntry
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return "", fmt.Errorf("decode Run log API record: %w", err)
		}
		if err := writeRunLogEntries([]runLogEntry{entry.runLogEntry}, stdout, stderr); err != nil {
			return "", err
		}
		if entry.Cursor != "" {
			cursor = entry.Cursor
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read Run log API stream: %w", err)
	}
	return cursor, nil
}

func writeRunLogEntries(entries []runLogEntry, stdout, stderr io.Writer) error {
	for _, entry := range entries {
		var output io.Writer
		switch entry.Stream {
		case "stdout":
			output = stdout
		case "stderr":
			output = stderr
		default:
			continue
		}
		if err := writeLogOutput(output, entry.Message); err != nil {
			return fmt.Errorf("write %s: %w", entry.Stream, err)
		}
	}
	return nil
}

func writeLogOutput(w io.Writer, output string) error {
	if output == "" {
		return nil
	}
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	_, err := io.WriteString(w, output)
	return err
}
