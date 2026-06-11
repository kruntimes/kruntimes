package runtimed

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	rlegpkg "github.com/kruntimes/kruntimes/internal/runtimed/rleg"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

var workspacePath = "/workspace" //nolint:gochecknoglobals

const defaultHeartbeatInterval = 5 * time.Second

var (
	runsCompleted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kruntimes_runtimed_runs_total",
			Help: "Total number of runs completed by this runtimed.",
		},
		[]string{"runtime", "result"},
	)
	runDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kruntimes_runtimed_run_duration_seconds",
			Help:    "Run execution duration.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"runtime"},
	)
)

type activeRun struct {
	run      *v1alpha1.Run
	workDir  string
	deadline time.Time
	start    time.Time
}

// Controller reconciles Runs assigned to this pod.
type Controller struct {
	client.Client
	PodReader         client.Reader
	RunReader         client.Reader
	Log               logr.Logger
	Hostname          string
	RuntimeName       string
	RuntimeNamespace  string
	RuntimeEndpoint   string
	Workers           int
	ArtifactStore     artifact.Store
	MaxArtifactBytes  int64
	MaxArtifactsBytes int64

	HeartbeatInterval  time.Duration
	ExecutionLogWriter io.Writer

	activeRuns sync.Map // uid → *activeRun
	rlegCh     chan event.GenericEvent
	logMu      sync.Mutex

	runtimeCli pb.RuntimeClient
	rleg       rlegpkg.RunLifecycleEventGenerator
	Recorder   record.EventRecorder
}

// SetupWithManager registers the controller with controller-runtime.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	c.rlegCh = make(chan event.GenericEvent, 256)

	if err := mgr.Add(c); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}).
		WithEventFilter(c.runFilter()).
		WatchesRawSource(source.Channel(c.rlegCh, &handler.EnqueueRequestForObject{})).
		Complete(c)
}

// Start implements manager.Runnable.
func (c *Controller) Start(ctx context.Context) error {
	conn, err := grpc.NewClient(c.RuntimeEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial runtime %s: %w", c.RuntimeEndpoint, err)
	}
	c.runtimeCli = pb.NewRuntimeClient(conn)
	go func() { <-ctx.Done(); conn.Close() }()

	c.rleg = rlegpkg.NewGenericRLEG(&statusAdapter{cli: c.runtimeCli}, rlegpkg.DefaultRelistInterval)
	go c.rleg.Start(ctx)
	go c.forwardRLEGEvents(ctx)
	go c.heartbeat(ctx)
	go c.recoverActiveRuns(ctx)

	klog.Infof("runtimed controller started, hostname=%s, runtime=%s, workers=%d", c.Hostname, c.RuntimeEndpoint, c.capacity())
	return nil
}

// forwardRLEGEvents translates RLEG events to controller-runtime GenericEvents.
func (c *Controller) forwardRLEGEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-c.rleg.Events():
			if !ok {
				return
			}
			select {
			case c.rlegCh <- event.GenericEvent{Object: ev.Run}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (c *Controller) runFilter() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			run, ok := e.Object.(*v1alpha1.Run)
			return ok && c.matchesRuntimeNamespace(run) && run.Status.AssignedPod == c.Hostname
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			run, ok := e.ObjectNew.(*v1alpha1.Run)
			return ok && c.matchesRuntimeNamespace(run) &&
				(run.Status.AssignedPod == c.Hostname || c.shouldCleanupArtifacts(run))
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

func (c *Controller) matchesRuntimeNamespace(run *v1alpha1.Run) bool {
	return run != nil && (c.RuntimeNamespace == "" || run.Namespace == c.RuntimeNamespace)
}

// Reconcile reads the Run spec+status, dispatches to the appropriate
// sub-reconciler based on phase, then updates the status once.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run v1alpha1.Run
	if err := c.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		return c.reconcileArtifactDeletion(ctx, &run)
	}

	switch run.Status.Phase {
	case v1alpha1.RunScheduled:
		return c.reconcileScheduled(ctx, &run)
	case v1alpha1.RunRunning:
		return c.reconcileRunning(ctx, &run)
	}
	return ctrl.Result{}, nil
}

// ---------------------------------------------------------------------------
// Scheduled → claim + start execution
// ---------------------------------------------------------------------------

