// Package sandbox provides the agent-facing Go API for Session-mode Runs.
package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/runextensionapi"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const defaultPollInterval = 500 * time.Millisecond

// LogReader opens the runtimed container log for one assigned Runtime Pod.
type LogReader interface {
	Open(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)
}

// Client creates and manages Sandboxes backed by Session-mode Runs.
type Client struct {
	runs         client.Client
	extension    *runextensionapi.Client
	logReader    LogReader
	pollInterval time.Duration
}

// Config supplies the Kubernetes dependencies for Client.
//
// RESTConfig is required. The SDK uses it for Kubernetes API access and for the
// aggregated Run extension API that serves Session operations and Session
// files. Runs, LogReader, and PollInterval are optional overrides.
type Config struct {
	RESTConfig   *rest.Config
	Runs         client.Client
	LogReader    LogReader
	PollInterval time.Duration
}

// New constructs a Sandbox client. RESTConfig is required.
func New(config Config) (*Client, error) {
	if config.RESTConfig == nil || config.RESTConfig.Host == "" {
		return nil, errors.New("Kubernetes REST config is required")
	}
	if config.Runs == nil {
		return nil, errors.New("sandbox Run client is required")
	}
	interval := config.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	extension, err := runextensionapi.NewForConfig(config.RESTConfig)
	if err != nil {
		return nil, err
	}
	return &Client{
		runs:         config.Runs,
		extension:    extension,
		logReader:    config.LogReader,
		pollInterval: interval,
	}, nil
}

// NewFromRESTConfig constructs a Client from Kubernetes REST credentials.
func NewFromRESTConfig(config *rest.Config, options Config) (*Client, error) {
	if config == nil {
		return nil, errors.New("Kubernetes REST config is required")
	}
	options.RESTConfig = config
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

// Command, Operation, and the other Session wire types are aliases of the
// reusable Run extension API client types.
type Command = runextensionapi.Command
type Operation = runextensionapi.Operation
type FileWrite = runextensionapi.FileWrite
type DirectoryCreate = runextensionapi.DirectoryCreate
type FileDelete = runextensionapi.FileDelete
type FileRename = runextensionapi.FileRename
type CommandResult = runextensionapi.CommandResult
type OperationEvent = runextensionapi.OperationEvent
type OperationOutput = runextensionapi.OperationOutput
type OperationProgress = runextensionapi.OperationProgress
type OperationFailure = runextensionapi.OperationFailure

// ReadFile returns bounded content at a workspace-relative path.
func (s *Sandbox) ReadFile(ctx context.Context, path string, maxBytes int64) ([]byte, bool, error) {
	if s == nil || s.client == nil {
		return nil, false, &StateError{Message: "Sandbox is not available"}
	}
	return s.client.extension.ReadSessionFile(ctx, s.run.Namespace, s.run.Name, path, maxBytes)
}

// FileInfo identifies one direct child of a workspace-relative directory.
type FileInfo = runextensionapi.SessionFileInfo

// ListFilesOptions selects one bounded page of direct workspace-relative children.
// A zero Limit lets the Runtime Server apply its default page size.
type ListFilesOptions = runextensionapi.ListSessionFilesOptions

// FilePage is one bounded directory listing response.
type FilePage = runextensionapi.SessionFilePage

// ListFiles returns one bounded page of direct children below a workspace-relative path.
func (s *Sandbox) ListFiles(ctx context.Context, options ListFilesOptions) (FilePage, error) {
	if s == nil || s.client == nil {
		return FilePage{}, &StateError{Message: "Sandbox is not available"}
	}
	page, err := s.client.extension.ListSessionFiles(ctx, s.run.Namespace, s.run.Name, &options)
	if err != nil {
		return FilePage{}, err
	}
	return *page, nil
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

// APIError reports a non-success Run extension API response.
type APIError = runextensionapi.APIError

// StateError reports an invalid Session Run lifecycle state.
type StateError struct {
	Run     *v1alpha1.Run
	Message string
}

func (e *StateError) Error() string { return e.Message }

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
