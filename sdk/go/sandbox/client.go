// Package sandbox provides the agent-facing Go API for Session-mode Runs.
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const defaultPollInterval = 500 * time.Millisecond

// HTTPDoer is the HTTP transport used for Console Runtime access requests.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// SessionDialer opens the SDK's internal persistent Session connection.
// Implementations may rewrite the endpoint for a scoped Console port-forward.
type SessionDialer interface {
	DialSession(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)
}

// LogReader opens the runtimed container log for one assigned Runtime Pod.
type LogReader interface {
	Open(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)
}

// Client creates and manages Sandboxes backed by Session-mode Runs.
type Client struct {
	runs          client.Client
	httpClient    HTTPDoer
	sessionDialer SessionDialer
	logReader     LogReader
	bearerToken   string
	pollInterval  time.Duration
}

// Config supplies the explicit Kubernetes and Console dependencies for Client.
// Callers may use a Kubernetes-configured HTTP client that injects credentials
// instead of BearerToken.
type Config struct {
	Runs          client.Client
	HTTPClient    HTTPDoer
	SessionDialer SessionDialer
	LogReader     LogReader
	BearerToken   string
	PollInterval  time.Duration
}

// New constructs a Sandbox client. Kubernetes and Console dependencies are
// explicit so the same client works for in-cluster, local, and test transports.
func New(config Config) (*Client, error) {
	if config.Runs == nil {
		return nil, errors.New("sandbox Run client is required")
	}
	if config.HTTPClient == nil {
		return nil, errors.New("sandbox HTTP client is required")
	}
	interval := config.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	dialer := config.SessionDialer
	if dialer == nil {
		if forwarded, ok := config.HTTPClient.(SessionDialer); ok {
			dialer = forwarded
		} else {
			dialer = defaultSessionDialer{}
		}
	}
	return &Client{runs: config.Runs, httpClient: config.HTTPClient, sessionDialer: dialer, logReader: config.LogReader, bearerToken: config.BearerToken, pollInterval: interval}, nil
}

// NewFromRESTConfig constructs a Client from Kubernetes REST credentials.
func NewFromRESTConfig(config *rest.Config, options Config) (*Client, error) {
	if config == nil {
		return nil, errors.New("Kubernetes REST config is required")
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add kruntimes API scheme: %w", err)
	}
	if options.Runs == nil {
		runs, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			return nil, fmt.Errorf("create Kubernetes Run client: %w", err)
		}
		options.Runs = runs
	}
	if options.HTTPClient == nil {
		httpClient, err := rest.HTTPClientFor(config)
		if err != nil {
			return nil, fmt.Errorf("create Console HTTP client: %w", err)
		}
		options.HTTPClient = httpClient
	}
	if options.BearerToken == "" {
		options.BearerToken = config.BearerToken
	}
	if options.SessionDialer == nil {
		dialer, err := newRESTSessionDialer(config)
		if err != nil {
			return nil, err
		}
		options.SessionDialer = dialer
	}
	if options.LogReader == nil {
		pods, err := corev1client.NewForConfig(config)
		if err != nil {
			return nil, fmt.Errorf("create Kubernetes Pod log client: %w", err)
		}
		options.LogReader = KubernetesLogReader{Pods: pods}
	}
	return New(options)
}

// NewInCluster constructs a Client from the in-cluster ServiceAccount.
func NewInCluster(options Config) (*Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster Kubernetes config: %w", err)
	}
	return NewFromRESTConfig(config, options)
}

// KubernetesLogReader reads runtimed logs through Kubernetes Pod log requests.
type KubernetesLogReader struct{ Pods corev1client.CoreV1Interface }

// Open implements LogReader.
func (r KubernetesLogReader) Open(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
	if r.Pods == nil {
		return nil, errors.New("Kubernetes Pod log client is required")
	}
	return r.Pods.Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container}).Stream(ctx)
}

// create is the internal Run-creation half of Runtime.AcquireSandbox. It must
// not be exposed because callers could otherwise receive a Sandbox before it
// has acquired Runtime capacity.
func (c *Client) create(ctx context.Context, namespace, runtimeName string, options AcquireOptions) (*Sandbox, error) {
	if namespace == "" || runtimeName == "" {
		return nil, errors.New("sandbox namespace and runtime are required")
	}
	if options.Name == "" && options.GenerateName == "" {
		return nil, errors.New("sandbox name or generateName is required")
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: options.Name, GenerateName: options.GenerateName, Namespace: namespace},
		Spec: v1alpha1.RunSpec{
			Runtime:        runtimeName,
			Source:         options.Source,
			ArtifactInputs: options.ArtifactInputs,
			Env:            environmentVariables(options.Env),
			Timeout:        options.Timeout,
			Mode:           v1alpha1.RunMode{Session: options.Session},
		},
	}
	if run.Spec.Mode.Session == nil {
		run.Spec.Mode.Session = &v1alpha1.RunSessionMode{}
	}
	if err := c.runs.Create(ctx, run); err != nil {
		return nil, fmt.Errorf("create Session Run: %w", err)
	}
	return &Sandbox{client: c, run: run}, nil
}