func (c *Controller) reconcileScheduled(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	uid := string(run.UID)
	if _, exists := c.activeRuns.Load(uid); exists {
		return ctrl.Result{}, nil
	}
	if run.Spec.CancelRequested {
		return c.applyTerminal(ctx, c.buildActiveRun(run), v1alpha1.RunCancelled, runretry.ReasonCancelled, "cancelled by user")
	}
	if c.activeRunCount() >= c.capacity() {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	run.Status.Phase = v1alpha1.RunRunning
	run.Status.StartTime = &metav1.Time{Time: time.Now()}
	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	klog.Infof("Claimed run %s", run.Name)

	ar := &activeRun{
		run:     run,
		start:   time.Now(),
		workDir: filepath.Join(workspacePath, uid),
	}
	if run.Spec.Timeout != nil {
		ar.deadline = run.Status.StartTime.Add(run.Spec.Timeout.Duration)
	}

	c.activeRuns.Store(uid, ar)

	workDir, err := prepareSource(run)
	if err != nil {
		return c.applyFailure(ctx, ar, runretry.ReasonPrepareSource, fmt.Sprintf("prepare source: %v", err))
	}
	ar.workDir = workDir
	if err := c.startExecution(ctx, ar); err != nil {
		return c.applyFailure(ctx, ar, runretry.ReasonRuntimeExecute, fmt.Sprintf("runtime Execute: %v", err))
	}
	c.rleg.AddRun(run)
	return ctrl.Result{}, nil
}

func (c *Controller) heartbeat(ctx context.Context) {
	interval := c.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	c.patchRuntimedReady(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.patchRuntimedReady(ctx)
		}
	}
}

func (c *Controller) patchRuntimedReady(ctx context.Context) {
	if c.Hostname == "" {
		return
	}

	var pod corev1.Pod
	key := types.NamespacedName{Name: c.Hostname, Namespace: podNamespace()}
	reader := c.PodReader
	if reader == nil {
		reader = c.Client
	}
	if err := reader.Get(ctx, key, &pod); err != nil {
		c.Log.Error(err, "failed to get own pod for runtimed heartbeat", "pod", key)
		return
	}

	before := pod.DeepCopy()
	runtimepod.SetRuntimedReadyCondition(&pod, corev1.ConditionTrue, "Heartbeat", "runtimed heartbeat is fresh", metav1.Now())
	if err := c.Status().Patch(ctx, &pod, client.MergeFrom(before)); err != nil {
		c.Log.Error(err, "failed to patch runtimed heartbeat", "pod", key)
	}
}

func (c *Controller) capacity() int {
	if c.Workers <= 0 {
		return 1
	}
	return c.Workers
}

