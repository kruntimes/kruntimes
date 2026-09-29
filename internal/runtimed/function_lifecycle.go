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

const functionRegistrationTimeout = 10 * time.Second

func (c *Controller) functionEndpoint(run *v1alpha1.Run) *v1alpha1.RunEndpoint {
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
			"%s/v1/namespaces/%s/runtimes/%s/functions/%s:invoke",
			strings.TrimRight(c.ConsoleURL, "/"), run.Namespace, run.Spec.Runtime, run.UID,
		),
	}
	if protocol == v1alpha1.RunEndpointProtocolHTTPS {
		endpoint.CABundle = slices.Clone(c.ConsoleCABundle)
	}
	return endpoint
}

func (c *Controller) reconcileRunningFunction(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	value, exists := c.activeRuns.Load(string(run.UID))
	if !exists {
		ar := c.buildActiveRun(run)
		if !c.tryClaimActiveRun(ar) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		c.recordActiveRuns(run.Spec.Runtime)
		return c.reconcileFunctionRegistration(ctx, ar)
	}
	ar := value.(*activeRun)
	ar.run = run
	if run.Spec.HasImmediateTermination() {
		return c.closeFunctionAndApplyTerminal(ctx, ar, v1alpha1.RunCancelled, runretry.ReasonCancelled, "cancelled by user")
	}
	if !ar.deadline.IsZero() && !time.Now().Before(ar.deadline) {
		return c.closeFunctionAndApplyTerminal(ctx, ar, v1alpha1.RunTimeout, runretry.ReasonTimeout, fmt.Sprintf("timeout after %s", run.Spec.Timeout.Duration))
	}
	condition := meta.FindStatusCondition(run.Status.Conditions, runstatus.ConditionRunning)
	if condition != nil && condition.Status == metav1.ConditionFalse {
		return c.reconcileRetryBackoff(ctx, ar)
	}
	return c.reconcileFunctionRegistration(ctx, ar)
}

func (c *Controller) reconcileReadyFunction(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	value, exists := c.activeRuns.Load(string(run.UID))
	if !exists {
		ar := c.buildActiveRun(run)
		if !c.tryClaimActiveRun(ar) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		c.recordActiveRuns(run.Spec.Runtime)
		return c.reconcileFunctionRegistration(ctx, ar)
	}
	ar := value.(*activeRun)
	ar.run = run
	if run.Spec.HasImmediateTermination() {
		return c.closeFunctionAndApplyTerminal(ctx, ar, v1alpha1.RunCancelled, runretry.ReasonCancelled, "cancelled by user")
	}
	return c.reconcileFunctionRegistration(ctx, ar)
}

func (c *Controller) startFunctionRegistrationAsync(ar *activeRun) {
	if ar == nil || ar.run == nil || !ar.beginFunctionRegistration() {
		return
	}
	go func() {
		ctx := context.Background()
		if err := c.prepareFunction(ctx, ar); err != nil {
			c.finishFunctionRegistration(ar, nil, runretry.ReasonPrepareSource, fmt.Errorf("prepare function: %w", err))
			return
		}
		registration, err := c.registerFunction(ctx, ar)
		if err != nil {
			c.finishFunctionRegistration(ar, nil, runretry.ReasonRuntimeExecute, fmt.Errorf("register function: %w", err))
			return
		}
		c.finishFunctionRegistration(ar, registration, "", nil)
	}()
}

func (c *Controller) finishFunctionRegistration(ar *activeRun, registration *pb.FunctionRegistration, reason string, err error) {
	var failure *functionRegistrationFailure
	if err != nil {
		failure = &functionRegistrationFailure{reason: reason, message: err.Error()}
		c.Log.Error(err, "local Function registration failed", "run", client.ObjectKeyFromObject(ar.run))
		c.recordEvent(ar.run, corev1.EventTypeWarning, "FunctionRegistrationFailed", "%s", err)
	}
	ar.finishFunctionRegistration(registration, failure)
	c.enqueueRun(ar.run)
}

