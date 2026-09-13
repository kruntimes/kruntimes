// Package runlogs provides the shared Runtime Pod log source and record format.
package runlogs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxRecordBytes = 2 << 20

var errRecordTooLarge = errors.New("structured log record exceeds limit")

// PodReader opens the fixed runtimed container-log stream for a Runtime Pod.
type PodReader interface {
	ReadPodLogs(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error)
}

// KubernetesPodReader reads Pod logs with the calling ServiceAccount.
type KubernetesPodReader struct {
	Client corev1client.CoreV1Interface
}

func (r KubernetesPodReader) ReadPodLogs(ctx context.Context, namespace, pod string, options corev1.PodLogOptions) (io.ReadCloser, error) {
	if r.Client == nil {
		return nil, errors.New("Kubernetes Pod log client is not configured")
	}
	return r.Client.Pods(namespace).GetLogs(pod, &options).Stream(ctx)
}

// Entry is one structured log record emitted by runtimed for a Run.
type Entry struct {
	Timestamp            string `json:"timestamp,omitempty"`
	Stream               string `json:"stream"`
	Message              string `json:"message"`
	InvocationID         string `json:"invocationId,omitempty"`
	Operation            string `json:"operation,omitempty"`
	Outcome              string `json:"outcome,omitempty"`
	StatusCode           string `json:"statusCode,omitempty"`
	ExitCode             *int32 `json:"exitCode,omitempty"`
	TimedOut             bool   `json:"timedOut,omitempty"`
	DurationMilliseconds int64  `json:"durationMilliseconds,omitempty"`
}

// Open opens the runtimed container log source for a Run. Callers filter the
// stream by Run UID because a Runtime Pod can execute multiple Runs.
func Open(ctx context.Context, podLogs PodReader, run *v1alpha1.Run, follow bool, limitBytes int64) (io.ReadCloser, error) {
	if podLogs == nil {
		return nil, errors.New("Runtime Pod log reader is not configured")
	}
	if run == nil || run.Status.AssignedPod == "" {
		return nil, status.Error(codes.FailedPrecondition, "Run has no assigned Runtime Pod")
	}
	logStartTime := run.CreationTimestamp
	if run.Status.StartTime != nil {
		logStartTime = *run.Status.StartTime
	}
	options := corev1.PodLogOptions{
		Container:  "runtimed",
		Follow:     follow,
		SinceTime:  &logStartTime,
		Timestamps: true,
	}
	if !follow && limitBytes > 0 {
		options.LimitBytes = &limitBytes
	}
	return podLogs.ReadPodLogs(ctx, run.Namespace, run.Status.AssignedPod, options)
}

// ForEach filters a shared runtimed container log stream to a Run UID.
func ForEach(stream io.Reader, runUID string, visit func(Entry) error) error {
	reader := bufio.NewReaderSize(stream, 64<<10)
	for {
		line, err := readLine(reader)
		if errors.Is(err, errRecordTooLarge) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(line) > 0 {
			timestamp, recordLine := splitTimestamp(line)
			var record runtimedRecord
			if json.Unmarshal(recordLine, &record) == nil && record.RunUID == runUID {
				if visitErr := visit(record.entry(timestamp)); visitErr != nil {
					return visitErr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

type runtimedRecord struct {
	RunUID               string `json:"run_uid"`
	Stream               string `json:"stream"`
	Message              string `json:"message"`
	InvocationID         string `json:"invocation_id,omitempty"`
	Operation            string `json:"operation,omitempty"`
	Outcome              string `json:"outcome,omitempty"`
	StatusCode           string `json:"status_code,omitempty"`
	ExitCode             *int32 `json:"exit_code,omitempty"`
	TimedOut             bool   `json:"timed_out,omitempty"`
	DurationMilliseconds int64  `json:"duration_milliseconds,omitempty"`
}

func (r runtimedRecord) entry(timestamp string) Entry {
	return Entry{
		Timestamp:            timestamp,
		Stream:               r.Stream,
		Message:              r.Message,
		InvocationID:         r.InvocationID,
		Operation:            r.Operation,
		Outcome:              r.Outcome,
		StatusCode:           r.StatusCode,
		ExitCode:             r.ExitCode,
		TimedOut:             r.TimedOut,
		DurationMilliseconds: r.DurationMilliseconds,
	}
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 64<<10)
	tooLarge := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !tooLarge {
			if len(line)+len(fragment) > maxRecordBytes {
				tooLarge = true
			} else {
				line = append(line, fragment...)
			}
		}
		switch {
		case err == nil:
			if tooLarge {
				return nil, errRecordTooLarge
			}
			return trimLineEnding(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if tooLarge {
				return nil, errRecordTooLarge
			}
			return trimLineEnding(line), io.EOF
		default:
			return nil, err
		}
	}
}

func trimLineEnding(line []byte) []byte {
	return trimSuffix(trimSuffix(line, '\n'), '\r')
}

func trimSuffix(value []byte, suffix byte) []byte {
	if len(value) > 0 && value[len(value)-1] == suffix {
		return value[:len(value)-1]
	}
	return value
}

func splitTimestamp(line []byte) (string, []byte) {
	before, after, found := strings.Cut(string(line), " ")
	if !found || !strings.HasPrefix(after, "{") {
		return "", line
	}
	return before, []byte(after)
}
