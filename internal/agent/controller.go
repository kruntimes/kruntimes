package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/airconduct/kruntime/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/airconduct/kruntime/api/taskruntime/v1"
)

var (
	tasksCompleted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kruntime_agent_tasks_total",
			Help: "Total number of tasks completed by this agent.",
		},
		[]string{"runtime", "result"},
	)
	taskDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kruntime_agent_task_duration_seconds",
			Help:    "Task execution duration.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"runtime"},
	)
	claimConflicts = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "kruntime_agent_claim_conflicts_total",
			Help: "Total number of task claim conflicts.",
		},
	)
)

// Controller watches for Tasks assigned to this pod and delegates
// execution to the runtime via gRPC.
type Controller struct {
	Client          client.Client
	Hostname        string
	RuntimeEndpoint string
	Workers         int

	wg         sync.WaitGroup
	runtimeCli pb.TaskRuntimeClient
}

// Run starts the controller and blocks until ctx is done.
func (c *Controller) Run(ctx context.Context) error {
	if c.Workers <= 0 {
		c.Workers = 2
	}

	conn, err := grpc.NewClient(c.RuntimeEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial runtime %s: %w", c.RuntimeEndpoint, err)
	}
	defer conn.Close()
	c.runtimeCli = pb.NewTaskRuntimeClient(conn)

	klog.Infof("Agent controller starting, hostname=%s, runtime=%s, workers=%d",
		c.Hostname, c.RuntimeEndpoint, c.Workers)

	sem := make(chan struct{}, c.Workers)

	wait.UntilWithContext(ctx, func(ctx context.Context) {
		task, err := c.claimTask(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			klog.V(4).Infof("No task claimed: %v", err)
			return
		}
		if task == nil {
			return
		}

		sem <- struct{}{}
		c.wg.Add(1)
		go func(t *v1alpha1.Task) {
			defer c.wg.Done()
			defer func() { <-sem }()
			c.executeTask(ctx, t)
		}(task)
	}, 500*time.Millisecond)

	c.wg.Wait()
	return nil
}

func (c *Controller) claimTask(ctx context.Context) (*v1alpha1.Task, error) {
	var tasks v1alpha1.TaskList
	if err := c.Client.List(ctx, &tasks); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	for i := range tasks.Items {
		task := &tasks.Items[i]
		if task.Status.AssignedPod != c.Hostname || task.Status.Phase != v1alpha1.TaskScheduled {
			continue
		}

		task.Status.Phase = v1alpha1.TaskRunning
		task.Status.StartTime = &metav1.Time{Time: time.Now()}

		if err := c.Client.Status().Update(ctx, task); err != nil {
			if apierrors.IsConflict(err) {
				claimConflicts.Inc()
				continue
			}
			klog.Errorf("Failed to claim task %s: %v", task.Name, err)
			continue
		}

		klog.Infof("Claimed task %s", task.Name)
		return task, nil
	}

	return nil, nil
}

func (c *Controller) executeTask(ctx context.Context, task *v1alpha1.Task) {
	start := time.Now()
	defer func() {
		taskDuration.WithLabelValues(task.Spec.Runtime).Observe(time.Since(start).Seconds())
	}()

	env := make(map[string]string)
	for _, e := range task.Spec.Env {
		env[e.Name] = e.Value
	}
	var timeoutSec int64
	if task.Spec.Timeout != nil {
		timeoutSec = int64(task.Spec.Timeout.Duration.Seconds())
	}

	// Delegate to runtime via gRPC
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := c.runtimeCli.CreateTask(rctx, &pb.CreateTaskRequest{
		Id:             string(task.UID),
		Commands:       task.Spec.Commands,
		Env:            env,
		TimeoutSeconds: timeoutSec,
	})
	cancel()
	if err != nil {
		c.updateTaskStatus(context.Background(), task, v1alpha1.TaskFailed, fmt.Sprintf("runtime CreateTask: %v", err))
		tasksCompleted.WithLabelValues(task.Spec.Runtime, string(v1alpha1.TaskFailed)).Inc()
		return
	}

	// Poll for completion
	for {
		select {
		case <-ctx.Done():
			c.updateTaskStatus(context.Background(), task, v1alpha1.TaskFailed, "agent shutting down")
			tasksCompleted.WithLabelValues(task.Spec.Runtime, string(v1alpha1.TaskFailed)).Inc()
			return
		default:
		}

		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := c.runtimeCli.GetTask(rctx, &pb.GetTaskRequest{Id: string(task.UID)})
		cancel()
		if err != nil {
			c.updateTaskStatus(context.Background(), task, v1alpha1.TaskFailed, fmt.Sprintf("runtime GetTask: %v", err))
			tasksCompleted.WithLabelValues(task.Spec.Runtime, string(v1alpha1.TaskFailed)).Inc()
			return
		}

		switch resp.State {
		case pb.TaskState_TASK_STATE_SUCCEEDED:
			c.updateTaskStatus(context.Background(), task, v1alpha1.TaskSucceeded, resp.Stdout)
			tasksCompleted.WithLabelValues(task.Spec.Runtime, string(v1alpha1.TaskSucceeded)).Inc()
			klog.Infof("Task %s succeeded", task.Name)
			return
		case pb.TaskState_TASK_STATE_FAILED:
			msg := resp.ErrorMessage
			if msg == "" {
				msg = resp.Stderr
			}
			c.updateTaskStatus(context.Background(), task, v1alpha1.TaskFailed, msg)
			tasksCompleted.WithLabelValues(task.Spec.Runtime, string(v1alpha1.TaskFailed)).Inc()
			klog.Infof("Task %s failed: %s", task.Name, msg)
			return
		}

		time.Sleep(200 * time.Millisecond)
	}
}

func (c *Controller) updateTaskStatus(ctx context.Context, task *v1alpha1.Task, phase v1alpha1.TaskPhase, msg string) {
	retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.Task{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		now := metav1.Now()
		latest.Status.Phase = phase
		latest.Status.Message = msg
		latest.Status.CompletionTime = &now
		return c.Client.Status().Update(ctx, latest)
	})
}