func (c *Controller) prepareFunction(ctx context.Context, ar *activeRun) error {
	if ar == nil || ar.run == nil {
		return fmt.Errorf("active Run is required")
	}
	if ar.prepared.Load() {
		return nil
	}
	if err := prepareSource(ar); err != nil {
		return err
	}
	if err := c.stageArtifactInputs(ctx, ar); err != nil {
		return err
	}
	ar.prepared.Store(true)
	return nil
}

func (c *Controller) registerFunction(ctx context.Context, ar *activeRun) (*pb.FunctionRegistration, error) {
	if c.functionCli == nil {
		return nil, fmt.Errorf("FunctionRuntime client is not configured")
	}
	request, err := functionRegistrationRequest(ar.run, ar.workDir, ar.outputPath)
	if err != nil {
		return nil, err
	}
	registrationCtx, cancel := context.WithTimeout(ctx, functionRegistrationTimeout)
	defer cancel()
	response, err := c.functionCli.RegisterFunction(registrationCtx, request, grpc.WaitForReady(true))
	if err != nil {
		return nil, err
	}
	if response.GetRegistration() == nil || response.GetRegistration().GetRunUid() != string(ar.run.UID) || response.GetRegistration().GetRegistrationId() == "" {
		return nil, fmt.Errorf("Runtime Server returned invalid function registration")
	}
	return response.GetRegistration(), nil
}

func (c *Controller) reconcileFunctionRegistration(ctx context.Context, ar *activeRun) (ctrl.Result, error) {
	if ar == nil || ar.run == nil {
		return ctrl.Result{}, fmt.Errorf("active Run is required")
	}
	if c.functionCli == nil {
		return ctrl.Result{}, fmt.Errorf("FunctionRuntime client is not configured")
	}
	registration := ar.functionRegistrationRef()
	if registration == nil {
		if failure := ar.consumeFunctionRegistrationFailure(); failure != nil {
			return c.applyFunctionRegistrationFailure(ctx, ar, failure.reason, failure.message)
		}
		if !ar.functionRegistrationInFlight() {
			c.startFunctionRegistrationAsync(ar)
		}
		return ctrl.Result{RequeueAfter: activeRunRequeueAfter(ar)}, nil
	}
	statusCtx, cancel := context.WithTimeout(ctx, functionRegistrationTimeout)
	defer cancel()
	response, err := c.functionCli.FunctionStatus(statusCtx, &pb.FunctionStatusRequest{Registration: registration})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return c.applyFunctionRegistrationFailure(ctx, ar, runretry.ReasonRuntimeExecute, "function registration not found")
		}
		return ctrl.Result{}, fmt.Errorf("get FunctionRuntime registration status: %w", err)
	}
	if response.GetRegistration() == nil || response.GetRegistration().GetRunUid() != string(ar.run.UID) || response.GetRegistration().GetRegistrationId() != registration.GetRegistrationId() {
		return c.applyFunctionRegistrationFailure(ctx, ar, runretry.ReasonRuntimeExecute, "Runtime Server returned mismatched function registration")
	}
	switch response.GetState() {
	case pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY:
		return c.reconcileFunctionReadyStatus(ctx, ar, response)
	case pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_REGISTERING:
		return ctrl.Result{RequeueAfter: activeRunRequeueAfter(ar)}, nil
	case pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_FAILED:
		return c.applyFunctionRegistrationFailure(ctx, ar, runretry.ReasonRuntimeExecute, boundedStatusMessage(response.GetFatalError()))
	default:
		return c.applyFunctionRegistrationFailure(ctx, ar, runretry.ReasonRuntimeExecute, fmt.Sprintf("Runtime Server function registration is %s", response.GetState()))
	}
}