func (c *Controller) activeRunCount() int {
	count := 0
	c.activeRuns.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// ---------------------------------------------------------------------------
// Running — dispatch by sub-state (cancel / backoff / active)
// ---------------------------------------------------------------------------

func (c *Controller) reconcileRunning(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	uid := string(run.UID)
	val, exists := c.activeRuns.Load(uid)
	if !exists {
		return c.reconcileRunningRecovered(ctx, run)
	}
	ar := val.(*activeRun)
	ar.run = run
	c.rleg.UpdateRun(run)

	// Cancel takes priority.
	if run.Spec.CancelRequested {
		return c.applyCancel(ctx, ar)
	}

	cond := meta.FindStatusCondition(run.Status.Conditions, "Running")
	if cond != nil && cond.Status == metav1.ConditionFalse {
		return c.reconcileRetryBackoff(ctx, ar)
	}
	return c.reconcileRunningActive(ctx, ar)
}

func (c *Controller) reconcileRunningRecovered(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	resp, err := c.pollStatus(ctx, string(run.UID))
	if err != nil {
		ar := c.buildActiveRun(run)
		return c.applyFailure(ctx, ar, runretry.ReasonRuntimeExecute, fmt.Sprintf("runtime Status after runtimed restart: %v", err))
	}

	ar := c.addRecoveredRun(run)
	switch resp.State {
	case pb.ExecutionState_EXECUTION_STATE_SUCCEEDED:
		return c.applySuccess(ctx, ar, resp)
	case pb.ExecutionState_EXECUTION_STATE_FAILED:
		reason := classifyFailureReason(resp, nil)
		msg := summarizeRuntimeFailure(resp)
		return c.applyFailureWithOutput(ctx, ar, reason, msg, outputFromStatus(resp))
	default:
		return ctrl.Result{}, nil
	}
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

func (c *Controller) applyCancel(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	run := ar.run
	uid := string(run.UID)
	_, _ = c.runtimeCli.Cancel(ctx, &pb.CancelRequest{Id: uid})
	return c.applyTerminal(ctx, ar, v1alpha1.RunCancelled, runretry.ReasonCancelled, "cancelled by user")
}

// ---------------------------------------------------------------------------
// Timeout
// ---------------------------------------------------------------------------

func (c *Controller) handleTimeout(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	uid := string(ar.run.UID)
	_, _ = c.runtimeCli.Cancel(ctx, &pb.CancelRequest{Id: uid})
	msg := fmt.Sprintf("timeout after %s", ar.run.Spec.Timeout.Duration)
	return c.applyFailure(ctx, ar, runretry.ReasonTimeout, msg)
}

// ===========================================================================
// reconcileRunningActive — poll gRPC status, handle terminal states
// ===========================================================================

func (c *Controller) reconcileRunningActive(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	if !ar.deadline.IsZero() && time.Now().After(ar.deadline) {
		return c.handleTimeout(ctx, ar)
	}

	resp, err := c.pollStatus(ctx, string(ar.run.UID))
	if err != nil {
		return ctrl.Result{}, nil
	}

	switch resp.State {
	case pb.ExecutionState_EXECUTION_STATE_SUCCEEDED:
		return c.applySuccess(ctx, ar, resp)
	case pb.ExecutionState_EXECUTION_STATE_FAILED:
		reason := classifyFailureReason(resp, nil)
		msg := summarizeRuntimeFailure(resp)
		return c.applyFailureWithOutput(ctx, ar, reason, msg, outputFromStatus(resp))
	}
	return ctrl.Result{}, nil
}

// ===========================================================================
// Retry backoff — check if backoff has expired, start retry if so
// ===========================================================================

func (c *Controller) reconcileRetryBackoff(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	run := ar.run
	policy := runretry.WithDefaults(run.Spec.RetryPolicy)
	backoff := runretry.Backoff(policy, run.Status.Attempt)

	cond := meta.FindStatusCondition(run.Status.Conditions, "Running")
	if cond == nil {
		return ctrl.Result{}, nil
	}
	retryAfter := cond.LastTransitionTime.Add(backoff)
	if time.Now().Before(retryAfter) {
		return ctrl.Result{RequeueAfter: time.Until(retryAfter)}, nil
	}

	// Backoff expired — start the retry.
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: "Running", Status: metav1.ConditionTrue, Reason: "Retrying", Message: "Retry after failure",
	})
	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	c.recordEvent(run, corev1.EventTypeNormal, "RunRetrying",
		"Retry attempt %d/%d starting", run.Status.Attempt+1, policy.MaxAttempts)
	if err := c.startExecution(ctx, ar); err != nil {
		return c.applyFailure(ctx, ar, runretry.ReasonRuntimeExecute, fmt.Sprintf("runtime Execute: %v", err))
	}
	c.rleg.AddRun(run)
	return ctrl.Result{}, nil
}

// ===========================================================================
// applySuccess — terminal success, no retry
// ===========================================================================

func (c *Controller) applySuccess(ctx context.Context, ar *activeRun, resp *pb.StatusResponse) (ctrl.Result, error) {
	run := ar.run
	outputs, err := readOutputs(outputsPath(ar.workDir))
	if err != nil {
		reason := reasonOutputsInvalid
		if isOutputsTooLarge(err) {
			reason = reasonOutputsTooLarge
		}
		return c.applyTerminalWithOutput(ctx, ar, v1alpha1.RunFailed, reason, err.Error(), outputFromStatus(resp))
	}

	artifactRefs, err := c.collectArtifacts(ctx, run)
	if err != nil {
		if isArtifactInvalid(err) {
			if cleanupErr := c.deleteRunArtifacts(ctx, run); cleanupErr != nil {
				return ctrl.Result{}, cleanupErr
			}
			return c.applyTerminalWithOutput(
				ctx,
				ar,
				v1alpha1.RunFailed,
				"ArtifactInvalid",
				err.Error(),
				outputFromStatus(resp),
			)
		}
		c.Log.Error(err, "artifact collection failed; retrying", "run", client.ObjectKeyFromObject(run))
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	now := metav1.Now()
	if run.Status.Attempt == 0 {
		run.Status.Attempt = 1
	}
	run.Status.Phase = v1alpha1.RunSucceeded
	run.Status.Message = "execution completed"
	run.Status.CompletionTime = &now
	run.Status.Outputs = outputs
	run.Status.ArtifactRefs = artifactRefs
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: "Running", Status: metav1.ConditionFalse, Reason: "Completed", Message: "",
	})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: "Completed", Status: metav1.ConditionTrue, Reason: "RunCompleted", Message: "Completed successfully",
	})

	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	c.emitExecutionOutput(run, outputFromStatus(resp))
	c.cleanup(ctx, ar, v1alpha1.RunSucceeded)
	if run.Status.Attempt > 1 {
		c.recordEvent(run, corev1.EventTypeNormal, "RunSucceeded",
			"Run succeeded after %d attempts", run.Status.Attempt)
	}
	return ctrl.Result{}, nil
}