// Open reads an existing Session-mode Run. It never creates or re-registers a
// sandbox as a side effect.
func (c *Client) Open(ctx context.Context, namespace, name string) (*Sandbox, error) {
	run := &v1alpha1.Run{}
	if err := c.runs.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, run); err != nil {
		return nil, fmt.Errorf("get Session Run: %w", err)
	}
	if run.Spec.Mode.Session == nil {
		return nil, &StateError{Run: run.DeepCopy(), Message: "Run is not a Session Run"}
	}
	return &Sandbox{client: c, run: run}, nil
}

// Sandbox is an opened Session-mode Run.
type Sandbox struct {
	client *Client
	run    *v1alpha1.Run
}

// Run returns a copy of the latest known Kubernetes Run object.
func (s *Sandbox) Run() *v1alpha1.Run { return s.run.DeepCopy() }

// Refresh reads the current Run status.
func (s *Sandbox) Refresh(ctx context.Context) error {
	if err := s.client.runs.Get(ctx, client.ObjectKeyFromObject(s.run), s.run); err != nil {
		return fmt.Errorf("get Session Run: %w", err)
	}
	return nil
}

// Wait waits until the Sandbox is Ready or becomes terminal.
func (s *Sandbox) Wait(ctx context.Context) error {
	for {
		if err := s.Refresh(ctx); err != nil {
			return err
		}
		switch s.run.Status.Phase {
		case v1alpha1.RunReady:
			return nil
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			return &StateError{Run: s.run.DeepCopy(), Message: "Session Run became terminal before it was ready"}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.client.pollInterval):
		}
	}
}

// Close requests graceful Session Run completion and waits for its successful
// terminal lifecycle. It never calls Runtime Server CloseSession directly.
func (s *Sandbox) Close(ctx context.Context) error {
	return s.terminate(ctx, v1alpha1.RunTerminationDrain)
}

// Cancel requests immediate Session Run cancellation and waits for its terminal
// lifecycle. It may escalate an existing Drain request, but never downgrades
// an Immediate request.
func (s *Sandbox) Cancel(ctx context.Context) error {
	return s.terminate(ctx, v1alpha1.RunTerminationImmediate)
}

func (s *Sandbox) terminate(ctx context.Context, mode v1alpha1.RunTerminationMode) error {
	if err := s.Refresh(ctx); err != nil {
		return err
	}
	if shouldRequestTermination(s.run.Spec.Termination, mode) {
		s.run.Spec.Termination = &v1alpha1.RunTermination{Mode: mode}
		if err := s.client.runs.Update(ctx, s.run); err != nil {
			return fmt.Errorf("terminate Session Run: %w", err)
		}
	}
	for {
		if err := s.Refresh(ctx); err != nil {
			return err
		}
		if isTerminal(s.run.Status.Phase) {
			return terminationResult(s.run, mode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.client.pollInterval):
		}
	}
}

func terminationResult(run *v1alpha1.Run, mode v1alpha1.RunTerminationMode) error {
	if run == nil {
		return &StateError{Message: "Session Run is not available"}
	}
	want := v1alpha1.RunSucceeded
	operation := "close"
	if mode == v1alpha1.RunTerminationImmediate {
		want = v1alpha1.RunCancelled
		operation = "cancel"
	}
	if run.Status.Phase == want {
		return nil
	}
	return &StateError{Run: run.DeepCopy(), Message: fmt.Sprintf("Session Run did not %s successfully; phase is %s", operation, run.Status.Phase)}
}

func shouldRequestTermination(current *v1alpha1.RunTermination, requested v1alpha1.RunTerminationMode) bool {
	if current == nil {
		return true
	}
	return current.Mode == v1alpha1.RunTerminationDrain && requested == v1alpha1.RunTerminationImmediate
}

