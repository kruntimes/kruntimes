package runtimed

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	rlegpkg "github.com/kruntimes/kruntimes/internal/runtimed/rleg"
)

const (
	workspacePath = "/workspace"
	pollInterval  = 500 * time.Millisecond
)

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
	Log             logr.Logger
	Hostname        string
	RuntimeEndpoint string

	activeRuns sync.Map // uid → *activeRun
	rlegCh     chan event.GenericEvent

	runtimeCli pb.RuntimeClient
	rleg       rlegpkg.RunLifecycleEventGenerator
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

	klog.Infof("runtimed controller started, hostname=%s, runtime=%s", c.Hostname, c.RuntimeEndpoint)
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
			return ok && run.Status.AssignedPod == c.Hostname
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			run, ok := e.ObjectNew.(*v1alpha1.Run)
			return ok && run.Status.AssignedPod == c.Hostname
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

// Reconcile handles a single Run assigned to this pod.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := c.Log.WithValues("run", req.NamespacedName)

	var run v1alpha1.Run
	if err := c.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	uid := string(run.UID)

	switch run.Status.Phase {
	case v1alpha1.RunScheduled:
		if _, exists := c.activeRuns.Load(uid); !exists {
			if ar := c.claimAndExecute(ctx, &run); ar != nil {
				c.activeRuns.Store(uid, ar)
				c.rleg.AddRun(ar.run)
			}
		}

	case v1alpha1.RunRunning:
		val, exists := c.activeRuns.Load(uid)
		if !exists {
			return ctrl.Result{}, nil
		}
		ar := val.(*activeRun)
		ar.run = &run
		c.rleg.UpdateRun(&run)
		c.handleRunning(ctx, ar, uid, log)
	}

	return ctrl.Result{}, nil
}

func (c *Controller) handleRunning(ctx context.Context, ar *activeRun, uid string, log logr.Logger) {
	if ar.run.Spec.CancelRequested {
		_, _ = c.runtimeCli.Cancel(ctx, &pb.CancelRequest{Id: uid})
		c.finishRun(ctx, ar, v1alpha1.RunCancelled, "cancelled by user")
		runsCompleted.WithLabelValues(ar.run.Spec.Runtime, string(v1alpha1.RunCancelled)).Inc()
		return
	}

	if !ar.deadline.IsZero() && time.Now().After(ar.deadline) {
		_, _ = c.runtimeCli.Cancel(ctx, &pb.CancelRequest{Id: uid})
		c.finishRun(ctx, ar, v1alpha1.RunTimeout, fmt.Sprintf("timeout after %s", ar.run.Spec.Timeout.Duration))
		runsCompleted.WithLabelValues(ar.run.Spec.Runtime, string(v1alpha1.RunTimeout)).Inc()
		return
	}

	resp, err := c.pollStatus(ctx, uid)
	if err != nil {
		log.Error(err, "gRPC Status")
		return
	}

	switch resp.State {
	case pb.ExecutionState_EXECUTION_STATE_SUCCEEDED:
		c.finishRun(ctx, ar, v1alpha1.RunSucceeded, resp.Stdout)
		runsCompleted.WithLabelValues(ar.run.Spec.Runtime, string(v1alpha1.RunSucceeded)).Inc()
	case pb.ExecutionState_EXECUTION_STATE_FAILED:
		msg := resp.ErrorMessage
		if msg == "" {
			msg = resp.Stderr
		}
		c.finishRun(ctx, ar, v1alpha1.RunFailed, msg)
		runsCompleted.WithLabelValues(ar.run.Spec.Runtime, string(v1alpha1.RunFailed)).Inc()
	}
}

func (c *Controller) pollStatus(ctx context.Context, uid string) (*pb.StatusResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.runtimeCli.Status(rctx, &pb.StatusRequest{Id: uid})
}

func (c *Controller) claimAndExecute(ctx context.Context, run *v1alpha1.Run) *activeRun {
	run.Status.Phase = v1alpha1.RunRunning
	run.Status.StartTime = &metav1.Time{Time: time.Now()}

	if err := c.Status().Update(ctx, run); err != nil {
		klog.Errorf("claim run %s: %v", run.Name, err)
		return nil
	}

	klog.Infof("Claimed run %s", run.Name)

	ar := &activeRun{
		run:     run,
		start:   time.Now(),
		workDir: filepath.Join(workspacePath, string(run.UID)),
	}

	if run.Spec.Timeout != nil && run.Status.StartTime != nil {
		ar.deadline = run.Status.StartTime.Add(run.Spec.Timeout.Duration)
	}

	go c.execute(ctx, ar)

	return ar
}