func (c *Controller) shouldCleanupArtifacts(run *v1alpha1.Run) bool {
	if run == nil || run.DeletionTimestamp.IsZero() ||
		!controllerutil.ContainsFinalizer(run, artifact.RunArtifactFinalizer) {
		return false
	}
	if c.RuntimeName != "" {
		return run.Spec.Runtime == c.RuntimeName
	}
	return run.Status.AssignedPod == c.Hostname
}

// ===========================================================================
// applyFailure — terminal failure or schedule retry
// ===========================================================================

func (c *Controller) applyFailure(ctx context.Context, ar *activeRun, reason, msg string) (ctrl.Result, error) {
	return c.applyFailureWithOutput(ctx, ar, reason, msg, executionOutput{})
}

func (c *Controller) applyFailureWithOutput(
	ctx context.Context,
	ar *activeRun,
	reason, msg string,
	output executionOutput,
) (ctrl.Result, error) {
	run := ar.run
	policy := runretry.WithDefaults(run.Spec.RetryPolicy)
	msg = boundedStatusMessage(msg)

	curAttempt := runretry.CurrentAttempt(run.Status.Attempt)

	if !runretry.ShouldRetry(policy, curAttempt, reason) {
		return c.applyTerminalWithOutput(ctx, ar, terminalPhaseForFailure(reason), reason, msg, output)
	}

	// Schedule retry.
	return c.scheduleRetry(ctx, ar, curAttempt, policy, reason, msg)
}

func terminalPhaseForFailure(reason string) v1alpha1.RunPhase {
	if reason == runretry.ReasonTimeout {
		return v1alpha1.RunTimeout
	}
	return v1alpha1.RunFailed
}

func (c *Controller) scheduleRetry(ctx context.Context, ar *activeRun, curAttempt int32, policy *v1alpha1.RetryPolicy, reason, msg string) (ctrl.Result, error) {
	run := ar.run
	nextAttempt := curAttempt + 1
	backoff := runretry.Backoff(policy, nextAttempt)

	run.Status.Attempt = nextAttempt
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: "Running", Status: metav1.ConditionFalse, Reason: reason, Message: msg,
	})

	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	c.rleg.RemoveRun(string(run.UID))
	c.recordEvent(run, corev1.EventTypeWarning, "RunFailedRetrying",
		"Run failed (attempt %d/%d), retrying in %s. Reason: %s: %s",
		curAttempt, policy.MaxAttempts, backoff, reason, msg)
	c.scheduleRetryReconcile(run, backoff)
	return ctrl.Result{}, nil
}

// ===========================================================================
// applyTerminal — common terminal phase transition
// ===========================================================================

func (c *Controller) applyTerminal(ctx context.Context, ar *activeRun, phase v1alpha1.RunPhase, reason, msg string) (ctrl.Result, error) {
	return c.applyTerminalWithOutput(ctx, ar, phase, reason, msg, executionOutput{})
}

