package runtimed

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
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
	claimConflicts = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "kruntimes_runtimed_claim_conflicts_total",
			Help: "Total number of run claim conflicts.",
		},
	)
)

// Controller watches for Runs assigned to this pod and delegates
// execution to the runtime via gRPC.
type Controller struct {
	Client          client.Client
	Hostname        string
	RuntimeEndpoint string
	Workers         int

	wg         sync.WaitGroup
	runtimeCli pb.RuntimeClient
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
	c.runtimeCli = pb.NewRuntimeClient(conn)

	klog.Infof("runtimed controller starting, hostname=%s, runtime=%s, workers=%d",
		c.Hostname, c.RuntimeEndpoint, c.Workers)

	sem := make(chan struct{}, c.Workers)

	wait.UntilWithContext(ctx, func(ctx context.Context) {
		run, err := c.claimRun(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			klog.V(4).Infof("No run claimed: %v", err)
			return
		}
		if run == nil {
			return
		}

		sem <- struct{}{}
		c.wg.Add(1)
		go func(r *v1alpha1.Run) {
			defer c.wg.Done()
			defer func() { <-sem }()
			c.executeRun(ctx, r)
		}(run)
	}, 500*time.Millisecond)

	c.wg.Wait()
	return nil
}

func (c *Controller) claimRun(ctx context.Context) (*v1alpha1.Run, error) {
	var runList v1alpha1.RunList
	if err := c.Client.List(ctx, &runList); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	for i := range runList.Items {
		run := &runList.Items[i]
		if run.Status.AssignedPod != c.Hostname || run.Status.Phase != v1alpha1.RunScheduled {
			continue
		}

		run.Status.Phase = v1alpha1.RunRunning
		run.Status.StartTime = &metav1.Time{Time: time.Now()}

		if err := c.Client.Status().Update(ctx, run); err != nil {
			if apierrors.IsConflict(err) {
				claimConflicts.Inc()
				continue
			}
			klog.Errorf("Failed to claim run %s: %v", run.Name, err)
			continue
		}

		klog.Infof("Claimed run %s", run.Name)
		return run, nil
	}

	return nil, nil
}

func (c *Controller) executeRun(ctx context.Context, run *v1alpha1.Run) {
	start := time.Now()
	defer func() {
		runDuration.WithLabelValues(run.Spec.Runtime).Observe(time.Since(start).Seconds())
	}()

	env := make(map[string]string)
	for _, e := range run.Spec.Env {
		env[e.Name] = e.Value
	}
	var timeoutSec int64
	if run.Spec.Timeout != nil {
		timeoutSec = int64(run.Spec.Timeout.Duration.Seconds())
	}

	// Map Source fields to proto
	var sourceInline, sourceRepoURL, sourceCommitSHA string
	if run.Spec.Source != nil {
		if run.Spec.Source.Inline != nil {
			sourceInline = *run.Spec.Source.Inline
		}
		sourceRepoURL = run.Spec.Source.RepoURL
		sourceCommitSHA = run.Spec.Source.CommitSHA
	}

	// Delegate to runtime via gRPC
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := c.runtimeCli.Execute(rctx, &pb.ExecuteRequest{
		Id:              string(run.UID),
		Args:            run.Spec.Args,
		Env:             env,
		TimeoutSeconds:  timeoutSec,
		SourceInline:    sourceInline,
		SourceRepoUrl:   sourceRepoURL,
		SourceCommitSha: sourceCommitSHA,
		Entrypoint:      run.Spec.Entrypoint,
	})
	cancel()
	if err != nil {
		c.updateRunStatus(context.Background(), run, v1alpha1.RunFailed, fmt.Sprintf("runtime CreateTask: %v", err))
		runsCompleted.WithLabelValues(run.Spec.Runtime, string(v1alpha1.RunFailed)).Inc()
		return
	}

	// Poll for completion
	for {
		select {
		case <-ctx.Done():
			c.updateRunStatus(context.Background(), run, v1alpha1.RunFailed, "runtimed shutting down")
			runsCompleted.WithLabelValues(run.Spec.Runtime, string(v1alpha1.RunFailed)).Inc()
			return
		default:
		}

		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := c.runtimeCli.Status(rctx, &pb.StatusRequest{Id: string(run.UID)})
		cancel()
		if err != nil {
			c.updateRunStatus(context.Background(), run, v1alpha1.RunFailed, fmt.Sprintf("runtime GetTask: %v", err))
			runsCompleted.WithLabelValues(run.Spec.Runtime, string(v1alpha1.RunFailed)).Inc()
			return
		}

		switch resp.State {
		case pb.ExecutionState_EXECUTION_STATE_SUCCEEDED:
			c.updateRunStatus(context.Background(), run, v1alpha1.RunSucceeded, resp.Stdout)
			runsCompleted.WithLabelValues(run.Spec.Runtime, string(v1alpha1.RunSucceeded)).Inc()
			klog.Infof("Run %s succeeded", run.Name)
			return
		case pb.ExecutionState_EXECUTION_STATE_FAILED:
			msg := resp.ErrorMessage
			if msg == "" {
				msg = resp.Stderr
			}
			c.updateRunStatus(context.Background(), run, v1alpha1.RunFailed, msg)
			runsCompleted.WithLabelValues(run.Spec.Runtime, string(v1alpha1.RunFailed)).Inc()
			klog.Infof("Run %s failed: %s", run.Name, msg)
			return
		}

		time.Sleep(200 * time.Millisecond)
	}
}

func (c *Controller) updateRunStatus(ctx context.Context, run *v1alpha1.Run, phase v1alpha1.RunPhase, msg string) {
	retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.Run{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(run), latest); err != nil {
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