func (c *Controller) reconcileFunctionReadyStatus(ctx context.Context, ar *activeRun, response *pb.FunctionStatusResponse) (ctrl.Result, error) {
	if ar == nil || ar.run == nil || response == nil {
		return ctrl.Result{}, fmt.Errorf("active Function Run status is required")
	}
	now := time.Now()
	if !ar.deadline.IsZero() {
		if !now.Before(ar.deadline) {
			return c.closeFunctionAndApplyTerminal(ctx, ar, v1alpha1.RunTimeout, runretry.ReasonTimeout, fmt.Sprintf("timeout after %s", ar.run.Spec.Timeout.Duration))
		}
	}

	requeueAfter := activeRunRequeueAfter(ar)
	if idleTimeout := ar.run.Spec.Mode.Function.IdleTimeoutSeconds; idleTimeout != nil {
		lastActivity := time.Unix(0, response.GetLastActivityUnixNano())
		if response.GetLastActivityUnixNano() <= 0 {
			return c.applyFunctionRegistrationFailure(ctx, ar, runretry.ReasonRuntimeExecute, "Runtime Server returned no function activity timestamp")
		}
		idleDeadline := lastActivity.Add(time.Duration(*idleTimeout) * time.Second)
		if !now.Before(idleDeadline) {
			return c.closeFunctionAndApplyTerminal(ctx, ar, v1alpha1.RunTimeout, runretry.ReasonTimeout, "function idle timeout exceeded")
		}
		requeueAfter = min(requeueAfter, time.Until(idleDeadline))
	}
	return c.applyFunctionReady(ctx, ar, requeueAfter)
}

func (c *Controller) applyFunctionReady(ctx context.Context, ar *activeRun, requeueAfter time.Duration) (ctrl.Result, error) {
	run := ar.run
	if (run.Status.Phase != v1alpha1.RunRunning && run.Status.Phase != v1alpha1.RunReady) || run.Status.AssignedPod != c.PodName {
		return ctrl.Result{}, nil
	}
	if run.Status.Phase == v1alpha1.RunReady {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	run.Status.Phase = v1alpha1.RunReady
	run.Status.Message = "function registered"
	if endpoint := c.functionEndpoint(run); endpoint != nil {
		run.Status.Endpoint = endpoint
	}
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{Type: runstatus.ConditionReady, Status: metav1.ConditionTrue, Reason: "FunctionRegistered", Message: "function is ready for invocation", LastTransitionTime: metav1.Now()})
	if err := c.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// applyFunctionRegistrationFailure lets the generic retry engine own status
// transitions, then drops the local opaque handle when it scheduled another
// registration attempt. Keeping that handle would otherwise make the next
// attempt poll the failed generation forever instead of registering anew.
func (c *Controller) applyFunctionRegistrationFailure(ctx context.Context, ar *activeRun, reason, message string) (ctrl.Result, error) {
	result, err := c.applyFailure(ctx, ar, reason, message)
	if err == nil && ar.run.Status.Phase == v1alpha1.RunRunning {
		ar.resetFunctionRegistration()
	}
	return result, err
}

func (c *Controller) closeFunctionAndApplyTerminal(ctx context.Context, ar *activeRun, phase v1alpha1.RunPhase, reason, message string) (ctrl.Result, error) {
	if err := c.ensureActiveFunctionClosed(ctx, ar); err != nil {
		c.Log.Error(err, "failed to unregister Function before terminal transition; retrying", "run", client.ObjectKeyFromObject(ar.run))
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return c.applyTerminal(ctx, ar, phase, reason, message)
}

func (c *Controller) ensureActiveFunctionClosed(ctx context.Context, ar *activeRun) error {
	if ar == nil || ar.run == nil {
		return nil
	}
	ar.functionCloseMu.Lock()
	defer ar.functionCloseMu.Unlock()
	if ar.functionClosed.Load() {
		return nil
	}
	registration := ar.functionRegistrationRef()
	if registration == nil {
		if ar.functionRegistrationInFlight() {
			return fmt.Errorf("function registration is still in progress")
		}
		if err := c.prepareFunction(ctx, ar); err != nil {
			return fmt.Errorf("prepare function for cleanup: %w", err)
		}
		var err error
		registration, err = c.registerFunction(ctx, ar)
		if err != nil {
			return fmt.Errorf("recover function registration for cleanup: %w", err)
		}
		ar.finishFunctionRegistration(registration, nil)
	}
	if c.functionCli == nil {
		return fmt.Errorf("FunctionRuntime client is not configured")
	}
	closeCtx, cancel := context.WithTimeout(ctx, functionRegistrationTimeout)
	defer cancel()
	_, err := c.functionCli.UnregisterFunction(closeCtx, &pb.UnregisterFunctionRequest{Registration: registration, CancelInFlight: true})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("unregister Runtime Server function: %w", err)
	}
	ar.functionClosed.Store(true)
	return nil
}