func (c *Controller) execute(ctx context.Context, ar *activeRun) {
	run := ar.run

	workDir, err := prepareSource(run)
	if err != nil {
		c.finishRun(ctx, ar, v1alpha1.RunFailed, fmt.Sprintf("prepare source: %v", err))
		runsCompleted.WithLabelValues(run.Spec.Runtime, string(v1alpha1.RunFailed)).Inc()
		c.activeRuns.Delete(string(run.UID))
		return
	}
	ar.workDir = workDir

	env := make(map[string]string)
	for _, e := range run.Spec.Env {
		env[e.Name] = e.Value
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
	_, err = c.runtimeCli.Execute(rctx, &pb.ExecuteRequest{
		Id:             string(run.UID),
		Args:           run.Spec.Args,
		Env:            env,
		TimeoutSeconds: timeoutSec,
		WorkingDir:     workDir,
		Entrypoint:     entrypoint,
		Handler:        run.Spec.Handler,
	})
	cancel()
	if err != nil {
		c.finishRun(ctx, ar, v1alpha1.RunFailed, fmt.Sprintf("runtime Execute: %v", err))
		runsCompleted.WithLabelValues(run.Spec.Runtime, string(v1alpha1.RunFailed)).Inc()
		c.activeRuns.Delete(string(run.UID))
	}
}

// Run starts the controller without a controller-runtime manager (integration tests).
func (c *Controller) Run(ctx context.Context) error {
	c.Log = ctrl.Log.WithName("runtimed")

	conn, err := grpc.NewClient(c.RuntimeEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial runtime %s: %w", c.RuntimeEndpoint, err)
	}
	c.runtimeCli = pb.NewRuntimeClient(conn)
	defer conn.Close()

	c.rleg = rlegpkg.NewGenericRLEG(&statusAdapter{cli: c.runtimeCli}, rlegpkg.DefaultRelistInterval)
	go c.rleg.Start(ctx)

	c.rlegCh = make(chan event.GenericEvent, 256)
	go c.forwardRLEGEvents(ctx)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-c.rlegCh:
				_, _ = c.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ev.Object)})
			}
		}
	}()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var runList v1alpha1.RunList
			if err := c.List(ctx, &runList); err != nil {
				continue
			}
			for _, r := range runList.Items {
				if r.Status.AssignedPod != c.Hostname {
					continue
				}
				uid := string(r.UID)
				if _, exists := c.activeRuns.Load(uid); !exists {
					_, _ = c.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&r)})
				}
			}
		}
	}
}

func (c *Controller) finishRun(ctx context.Context, ar *activeRun, phase v1alpha1.RunPhase, msg string) {
	c.rleg.RemoveRun(string(ar.run.UID))
	c.activeRuns.Delete(string(ar.run.UID))

	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.Run{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(ar.run), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		now := metav1.Now()
		latest.Status.Phase = phase
		latest.Status.Message = msg
		if phase == v1alpha1.RunSucceeded {
			latest.Status.Outputs = readOutputs(ar.workDir)
		}
		latest.Status.CompletionTime = &now
		return c.Status().Update(ctx, latest)
	})
	runDuration.WithLabelValues(ar.run.Spec.Runtime).Observe(time.Since(ar.start).Seconds())
}

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

func readOutputs(workingDir string) map[string]string {
	if workingDir == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(workingDir, "outputs"))
	if err != nil {
		return nil
	}
	defer f.Close()

	outputs := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			outputs[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	if err := scanner.Err(); err != nil {
		klog.Warningf("error reading outputs file %s: %v", workingDir, err)
	}
	return outputs
}

// statusAdapter adapts the gRPC client to rlegpkg.StatusProvider.
type statusAdapter struct {
	cli pb.RuntimeClient
}

func (a *statusAdapter) Status(ctx context.Context, uid string) (*pb.StatusResponse, error) {
	return a.cli.Status(ctx, &pb.StatusRequest{Id: uid})
}
