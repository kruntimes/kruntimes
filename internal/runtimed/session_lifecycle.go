package runtimed

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
)

const sessionRegistrationTimeout = 10 * time.Second

func (c *Controller) startSessionRegistrationAsync(ar *activeRun) {
	if ar == nil || ar.run == nil || !ar.beginSessionRegistration() {
		return
	}
	go func() {
		ctx := context.Background()
		if err := c.prepareSession(ctx, ar); err != nil {
			c.finishSessionRegistration(ar, runretry.ReasonPrepareSource, fmt.Errorf("prepare session: %w", err))
			return
		}
		if err := c.registerSession(ctx, ar); err != nil {
			c.finishSessionRegistration(ar, runretry.ReasonSessionRegister, fmt.Errorf("register session: %w", err))
			return
		}
		c.finishSessionRegistration(ar, "", nil)
	}()
}

func (c *Controller) finishSessionRegistration(ar *activeRun, reason string, err error) {
	var failure *sessionRegistrationFailure
	if err != nil {
		failure = &sessionRegistrationFailure{reason: reason, message: err.Error()}
		c.Log.Error(err, "local Session registration failed", "run", client.ObjectKeyFromObject(ar.run))
		c.recordEvent(ar.run, corev1.EventTypeWarning, "SessionRegistrationFailed", "%s", err)
	}
	ar.finishSessionRegistration(failure)
	c.enqueueRun(ar.run)
}

func (c *Controller) prepareSession(ctx context.Context, ar *activeRun) error {
	if ar == nil || ar.run == nil {
		return fmt.Errorf("active Run is required")
	}
	if ar.prepared.Load() {
		return nil
	}
	if err := prepareSource(ar); err != nil {
		return err
	}
	if _, err := c.prepareArtifactStaging(ar); err != nil {
		return err
	}
	if err := c.stageArtifactInputs(ctx, ar); err != nil {
		return err
	}
	ar.prepared.Store(true)
	return nil
}

func (c *Controller) registerSession(ctx context.Context, ar *activeRun) error {
	if c.sessionCli == nil {
		return fmt.Errorf("SessionRuntime client is not configured")
	}
	artifactsDir := ""
	if c.ArtifactStore != nil {
		artifactsDir = ar.artifactDir
	}
	request, err := sessionRegistrationRequest(ar.run, ar.workDir, artifactsDir)
	if err != nil {
		return err
	}
	registrationCtx, cancel := context.WithTimeout(ctx, sessionRegistrationTimeout)
	defer cancel()
	response, err := c.sessionCli.RegisterSession(registrationCtx, request, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	if response.GetState() != pb.SessionState_SESSION_STATE_READY {
		return fmt.Errorf("Runtime Server returned session state %s", response.GetState())
	}
	return nil
}

// reconcileSessionRegistration observes local Runtime Server state and is the
// only place registration changes Run status. Registration itself is async so
// source preparation and the gRPC call never block the controller worker.
func (c *Controller) reconcileSessionRegistration(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	if ar == nil || ar.run == nil {
		return ctrl.Result{}, fmt.Errorf("active Run is required")
	}
	if c.sessionCli == nil {
		return ctrl.Result{}, fmt.Errorf("SessionRuntime client is not configured")
	}
	identity, err := sessionIdentityForRun(ar.run)
	if err != nil {
		return c.applyFailure(ctx, ar, runretry.ReasonSessionRegister, err.Error())
	}
	statusCtx, cancel := context.WithTimeout(ctx, sessionRegistrationTimeout)
	defer cancel()
	response, err := c.sessionCli.GetSessionStatus(statusCtx, &pb.GetSessionStatusRequest{Identity: identity})
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return ctrl.Result{}, fmt.Errorf("get SessionRuntime registration status: %w", err)
		}
		if failure := ar.consumeSessionRegistrationFailure(); failure != nil {
			return c.applyFailure(ctx, ar, failure.reason, failure.message)
		}
		if !ar.sessionRegistrationInFlight() {
			c.startSessionRegistrationAsync(ar)
		}
		return ctrl.Result{RequeueAfter: activeRunRequeueAfter(ar)}, nil
	}

	switch response.GetState() {
	case pb.SessionState_SESSION_STATE_READY:
		return c.applySessionReady(ctx, ar)
	case pb.SessionState_SESSION_STATE_REGISTERING:
		// TODO: Support Runtime Server-side asynchronous session registration.
		// Current built-in Runtime Servers register synchronously and return
		// READY; this state is reserved for a future versioned contract.
		return ctrl.Result{RequeueAfter: activeRunRequeueAfter(ar)}, nil
	case pb.SessionState_SESSION_STATE_CLOSING, pb.SessionState_SESSION_STATE_CLOSED, pb.SessionState_SESSION_STATE_FAILED, pb.SessionState_SESSION_STATE_UNSPECIFIED:
		return c.applyFailure(ctx, ar, runretry.ReasonSessionRegister, fmt.Sprintf("Runtime Server session is %s", response.GetState()))
	default:
		return c.applyFailure(ctx, ar, runretry.ReasonSessionRegister, fmt.Sprintf("Runtime Server returned unknown session state %s", response.GetState()))
	}
}

