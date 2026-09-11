package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultLogTailLines int64 = 100
	maxLogTailLines     int64 = 500
	defaultLogBytes     int64 = 1 << 20
	maxLogBytes         int64 = 1 << 20
	maxLogRecordBytes         = 2 << 20
)

var errLogRecordTooLarge = errors.New("structured log record exceeds limit")

// PodLogReader opens the single fixed container-log stream the Gateway needs
// after it has authenticated and authorized access to a Run.
type PodLogReader interface {
	ReadPodLogs(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error)
}

// KubernetesPodLogReader reads Pod logs with the Gateway ServiceAccount.
type KubernetesPodLogReader struct {
	Client corev1client.CoreV1Interface
}

func (r KubernetesPodLogReader) ReadPodLogs(ctx context.Context, namespace, pod string, options corev1.PodLogOptions) (io.ReadCloser, error) {
	if r.Client == nil {
		return nil, errors.New("Kubernetes Pod log client is not configured")
	}
	return r.Client.Pods(namespace).GetLogs(pod, &options).Stream(ctx)
}

type runLogOptions struct {
	tailLines  int64
	limitBytes int64
	follow     bool
}

type runtimedLogRecord struct {
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

type RunLogEntry struct {
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

// runLogEntry retains the original internal name while allowing the
// aggregation API to use the exact same structured log representation.
type runLogEntry = RunLogEntry

func (r runtimedLogRecord) entry(timestamp string) runLogEntry {
	return runLogEntry{
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

// OpenRunLogStream opens the runtimed container log source for a Run. Callers
// apply Run UID filtering after opening it because a Runtime Pod is shared by
// multiple Runs.
func OpenRunLogStream(ctx context.Context, podLogs PodLogReader, run *v1alpha1.Run, follow bool, limitBytes int64) (io.ReadCloser, error) {
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
	options := corev1.PodLogOptions{Container: "runtimed", Follow: follow, SinceTime: &logStartTime, Timestamps: true}
	if !follow && limitBytes > 0 {
		options.LimitBytes = &limitBytes
	}
	return podLogs.ReadPodLogs(ctx, run.Namespace, run.Status.AssignedPod, options)
}

func runLogRoute(path string) (namespace, runtimeName, runUID string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 8 || parts[0] != "v1" || parts[1] != "namespaces" || parts[3] != "runtimes" || parts[5] != "runs" || parts[7] != "logs" {
		return "", "", "", false
	}
	var err error
	if namespace, err = url.PathUnescape(parts[2]); err != nil || namespace == "" {
		return "", "", "", false
	}
	if runtimeName, err = url.PathUnescape(parts[4]); err != nil || runtimeName == "" {
		return "", "", "", false
	}
	if runUID, err = url.PathUnescape(parts[6]); err != nil || runUID == "" {
		return "", "", "", false
	}
	return namespace, runtimeName, runUID, true
}

func parseRunLogOptions(query url.Values) (runLogOptions, error) {
	options := runLogOptions{tailLines: defaultLogTailLines, limitBytes: defaultLogBytes}
	if value := query.Get("tailLines"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 || parsed > maxLogTailLines {
			return runLogOptions{}, fmt.Errorf("tailLines must be an integer from 1 through %d", maxLogTailLines)
		}
		options.tailLines = parsed
	}
	if value := query.Get("limitBytes"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 || parsed > maxLogBytes {
			return runLogOptions{}, fmt.Errorf("limitBytes must be a positive integer no greater than %d", maxLogBytes)
		}
		options.limitBytes = parsed
	}
	if value := query.Get("follow"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return runLogOptions{}, errors.New("follow must be true or false")
		}
		options.follow = parsed
	}
	if options.follow && query.Get("limitBytes") != "" {
		return runLogOptions{}, errors.New("limitBytes cannot be used with follow=true")
	}
	return options, nil
}

func (s *Server) serveRunLogs(w http.ResponseWriter, r *http.Request, namespace, runtimeName, runUID string) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	options, err := parseRunLogOptions(r.URL.Query())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := s.runForLogs(r.Context(), namespace, runtimeName, runUID)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	if s.Authorizer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "gateway authorization is not configured")
		return
	}
	if err := s.Authorizer.Authorize(r.Context(), r, run); err != nil {
		s.writeGatewayError(w, err)
		return
	}
	if s.PodLogs == nil {
		s.writeError(w, http.StatusServiceUnavailable, "gateway Pod log reader is not configured")
		return
	}

	// The Pod log source is shared by many Runs. Asking Kubernetes for its
	// global tail first can exclude an older selected Run even while its records
	// are still retained. Start at this Run instead, then apply tailLines after
	// UID filtering below.
	stream, err := OpenRunLogStream(r.Context(), s.PodLogs, run, options.follow, options.limitBytes)
	if err != nil {
		s.writeRunLogReadError(w, err)
		return
	}
	defer stream.Close()

	if options.follow {
		s.streamRunLogs(w, stream, string(run.UID))
		return
	}
	entries, err := readRunLogEntries(stream, string(run.UID), options.tailLines)
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "read Runtime Pod logs")
		return
	}
	s.writeJSON(w, http.StatusOK, struct {
		Items []runLogEntry `json:"items"`
	}{Items: entries})
}