func (c *Controller) applyTerminalWithOutput(
	ctx context.Context,
	ar *activeRun,
	phase v1alpha1.RunPhase,
	reason, msg string,
	output executionOutput,
) (ctrl.Result, error) {
	run := ar.run
	msg = boundedStatusMessage(msg)
	if run.Status.Attempt == 0 {
		run.Status.Attempt = 1
	}

	now := metav1.Now()
	run.Status.Phase = phase
	run.Status.Message = msg
	run.Status.CompletionTime = &now
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: "Running", Status: metav1.ConditionFalse, Reason: reason, Message: msg,
	})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: "Completed", Status: metav1.ConditionFalse, Reason: reason, Message: msg,
	})

	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	c.emitExecutionOutput(run, output)
	c.cleanup(ctx, ar, phase)
	c.recordEvent(run, corev1.EventTypeWarning, "RunRetriesExhausted",
		"Run failed after %d attempts: %s: %s", run.Status.Attempt, reason, msg)
	return ctrl.Result{}, nil
}

// ===========================================================================
// cleanup — in-memory bookkeeping after a successful status update
// ===========================================================================

func (c *Controller) cleanup(_ context.Context, ar *activeRun, phase v1alpha1.RunPhase) {
	if c.rleg != nil {
		c.rleg.RemoveRun(string(ar.run.UID))
	}
	c.activeRuns.Delete(string(ar.run.UID))
	runDuration.WithLabelValues(ar.run.Spec.Runtime).Observe(time.Since(ar.start).Seconds())
	runsCompleted.WithLabelValues(ar.run.Spec.Runtime, string(phase)).Inc()
}

// ===========================================================================
// startExecution — common gRPC Execute call used for initial and retry.
// ===========================================================================

func (c *Controller) startExecution(ctx context.Context, ar *activeRun) error {
	run := ar.run
	env := make(map[string]string)
	for _, e := range run.Spec.Env {
		env[e.Name] = e.Value
	}
	outputPath := outputsPath(ar.workDir)
	if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reset outputs file: %w", err)
	}
	env[artifact.OutputsEnv] = outputPath
	artifactsDir, err := c.prepareArtifactStaging(run)
	if err != nil {
		return err
	}
	if artifactsDir != "" {
		env[artifact.ArtifactsDirEnv] = artifactsDir
	}
	var timeoutSec int64
	if run.Spec.Timeout != nil {
		timeoutSec = int64(run.Spec.Timeout.Duration.Seconds())
	}
	entrypoint := run.Spec.Entrypoint
	if entrypoint == "" {
		entrypoint = "script"
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = c.runtimeCli.Execute(rctx, &pb.ExecuteRequest{
		Id:             string(run.UID),
		Args:           run.Spec.Args,
		Env:            env,
		TimeoutSeconds: timeoutSec,
		WorkingDir:     ar.workDir,
		Entrypoint:     entrypoint,
		Handler:        run.Spec.Handler,
	}, grpc.WaitForReady(true))
	return err
}

func (c *Controller) pollStatus(ctx context.Context, uid string) (*pb.StatusResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.runtimeCli.Status(rctx, &pb.StatusRequest{Id: uid})
}

// ===========================================================================
// Events
// ===========================================================================

func (c *Controller) recordEvent(run *v1alpha1.Run, eventType, reason, messageFmt string, args ...any) {
	if c.Recorder != nil {
		c.Recorder.Eventf(run, eventType, reason, messageFmt, args...)
	}
}

func (c *Controller) scheduleRetryReconcile(run *v1alpha1.Run, backoff time.Duration) {
	if c.rlegCh == nil {
		return
	}
	go func() {
		time.Sleep(backoff)
		select {
		case c.rlegCh <- event.GenericEvent{Object: run}:
		default:
		}
	}()
}

// ===========================================================================
// Startup recovery
// ===========================================================================

func (c *Controller) recoverActiveRuns(ctx context.Context) {
	c.recoverActiveRunsOnce(ctx)
}

func (c *Controller) recoverActiveRunsOnce(ctx context.Context) {
	entries, err := c.listRuntimeExecutionsWithRetry(ctx)
	if err != nil {
		c.Log.Error(err, "failed to list runtime executions for recovery")
		return
	}
	executionIDs := make([]string, 0, len(entries))
	for id := range entries {
		executionIDs = append(executionIDs, id)
	}
	slices.Sort(executionIDs)

	var runs v1alpha1.RunList
	reader := c.RunReader
	if reader == nil {
		reader = c.Client
	}
	if err := reader.List(ctx, &runs, client.InNamespace(podNamespace())); err != nil {
		c.Log.Error(err, "failed to list assigned runs for recovery")
		return
	}

	recovered := 0
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.Status.AssignedPod != c.Hostname || run.Status.Phase != v1alpha1.RunRunning {
			continue
		}
		if _, ok := entries[string(run.UID)]; !ok {
			if _, err := c.reconcileRunningRecovered(ctx, run); err != nil {
				c.Log.Error(err, "failed to reconcile missing runtime execution during recovery", "run", run.Name)
			}
			continue
		}
		c.addRecoveredRun(run)
		recovered++
	}
	if recovered > 0 {
		c.Log.Info("recovered active runs from runtime server", "count", recovered, "executions", executionIDs)
	}
}