func (c *Controller) applySessionReady(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	run := ar.run
	if run.Status.Phase != v1alpha1.RunRunning || run.Status.AssignedPod != c.PodName {
		return ctrl.Result{}, nil
	}
	run.Status.Phase = v1alpha1.RunReady
	run.Status.Message = "session registered"
	if endpoint := c.sessionEndpoint(run); endpoint != nil {
		run.Status.Endpoint = endpoint
	}
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               runstatus.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "SessionRegistered",
		Message:            "session is ready for Console operations",
		LastTransitionTime: metav1.Now(),
	})
	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	if c.SessionOperations != nil {
		if err := c.SessionOperations.Ensure(run, time.Now()); err != nil {
			return ctrl.Result{}, fmt.Errorf("start Session idle tracking: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: activeRunRequeueAfter(ar)}, nil
}

func (c *Controller) sessionEndpoint(run *v1alpha1.Run) *v1alpha1.RunEndpoint {
	if run == nil || c.ConsoleURL == "" {
		return nil
	}
	protocol := v1alpha1.RunEndpointProtocolHTTP
	if strings.HasPrefix(strings.ToLower(c.ConsoleURL), "https://") {
		protocol = v1alpha1.RunEndpointProtocolHTTPS
	}
	endpoint := &v1alpha1.RunEndpoint{
		Protocol: protocol,
		URL: fmt.Sprintf(
			"%s/v1/namespaces/%s/runtimes/%s/sessions/%s",
			strings.TrimRight(c.ConsoleURL, "/"),
			run.Namespace,
			run.Spec.Runtime,
			run.UID,
		),
	}
	if protocol == v1alpha1.RunEndpointProtocolHTTPS {
		endpoint.CABundle = slices.Clone(c.ConsoleCABundle)
	}
	return endpoint
}

func (c *Controller) reconcileSessionRecovery(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	ar := c.buildActiveRun(run)
	identity, err := sessionIdentityForRun(run)
	if err != nil {
		return c.applyTerminal(ctx, ar, v1alpha1.RunFailed, runretry.ReasonExecutionLost, err.Error())
	}
	if c.sessionCli == nil {
		return ctrl.Result{}, fmt.Errorf("SessionRuntime client is not configured")
	}
	recoveryCtx, cancel := context.WithTimeout(ctx, sessionRegistrationTimeout)
	defer cancel()
	response, err := c.sessionCli.GetSessionStatus(recoveryCtx, &pb.GetSessionStatusRequest{Identity: identity})
	if err == nil {
		if response.GetState() == pb.SessionState_SESSION_STATE_READY {
			if !c.tryClaimActiveRun(ar) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if c.SessionOperations != nil {
				activity := time.Now()
				if lastActivity := response.GetLastActivityUnixNano(); lastActivity > 0 {
					activity = time.Unix(0, lastActivity)
				}
				if err := c.SessionOperations.Ensure(run, activity); err != nil {
					return ctrl.Result{}, fmt.Errorf("restore Session idle tracking: %w", err)
				}
				if leaseHeartbeat := response.GetLastLeaseHeartbeatUnixNano(); leaseHeartbeat > 0 {
					c.SessionOperations.TouchLease(string(run.UID), time.Unix(0, leaseHeartbeat))
				}
			}
			c.recordActiveRuns(run.Spec.Runtime)
			if run.Status.Phase == v1alpha1.RunRunning {
				return c.applySessionReady(ctx, ar)
			}
			return ctrl.Result{RequeueAfter: activeRunRequeueAfter(ar)}, nil
		}
		return c.applyTerminal(ctx, ar, v1alpha1.RunFailed, runretry.ReasonExecutionLost, fmt.Sprintf("runtime session recovered in state %s", response.GetState()))
	}
	if status.Code(err) != codes.NotFound {
		return ctrl.Result{}, fmt.Errorf("get SessionRuntime status after runtimed restart: %w", err)
	}
	if run.Status.Phase == v1alpha1.RunRunning {
		if !c.tryClaimActiveRun(ar) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		c.recordActiveRuns(run.Spec.Runtime)
		return c.reconcileSessionRegistration(ctx, ar)
	}
	return c.applyTerminal(ctx, ar, v1alpha1.RunFailed, runretry.ReasonExecutionLost, "runtime session not found after runtimed restart")
}

func (c *Controller) closeSessionAndApplyTerminal(ctx context.Context, ar *activeRun, phase v1alpha1.RunPhase, reason, message string) (ctrl.Result, error) {
	if err := c.ensureActiveSessionClosed(ctx, ar); err != nil {
		c.Log.Error(err, "failed to close Session before terminal transition; retrying", "run", client.ObjectKeyFromObject(ar.run))
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return c.applyTerminal(ctx, ar, phase, reason, message)
}

// beginSessionFinalization fences new Console operations by moving the Run to
// Finalizing. Owner runtimed then drains operations already accepted by its
// local FIFO queue before closing the Runtime Server session.
func (c *Controller) beginSessionFinalization(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	if ar == nil || ar.run == nil || ar.run.Spec.Mode.Session == nil {
		return ctrl.Result{}, fmt.Errorf("Session Run is required for finalization")
	}
	run := ar.run
	if run.Status.Phase != v1alpha1.RunRunning && run.Status.Phase != v1alpha1.RunReady {
		return ctrl.Result{}, nil
	}
	run.Status.Phase = v1alpha1.RunFinalizing
	run.Status.Message = "session finalization requested"
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               runstatus.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "Finalizing",
		Message:            "session is no longer accepting new operations",
		LastTransitionTime: metav1.Now(),
	})
	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (c *Controller) reconcileFinalizing(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	if run.Spec.Mode.Session == nil {
		return c.applyTerminal(ctx, c.buildActiveRun(run), v1alpha1.RunFailed, runretry.ReasonExecutionLost, "non-Session Run entered Finalizing")
	}
	uid := string(run.UID)
	value, exists := c.activeRuns.Load(uid)
	if !exists {
		return c.reconcileSessionRecovery(ctx, run)
	}
	ar := value.(*activeRun)
	ar.run = run
	if run.Spec.HasImmediateTermination() {
		return c.closeSessionAndApplyTerminal(ctx, ar, v1alpha1.RunCancelled, runretry.ReasonCancelled, "cancelled by user")
	}
	now := time.Now()
	if !ar.deadline.IsZero() && !now.Before(ar.deadline) {
		return c.closeSessionAndApplyTerminal(ctx, ar, v1alpha1.RunTimeout, runretry.ReasonTimeout, fmt.Sprintf("timeout after %s", run.Spec.Timeout.Duration))
	}
	if c.SessionOperations != nil && !c.SessionOperations.Drain(uid) {
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}
	if err := c.ensureActiveSessionClosed(ctx, ar); err != nil {
		c.Log.Error(err, "failed to close Session before artifact export; retrying", "run", client.ObjectKeyFromObject(ar.run))
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return c.completeFinalizingSession(ctx, ar)
}

// completeFinalizingSession exports the immutable final artifact directory
// only after the local Session Runtime has stopped accepting operations.
// Transient store errors leave the Run Finalizing so the same directory can be
// retried without admitting further Console work.
func (c *Controller) completeFinalizingSession(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	artifactRefs, err := c.collectArtifacts(ctx, ar)
	if err != nil {
		if isArtifactInvalid(err) {
			return c.applyTerminal(ctx, ar, v1alpha1.RunFailed, "ArtifactInvalid", err.Error())
		}
		c.Log.Error(err, "Session artifact collection failed; retrying", "run", client.ObjectKeyFromObject(ar.run))
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	ar.run.Status.ArtifactRefs = artifactRefs
	return c.applyTerminal(ctx, ar, v1alpha1.RunSucceeded, "SessionCompleted", "session finalized")
}

// ensureActiveSessionClosed closes the Runtime Server session exactly once.
// A Finalizing Run must observe a successful close before its artifact
// directory becomes immutable and safe to export.
func (c *Controller) ensureActiveSessionClosed(ctx context.Context, ar *activeRun) error {
	if ar == nil || ar.run == nil {
		return nil
	}
	ar.sessionCloseMu.Lock()
	defer ar.sessionCloseMu.Unlock()
	if ar.sessionClosed.Load() {
		return nil
	}
	if err := c.closeRuntimeSession(ctx, ar.run); err != nil {
		return err
	}
	ar.sessionClosed.Store(true)
	return nil
}

func (c *Controller) closeRuntimeSession(ctx context.Context, run *v1alpha1.Run) error {
	if run == nil {
		return nil
	}
	if c.SessionOperations != nil {
		c.SessionOperations.Close(string(run.UID))
	}
	if c.sessionCli == nil {
		return fmt.Errorf("SessionRuntime client is not configured")
	}
	identity, err := sessionIdentityForRun(run)
	if err != nil {
		return fmt.Errorf("build SessionRuntime close request: %w", err)
	}
	closeCtx, cancel := context.WithTimeout(ctx, c.sessionCloseTimeout())
	defer cancel()
	_, err = c.sessionCli.CloseSession(closeCtx, &pb.CloseSessionRequest{Identity: identity})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("close Runtime Server session: %w", err)
	}
	return nil
}

func (c *Controller) sessionCloseTimeout() time.Duration {
	if c.SessionCloseTimeout > 0 {
		return c.SessionCloseTimeout
	}
	return executionCleanupTimeout
}