// writeRunLogReadError keeps the Pod identity private while distinguishing a
// permanently unavailable source from a transient Kubernetes log-service
// failure. A Run can retain an assignedPod after that Runtime Pod is gone.
func (s *Server) writeRunLogReadError(w http.ResponseWriter, err error) {
	if apierrors.IsNotFound(err) {
		s.writeError(w, http.StatusConflict, "assigned Runtime Pod is no longer available; Run logs cannot be read")
		return
	}
	s.writeError(w, http.StatusServiceUnavailable, "read Runtime Pod logs")
}

func (s *Server) runForLogs(ctx context.Context, namespace, runtimeName, runUID string) (*v1alpha1.Run, error) {
	if s.Runs == nil {
		return nil, status.Error(codes.FailedPrecondition, "gateway is not configured")
	}
	var runs v1alpha1.RunList
	if err := s.Runs.List(ctx, &runs, client.InNamespace(namespace), client.MatchingFields{runtimeIndexField: runtimeName}); err != nil {
		return nil, status.Errorf(codes.Internal, "list Runtime Runs: %v", err)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if string(run.UID) != runUID {
			continue
		}
		if run.Spec.Runtime != runtimeName {
			return nil, status.Error(codes.NotFound, "Run not found")
		}
		if run.Status.AssignedPod == "" {
			return nil, status.Error(codes.FailedPrecondition, "Run has no assigned Runtime Pod")
		}
		return run, nil
	}
	return nil, status.Error(codes.NotFound, "Run not found")
}

func (s *Server) streamRunLogs(w http.ResponseWriter, stream io.Reader, runUID string) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	response := http.NewResponseController(w)
	if err := forEachRunLogEntry(stream, runUID, func(entry runLogEntry) error {
		if err := encoder.Encode(entry); err != nil {
			return err
		}
		return response.Flush()
	}); err != nil && !errors.Is(err, errLogRecordTooLarge) {
		return
	}
}

func readRunLogEntries(stream io.Reader, runUID string, limit int64) ([]runLogEntry, error) {
	entries := make([]runLogEntry, 0, limit)
	err := forEachRunLogEntry(stream, runUID, func(entry runLogEntry) error {
		if int64(len(entries)) == limit {
			copy(entries, entries[1:])
			entries[len(entries)-1] = entry
			return nil
		}
		entries = append(entries, entry)
		return nil
	})
	return entries, err
}

// ForEachRunLogEntry filters a shared runtimed container log stream to the
// requested Run. It is shared by the direct Gateway and aggregated log API.
func ForEachRunLogEntry(stream io.Reader, runUID string, visit func(RunLogEntry) error) error {
	reader := bufio.NewReaderSize(stream, 64<<10)
	for {
		line, err := readLogLine(reader)
		if errors.Is(err, errLogRecordTooLarge) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(line) > 0 {
			timestamp, recordLine := splitLogTimestamp(line)
			var record runtimedLogRecord
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

func forEachRunLogEntry(stream io.Reader, runUID string, visit func(runLogEntry) error) error {
	return ForEachRunLogEntry(stream, runUID, visit)
}

func readLogLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 64<<10)
	tooLarge := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !tooLarge {
			if len(line)+len(fragment) > maxLogRecordBytes {
				tooLarge = true
			} else {
				line = append(line, fragment...)
			}
		}
		switch {
		case err == nil:
			if tooLarge {
				return nil, errLogRecordTooLarge
			}
			return bytesTrimLineEnding(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if tooLarge {
				return nil, errLogRecordTooLarge
			}
			return bytesTrimLineEnding(line), io.EOF
		default:
			return nil, err
		}
	}
}

func bytesTrimLineEnding(line []byte) []byte {
	return bytesTrimSuffix(bytesTrimSuffix(line, '\n'), '\r')
}

func bytesTrimSuffix(value []byte, suffix byte) []byte {
	if len(value) > 0 && value[len(value)-1] == suffix {
		return value[:len(value)-1]
	}
	return value
}

func splitLogTimestamp(line []byte) (string, []byte) {
	before, after, found := strings.Cut(string(line), " ")
	if !found || !strings.HasPrefix(after, "{") {
		return "", line
	}
	return before, []byte(after)
}
