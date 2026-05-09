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

// Controller watches for Tasks assigned to this pod and executes them.
type Controller struct {
	Client   client.Client
	Hostname string
	Executor *Executor
	Workers  int

	wg sync.WaitGroup
}

// Run starts the controller and blocks until ctx is done. It polls for assigned
// tasks at a configurable interval.
func (c *Controller) Run(ctx context.Context) error {
	if c.Workers <= 0 {
		c.Workers = 2
	}

	sem := make(chan struct{}, c.Workers)

	klog.Infof("Agent controller starting, hostname=%s, workers=%d", c.Hostname, c.Workers)

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

	if task.Spec.Timeout != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Spec.Timeout.Duration)
		defer cancel()
	}

	result := c.Executor.Execute(ctx, task)

	retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.Task{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		now := metav1.Now()
		latest.Status.Phase = result.Phase
		latest.Status.Message = result.Message
		latest.Status.CompletionTime = &now
		return c.Client.Status().Update(ctx, latest)
	})

	tasksCompleted.WithLabelValues(task.Spec.Runtime, string(result.Phase)).Inc()
	klog.Infof("Task %s completed: phase=%s", task.Name, result.Phase)
}