func (c *Controller) listRuntimeExecutionsWithRetry(ctx context.Context) (map[string]*pb.StatusResponse, error) {
	var lastErr error
	for {
		entries, err := c.listRuntimeExecutions(ctx)
		if err == nil {
			return entries, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (c *Controller) listRuntimeExecutions(ctx context.Context) (map[string]*pb.StatusResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := c.runtimeCli.List(rctx, &pb.ListRequest{})
	if err != nil {
		return nil, err
	}
	entries := make(map[string]*pb.StatusResponse, len(resp.Entries))
	for _, entry := range resp.Entries {
		if entry == nil || entry.Id == "" {
			continue
		}
		entries[entry.Id] = entry
	}
	return entries, nil
}

func (c *Controller) addRecoveredRun(run *v1alpha1.Run) *activeRun {
	uid := string(run.UID)
	if val, ok := c.activeRuns.Load(uid); ok {
		ar := val.(*activeRun)
		ar.run = run
		if c.rleg != nil {
			c.rleg.UpdateRun(run)
		}
		return ar
	}

	ar := c.buildActiveRun(run)
	c.activeRuns.Store(uid, ar)
	if c.rleg != nil {
		c.rleg.AddRun(run)
	}
	return ar
}

func (c *Controller) buildActiveRun(run *v1alpha1.Run) *activeRun {
	start := time.Now()
	if run.Status.StartTime != nil {
		start = run.Status.StartTime.Time
	}
	ar := &activeRun{
		run:     run,
		start:   start,
		workDir: workDirForRun(run),
	}
	if run.Spec.Timeout != nil && run.Status.StartTime != nil {
		ar.deadline = run.Status.StartTime.Add(run.Spec.Timeout.Duration)
	}
	return ar
}

// ===========================================================================
// Helper functions
// ===========================================================================

func prepareSource(run *v1alpha1.Run) (string, error) {
	runDir := filepath.Join(workspacePath, string(run.UID))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	if run.Spec.Source == nil {
		return runDir, nil
	}
	if run.Spec.Source.Inline != nil {
		fileName := run.Spec.Entrypoint
		if fileName == "" {
			fileName = "script"
		}
		scriptPath := filepath.Join(runDir, fileName)
		if err := os.WriteFile(scriptPath, []byte(*run.Spec.Source.Inline), 0o644); err != nil {
			return "", fmt.Errorf("write inline: %w", err)
		}
		return runDir, nil
	}
	if run.Spec.Source.RepoURL != "" {
		cloneDir := filepath.Join(runDir, "repo")
		args := []string{"clone", run.Spec.Source.RepoURL, cloneDir}
		if run.Spec.Source.CommitSHA == "" {
			args = append(args, "--depth=1")
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = runDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git clone: %w\n%s", err, string(out))
		}
		if run.Spec.Source.CommitSHA != "" {
			cmd := exec.Command("git", "checkout", run.Spec.Source.CommitSHA)
			cmd.Dir = cloneDir
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("git checkout: %w\n%s", err, string(out))
			}
		}
		return cloneDir, nil
	}
	return "", nil
}

func workDirForRun(run *v1alpha1.Run) string {
	runDir := filepath.Join(workspacePath, string(run.UID))
	if run.Spec.Source != nil && run.Spec.Source.RepoURL != "" {
		return filepath.Join(runDir, "repo")
	}
	return runDir
}

func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

// statusAdapter adapts the gRPC client to rlegpkg.StatusProvider.
type statusAdapter struct {
	cli pb.RuntimeClient
}

func (a *statusAdapter) Status(ctx context.Context, uid string) (*pb.StatusResponse, error) {
	return a.cli.Status(ctx, &pb.StatusRequest{Id: uid})
}