// Command defines one workspace-relative process execution request.
type Command struct {
	Argv             []string          `json:"argv,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Stdin            []byte            `json:"stdin,omitempty"`
	TimeoutMillis    int64             `json:"timeoutMillis,omitempty"`
}

// CommandResult is the bounded result of one command operation.
type CommandResult struct {
	ExitCode int32  `json:"exitCode"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

// OperationEvent is one ordered event emitted by a streaming Session operation.
type OperationEvent struct {
	Sequence int64  `json:"sequence"`
	Type     string `json:"type"`
	Accepted *struct {
		OperationID string `json:"operationID"`
	} `json:"accepted,omitempty"`
	Output    *OperationOutput   `json:"output,omitempty"`
	Progress  *OperationProgress `json:"progress,omitempty"`
	Completed *struct {
		Command *CommandResult `json:"command,omitempty"`
	} `json:"completed,omitempty"`
	Failed *OperationFailure `json:"failed,omitempty"`
}

type OperationOutput struct {
	Stream string `json:"stream"`
	Data   []byte `json:"data,omitempty"`
}
type OperationProgress struct {
	Kind        string `json:"kind"`
	Message     string `json:"message,omitempty"`
	ToolCallID  string `json:"toolCallID,omitempty"`
	ToolName    string `json:"toolName,omitempty"`
	Data        []byte `json:"data,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}
type OperationFailure struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}

// Execute runs exactly one command. Transport errors have unknown execution
// outcome and are intentionally never retried by this SDK.
func (s *Sandbox) Execute(ctx context.Context, command Command) (CommandResult, error) {
	var response struct {
		Command *CommandResult `json:"command"`
	}
	err := s.operation(ctx, map[string]any{"command": command}, &response)
	if err != nil {
		return CommandResult{}, err
	}
	if response.Command == nil {
		return CommandResult{}, errors.New("Console response did not include a command result")
	}
	return *response.Command, nil
}

// WriteFile writes bounded content at a workspace-relative path.
func (s *Sandbox) WriteFile(ctx context.Context, path string, contents []byte, createParents bool) error {
	return s.operation(ctx, map[string]any{"writeFile": map[string]any{"path": path, "contents": contents, "createParents": createParents}}, nil)
}

// CreateDirectory creates a workspace-relative directory and missing parents.
func (s *Sandbox) CreateDirectory(ctx context.Context, path string) error {
	return s.operation(ctx, map[string]any{"createDirectory": map[string]any{"path": path}}, nil)
}

// DeleteFile removes a workspace-relative file or directory.
func (s *Sandbox) DeleteFile(ctx context.Context, path string, recursive bool) error {
	return s.operation(ctx, map[string]any{"deleteFile": map[string]any{"path": path, "recursive": recursive}}, nil)
}

// RenameFile renames a workspace-relative file or directory.
func (s *Sandbox) RenameFile(ctx context.Context, sourcePath, destinationPath string, overwrite bool) error {
	return s.operation(ctx, map[string]any{"renameFile": map[string]any{"sourcePath": sourcePath, "destinationPath": destinationPath, "overwrite": overwrite}}, nil)
}

// ReadFile returns bounded content at a workspace-relative path.
func (s *Sandbox) ReadFile(ctx context.Context, path string, maxBytes int64) ([]byte, bool, error) {
	endpoint, err := s.endpoint("files")
	if err != nil {
		return nil, false, err
	}
	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, false, fmt.Errorf("parse Session file endpoint: %w", err)
	}
	endpointURL.Path = strings.TrimRight(endpointURL.Path, "/") + "/" + path
	if maxBytes > 0 {
		query := endpointURL.Query()
		query.Set("maxBytes", fmt.Sprint(maxBytes))
		endpointURL.RawQuery = query.Encode()
	}
	var response struct {
		Contents  []byte `json:"contents"`
		Truncated bool   `json:"truncated"`
	}
	if err := s.request(ctx, http.MethodGet, endpointURL.String(), nil, &response); err != nil {
		return nil, false, err
	}
	return response.Contents, response.Truncated, nil
}

// FileInfo identifies one direct child of a workspace-relative directory.
type FileInfo struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	SizeBytes int64  `json:"sizeBytes"`
}

// ListFilesOptions selects one bounded page of direct workspace-relative children.
// A zero Limit lets the Runtime Server apply its default page size.
type ListFilesOptions struct {
	Directory string
	Limit     int
	PageToken string
}

// FilePage is one bounded directory listing response.
type FilePage struct {
	Entries       []FileInfo `json:"entries"`
	NextPageToken string     `json:"nextPageToken"`
}

// ListFiles returns one bounded page of direct children below a workspace-relative path.
func (s *Sandbox) ListFiles(ctx context.Context, options ListFilesOptions) (FilePage, error) {
	endpoint, err := s.endpoint("files")
	if err != nil {
		return FilePage{}, err
	}
	if options.Limit < 0 {
		return FilePage{}, errors.New("file page limit cannot be negative")
	}
	query := url.Values{}
	if options.Directory != "" {
		query.Set("path", options.Directory)
	}
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.PageToken != "" {
		query.Set("pageToken", options.PageToken)
	}
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	var response FilePage
	if err := s.request(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return FilePage{}, err
	}
	return response, nil
}

// LogLine is one structured output or audit record emitted by owner runtimed.
type LogLine struct {
	RunUID               string `json:"run_uid"`
	AssignedPodUID       string `json:"assigned_pod_uid,omitempty"`
	Stream               string `json:"stream"`
	Message              string `json:"message"`
	Operation            string `json:"operation,omitempty"`
	Outcome              string `json:"outcome,omitempty"`
	StatusCode           string `json:"status_code,omitempty"`
	ExitCode             *int32 `json:"exit_code,omitempty"`
	TimedOut             bool   `json:"timed_out,omitempty"`
	DurationMilliseconds int64  `json:"duration_milliseconds,omitempty"`
}

// Logs returns structured owner-runtimed log lines for this Sandbox only.
func (s *Sandbox) Logs(ctx context.Context) ([]LogLine, error) {
	if s.client.logReader == nil {
		return nil, errors.New("sandbox log reader is not configured")
	}
	if err := s.Refresh(ctx); err != nil {
		return nil, err
	}
	if s.run.Status.AssignedPod == "" {
		return nil, &StateError{Run: s.run.DeepCopy(), Message: "Session Run has no assigned Runtime Pod"}
	}
	stream, err := s.client.logReader.Open(ctx, s.run.Namespace, s.run.Status.AssignedPod, "runtimed")
	if err != nil {
		return nil, fmt.Errorf("open runtimed logs: %w", err)
	}
	defer stream.Close()
	lines := []LogLine{}
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var line LogLine
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line.RunUID == string(s.run.UID) {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read runtimed logs: %w", err)
	}
	return lines, nil
}

// APIError reports a non-success Console Runtime access API response.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Console Runtime access API returned HTTP %d: %s", e.StatusCode, e.Message)
}

// StateError reports an invalid Session Run lifecycle state.
type StateError struct {
	Run     *v1alpha1.Run
	Message string
}

func (e *StateError) Error() string { return e.Message }

func (s *Sandbox) operation(ctx context.Context, operation any, response any) error {
	endpoint, err := s.endpoint("operations:execute")
	if err != nil {
		return err
	}
	return s.request(ctx, http.MethodPost, endpoint, operation, response)
}

func (s *Sandbox) endpoint(suffix string) (string, error) {
	if s.run.Status.Phase != v1alpha1.RunReady || s.run.Status.Endpoint == nil || s.run.Status.Endpoint.URL == "" {
		return "", &StateError{Run: s.run.DeepCopy(), Message: "Session Run is not ready"}
	}
	endpoint, err := url.Parse(s.run.Status.Endpoint.URL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return "", &StateError{Run: s.run.DeepCopy(), Message: "Session Run has an invalid endpoint"}
	}
	parts := strings.Split(strings.Trim(endpoint.Path, "/"), "/")
	if len(parts) < 7 || parts[len(parts)-2] != "sessions" || parts[len(parts)-1] != string(s.run.UID) {
		return "", &StateError{Run: s.run.DeepCopy(), Message: "Session Run endpoint does not match its UID"}
	}
	endpoint.Path = path.Join(endpoint.Path, suffix)
	return endpoint.String(), nil
}

func (s *Sandbox) request(ctx context.Context, method, endpoint string, body, response any) error {
	var content io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Console request: %w", err)
		}
		content = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, content)
	if err != nil {
		return fmt.Errorf("build Console request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if s.client.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+s.client.bearerToken)
	}
	result, err := s.client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call Console Runtime access API: %w", err)
	}
	defer result.Body.Close()
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		var responseError struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(result.Body, 1<<20)).Decode(&responseError)
		return &APIError{StatusCode: result.StatusCode, Message: responseError.Error}
	}
	if response != nil {
		if err := json.NewDecoder(io.LimitReader(result.Body, 1<<20)).Decode(response); err != nil {
			return fmt.Errorf("decode Console response: %w", err)
		}
	}
	return nil
}

func isTerminal(phase v1alpha1.RunPhase) bool {
	return phase == v1alpha1.RunSucceeded || phase == v1alpha1.RunFailed || phase == v1alpha1.RunTimeout || phase == v1alpha1.RunCancelled
}

func environmentVariables(values map[string]string) []corev1.EnvVar {
	if len(values) == 0 {
		return nil
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	variables := make([]corev1.EnvVar, 0, len(names))
	for _, name := range names {
		variables = append(variables, corev1.EnvVar{Name: name, Value: values[name]})
	}
	return variables
}
