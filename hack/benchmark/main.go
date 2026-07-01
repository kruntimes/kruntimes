package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

const (
	benchmarkLabel   = "kruntimes.io/benchmark"
	benchmarkIDLabel = "kruntimes.io/benchmark-id"
)

type options struct {
	Namespace             string
	ControlPlaneNamespace string
	RuntimeName           string
	BashImage             string
	RuntimedImage         string
	Runs                  int
	Concurrency           int
	Replicas              int32
	Capacity              int32
	Sleep                 time.Duration
	Timeout               time.Duration
	PollInterval          time.Duration
	Cleanup               bool
	CapacityProbe         bool
}

type report struct {
	BenchmarkID   string             `json:"benchmarkID"`
	StartedAt     time.Time          `json:"startedAt"`
	CompletedAt   time.Time          `json:"completedAt"`
	Options       reportOptions      `json:"options"`
	Latency       latencyReport      `json:"latency"`
	Throughput    throughputReport   `json:"throughput"`
	Capacity      capacityReport     `json:"capacity"`
	ControlPlane  controlPlaneReport `json:"controlPlane"`
	TerminalPhase map[string]int     `json:"terminalPhase"`
}

type reportOptions struct {
	Namespace             string  `json:"namespace"`
	ControlPlaneNamespace string  `json:"controlPlaneNamespace"`
	RuntimeName           string  `json:"runtimeName"`
	Runs                  int     `json:"runs"`
	Concurrency           int     `json:"concurrency"`
	Replicas              int32   `json:"replicas"`
	CapacityPerPod        int32   `json:"capacityPerPod"`
	SleepSeconds          float64 `json:"sleepSeconds"`
	TimeoutSeconds        float64 `json:"timeoutSeconds"`
	PollIntervalSeconds   float64 `json:"pollIntervalSeconds"`
	CapacityProbe         bool    `json:"capacityProbe"`
}

type latencyReport struct {
	Schedule  latencyStats `json:"schedule"`
	Dispatch  latencyStats `json:"dispatch"`
	Execution latencyStats `json:"execution"`
	Complete  latencyStats `json:"complete"`
}

type latencyStats struct {
	Count int     `json:"count"`
	MinMS float64 `json:"minMs"`
	P50MS float64 `json:"p50Ms"`
	P95MS float64 `json:"p95Ms"`
	MaxMS float64 `json:"maxMs"`
}

type throughputReport struct {
	SuccessfulRuns int     `json:"successfulRuns"`
	FailedRuns     int     `json:"failedRuns"`
	WallSeconds    float64 `json:"wallSeconds"`
	RunsPerSecond  float64 `json:"runsPerSecond"`
}

type capacityReport struct {
	ReadyRuntimePods          int              `json:"readyRuntimePods"`
	ConfiguredTotalRunSlots   int32            `json:"configuredTotalRunSlots"`
	MaxObservedRunningRuns    int              `json:"maxObservedRunningRuns"`
	MaxObservedRunningByPod   map[string]int   `json:"maxObservedRunningByPod"`
	ObservedPendingAtCapacity bool             `json:"observedPendingAtCapacity"`
	AssignedRunsByPod         map[string]int   `json:"assignedRunsByPod"`
	RuntimePodNames           []string         `json:"runtimePodNames"`
	RuntimePodRestarts        map[string]int32 `json:"runtimePodRestarts"`
}

type controlPlaneReport struct {
	APICreate latencyStats       `json:"apiCreate"`
	APIList   latencyStats       `json:"apiList"`
	APIGet    latencyStats       `json:"apiGet"`
	Pods      []componentPodInfo `json:"pods"`
}

type componentPodInfo struct {
	Name      string `json:"name"`
	Ready     bool   `json:"ready"`
	Restarts  int32  `json:"restarts"`
	Component string `json:"component,omitempty"`
}

type runObservation struct {
	CreatedAt   time.Time
	ScheduledAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	Phase       v1alpha1.RunPhase
	AssignedPod string
}

func main() {
	opts := parseOptions()
	if err := run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "benchmark failed: %v\n", err)
		os.Exit(1)
	}
}

func parseOptions() options {
	opts := options{}
	replicas := envIntOrDefault("KRUNTIMES_BENCHMARK_REPLICAS", 2)
	capacity := envIntOrDefault("KRUNTIMES_BENCHMARK_CAPACITY", 64)

	flag.StringVar(&opts.Namespace, "namespace", envOrDefault("NAMESPACE", "default"), "Namespace for benchmark Runtime and Runs.")
	flag.StringVar(&opts.ControlPlaneNamespace, "control-plane-namespace", envOrDefault("KRUNTIMES_CONTROL_PLANE_NAMESPACE", envOrDefault("NAMESPACE", "default")), "Namespace where scheduler/controller pods run.")
	flag.StringVar(&opts.RuntimeName, "runtime", envOrDefault("KRUNTIMES_BENCHMARK_RUNTIME", "benchmark-bash"), "Runtime name used by benchmark Runs.")
	flag.StringVar(&opts.BashImage, "bash-image", envOrDefault("KRUNTIMES_BASH_RUNTIME_IMAGE", "kruntimes-bash-runtime:latest"), "Bash Runtime image.")
	flag.StringVar(&opts.RuntimedImage, "runtimed-image", envOrDefault("KRUNTIMES_RUNTIMED_IMAGE", "kruntimes-runtimed:latest"), "Runtimed image injected into the benchmark Runtime.")
	flag.IntVar(&opts.Runs, "runs", envIntOrDefault("KRUNTIMES_BENCHMARK_RUNS", 50), "Number of Runs to create.")
	flag.IntVar(&opts.Concurrency, "concurrency", envIntOrDefault("KRUNTIMES_BENCHMARK_CONCURRENCY", 25), "Maximum concurrent Run create requests.")
	flag.IntVar(&replicas, "replicas", replicas, "Benchmark Runtime replica count.")
	flag.IntVar(&capacity, "capacity", capacity, "Per-pod runs capacity for the benchmark Runtime.")
	flag.DurationVar(&opts.Sleep, "sleep", envDurationOrDefault("KRUNTIMES_BENCHMARK_SLEEP", 0), "Sleep duration executed by each Run.")
	flag.DurationVar(&opts.Timeout, "timeout", envDurationOrDefault("KRUNTIMES_BENCHMARK_TIMEOUT", 5*time.Minute), "Overall benchmark timeout.")
	flag.DurationVar(&opts.PollInterval, "poll-interval", envDurationOrDefault("KRUNTIMES_BENCHMARK_POLL_INTERVAL", 50*time.Millisecond), "Interval for polling Run status.")
	flag.BoolVar(&opts.Cleanup, "cleanup", envBoolOrDefault("KRUNTIMES_BENCHMARK_CLEANUP", true), "Delete benchmark Runs and Runtime after completion.")
	flag.BoolVar(&opts.CapacityProbe, "capacity-probe", envBoolOrDefault("KRUNTIMES_BENCHMARK_CAPACITY_PROBE", false), "Run a pre-benchmark capacity saturation probe.")
	flag.Parse()

	opts.Replicas = int32(replicas)
	opts.Capacity = int32(capacity)
	validateOptions(opts)
	return opts
}

func validateOptions(opts options) {
	if opts.Runs <= 0 {
		fail("runs must be > 0")
	}
	if opts.Concurrency <= 0 {
		fail("concurrency must be > 0")
	}
	if opts.Replicas <= 0 {
		fail("replicas must be > 0")
	}
	if opts.Capacity <= 0 {
		fail("capacity must be > 0")
	}
	if opts.PollInterval <= 0 {
		fail("poll-interval must be > 0")
	}
}

func run(parent context.Context, opts options) error {
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	restConfig := config.GetConfigOrDie()
	restConfig.QPS = 200
	restConfig.Burst = 400

	k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create controller-runtime client: %w", err)
	}
	coreClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	benchID := fmt.Sprintf("bench-%d", time.Now().Unix())
	startedAt := time.Now()

	if err := ensureNamespace(ctx, k8sClient, opts.Namespace); err != nil {
		return err
	}
	if err := ensureRuntime(ctx, k8sClient, opts); err != nil {
		return err
	}
	runtimePods, err := waitForRuntimeReady(ctx, k8sClient, opts)
	if err != nil {
		return err
	}
	if opts.Cleanup {
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			cleanup(cleanupCtx, k8sClient, opts, benchID)
		}()
	}

	capProbe := capacityReport{}
	if opts.CapacityProbe {
		var err error
		capProbe, err = observeCapacitySaturation(ctx, k8sClient, opts, benchID, runtimePods)
		if err != nil {
			return err
		}
	}
	observations, createLatencies, listLatencies, getLatencies, capReport, err := executeRuns(ctx, k8sClient, opts, benchID, runtimePods)
	if err != nil {
		return err
	}
	capReport.ObservedPendingAtCapacity = capProbe.ObservedPendingAtCapacity
	if capProbe.MaxObservedRunningRuns > capReport.MaxObservedRunningRuns {
		capReport.MaxObservedRunningRuns = capProbe.MaxObservedRunningRuns
	}
	for pod, count := range capProbe.MaxObservedRunningByPod {
		if count > capReport.MaxObservedRunningByPod[pod] {
			capReport.MaxObservedRunningByPod[pod] = count
		}
	}
	completedAt := time.Now()

	controlPlanePods, err := listControlPlanePods(ctx, coreClient, opts.ControlPlaneNamespace)
	if err != nil {
		return err
	}
	capReport.RuntimePodRestarts = runtimePodRestarts(runtimePods)

	out := buildReport(opts, benchID, startedAt, completedAt, observations, createLatencies, listLatencies, getLatencies, capReport, controlPlanePods)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

func ensureNamespace(ctx context.Context, k8sClient client.Client, namespace string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", namespace, err)
	}
	return nil
}

func ensureRuntime(ctx context.Context, k8sClient client.Client, opts options) error {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.RuntimeName,
			Namespace: opts.Namespace,
			Labels:    map[string]string{benchmarkLabel: "true"},
		},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate(opts.BashImage),
			Port:     9091,
			Replicas: opts.Replicas,
			Capacity: &v1alpha1.RuntimeCapacity{
				Resources: corev1.ResourceList{
					corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(int64(opts.Capacity), resource.DecimalSI),
				},
			},
			DaemonImage: opts.RuntimedImage,
		},
	}
	if err := k8sClient.Create(ctx, rt); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Runtime %s/%s: %w", opts.Namespace, opts.RuntimeName, err)
		}
		existing := &v1alpha1.Runtime{}
		if getErr := k8sClient.Get(ctx, client.ObjectKeyFromObject(rt), existing); getErr != nil {
			return fmt.Errorf("get Runtime %s/%s: %w", opts.Namespace, opts.RuntimeName, getErr)
		}
		existing.Labels = mergeStringMap(existing.Labels, rt.Labels)
		existing.Spec = rt.Spec
		if updateErr := k8sClient.Update(ctx, existing); updateErr != nil {
			return fmt.Errorf("update Runtime %s/%s: %w", opts.Namespace, opts.RuntimeName, updateErr)
		}
	}
	return nil
}

func runtimePodTemplate(image string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "runtime",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"--port=9091", "--work-dir=/workspace"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("25m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
			}},
		},
	}
}

func waitForRuntimeReady(ctx context.Context, k8sClient client.Client, opts options) ([]corev1.Pod, error) {
	var pods []corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, opts.Timeout, true, func(ctx context.Context) (bool, error) {
		list := &corev1.PodList{}
		if err := k8sClient.List(ctx, list, client.InNamespace(opts.Namespace), client.MatchingLabels{"runtime": opts.RuntimeName}); err != nil {
			return false, fmt.Errorf("list Runtime Pods: %w", err)
		}
		ready := make([]corev1.Pod, 0, len(list.Items))
		for i := range list.Items {
			pod := list.Items[i]
			if runtimePodReady(&pod, opts.Capacity) {
				ready = append(ready, pod)
			}
		}
		if len(ready) >= int(opts.Replicas) {
			pods = ready
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("wait for %d ready Runtime Pods: %w", opts.Replicas, err)
	}
	return pods, nil
}

func runtimePodReady(pod *corev1.Pod, capacity int32) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	if runtimepod.RunsCapacity(pod, 0) != capacity {
		return false
	}
	podReady := false
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			podReady = cond.Status == corev1.ConditionTrue
			break
		}
	}
	return podReady && runtimepod.FreshRuntimedReady(pod, time.Now(), 30*time.Second)
}

func executeRuns(ctx context.Context, k8sClient client.Client, opts options, benchID string, runtimePods []corev1.Pod) (map[string]*runObservation, []time.Duration, []time.Duration, []time.Duration, capacityReport, error) {
	observations := make(map[string]*runObservation, opts.Runs)
	createLatencies := make([]time.Duration, 0, opts.Runs)
	var mu sync.Mutex
	sem := make(chan struct{}, opts.Concurrency)
	errCh := make(chan error, opts.Runs)

	for i := 0; i < opts.Runs; i++ {
		i := i
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			run := benchmarkRun(opts, benchID, i, opts.Sleep)
			start := time.Now()
			if err := k8sClient.Create(ctx, run); err != nil {
				errCh <- fmt.Errorf("create Run %d: %w", i, err)
				return
			}
			mu.Lock()
			createLatencies = append(createLatencies, time.Since(start))
			observations[run.Name] = &runObservation{CreatedAt: start}
			mu.Unlock()
			errCh <- nil
		}()
	}
	for i := 0; i < opts.Runs; i++ {
		if err := <-errCh; err != nil {
			return nil, nil, nil, nil, capacityReport{}, err
		}
	}

	listLatencies := []time.Duration{}
	getLatencies := []time.Duration{}
	capReport := capacityReport{
		ReadyRuntimePods:        len(runtimePods),
		ConfiguredTotalRunSlots: int32(len(runtimePods)) * opts.Capacity,
		MaxObservedRunningByPod: map[string]int{},
		AssignedRunsByPod:       map[string]int{},
		RuntimePodNames:         runtimePodNames(runtimePods),
	}
	assignedSeen := map[string]struct{}{}
	if err := waitForAllTerminal(ctx, k8sClient, opts, benchID, observations, &listLatencies, &capReport, assignedSeen); err != nil {
		return nil, nil, nil, nil, capacityReport{}, err
	}

	for name := range observations {
		run := &v1alpha1.Run{}
		start := time.Now()
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: opts.Namespace, Name: name}, run); err != nil {
			return nil, nil, nil, nil, capacityReport{}, fmt.Errorf("get benchmark Run %s: %w", name, err)
		}
		getLatencies = append(getLatencies, time.Since(start))
	}
	return observations, createLatencies, listLatencies, getLatencies, capReport, nil
}

func observeCapacitySaturation(ctx context.Context, k8sClient client.Client, opts options, benchID string, runtimePods []corev1.Pod) (capacityReport, error) {
	totalSlots := int32(len(runtimePods)) * opts.Capacity
	out := capacityReport{
		ReadyRuntimePods:        len(runtimePods),
		ConfiguredTotalRunSlots: totalSlots,
		MaxObservedRunningByPod: map[string]int{},
		AssignedRunsByPod:       map[string]int{},
		RuntimePodNames:         runtimePodNames(runtimePods),
	}
	if totalSlots <= 0 {
		return out, nil
	}

	names := make([]string, 0, int(totalSlots)+1)
	for i := 0; i < int(totalSlots)+1; i++ {
		run := benchmarkRun(opts, benchID+"-capacity", i, 3*time.Second)
		if err := k8sClient.Create(ctx, run); err != nil {
			return out, fmt.Errorf("create capacity probe Run %d: %w", i, err)
		}
		names = append(names, run.Name)
	}
	defer deleteRuns(context.Background(), k8sClient, opts.Namespace, names)

	err := wait.PollUntilContextTimeout(ctx, opts.PollInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		list := &v1alpha1.RunList{}
		if err := k8sClient.List(ctx, list, client.InNamespace(opts.Namespace), client.MatchingLabels{benchmarkIDLabel: benchID + "-capacity"}); err != nil {
			return false, fmt.Errorf("list capacity probe Runs: %w", err)
		}
		runningByPod := map[string]int{}
		pending := 0
		for i := range list.Items {
			run := &list.Items[i]
			switch run.Status.Phase {
			case "", v1alpha1.RunPending:
				pending++
			case v1alpha1.RunRunning:
				runningByPod[run.Status.AssignedPod]++
			}
		}
		runningTotal := 0
		for pod, count := range runningByPod {
			runningTotal += count
			if count > out.MaxObservedRunningByPod[pod] {
				out.MaxObservedRunningByPod[pod] = count
			}
		}
		if runningTotal > out.MaxObservedRunningRuns {
			out.MaxObservedRunningRuns = runningTotal
		}
		if pending > 0 && runningTotal >= int(totalSlots) {
			out.ObservedPendingAtCapacity = true
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return out, nil
	}
	if err := waitForProbeTerminal(ctx, k8sClient, opts, benchID+"-capacity", len(names)); err != nil {
		return out, err
	}
	return out, nil
}

func waitForProbeTerminal(ctx context.Context, k8sClient client.Client, opts options, benchID string, want int) error {
	err := wait.PollUntilContextTimeout(ctx, opts.PollInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		list := &v1alpha1.RunList{}
		if err := k8sClient.List(ctx, list, client.InNamespace(opts.Namespace), client.MatchingLabels{benchmarkIDLabel: benchID}); err != nil {
			return false, fmt.Errorf("list capacity probe Runs: %w", err)
		}
		terminal := 0
		for i := range list.Items {
			if isTerminal(list.Items[i].Status.Phase) {
				terminal++
			}
		}
		return terminal >= want, nil
	})
	if err != nil {
		return fmt.Errorf("wait for capacity probe Runs to finish: %w", err)
	}
	return nil
}

func waitForAllTerminal(ctx context.Context, k8sClient client.Client, opts options, benchID string, observations map[string]*runObservation, listLatencies *[]time.Duration, capReport *capacityReport, assignedSeen map[string]struct{}) error {
	err := wait.PollUntilContextTimeout(ctx, opts.PollInterval, opts.Timeout, true, func(ctx context.Context) (bool, error) {
		list := &v1alpha1.RunList{}
		start := time.Now()
		if err := k8sClient.List(ctx, list, client.InNamespace(opts.Namespace), client.MatchingLabels{benchmarkIDLabel: benchID}); err != nil {
			return false, fmt.Errorf("list benchmark Runs: %w", err)
		}
		*listLatencies = append(*listLatencies, time.Since(start))

		now := time.Now()
		runningByPod := map[string]int{}
		terminal := 0
		for i := range list.Items {
			run := &list.Items[i]
			obs := observations[run.Name]
			if obs == nil {
				continue
			}
			obs.Phase = run.Status.Phase
			obs.AssignedPod = run.Status.AssignedPod
			if obs.ScheduledAt.IsZero() && run.Status.AssignedPod != "" {
				obs.ScheduledAt = now
			}
			if obs.StartedAt.IsZero() && run.Status.StartTime != nil {
				obs.StartedAt = now
			}
			if obs.FinishedAt.IsZero() && isTerminal(run.Status.Phase) {
				obs.FinishedAt = now
			}
			if run.Status.AssignedPod != "" {
				key := run.Name + "/" + run.Status.AssignedPod
				if _, ok := assignedSeen[key]; !ok {
					capReport.AssignedRunsByPod[run.Status.AssignedPod]++
					assignedSeen[key] = struct{}{}
				}
			}
			if run.Status.Phase == v1alpha1.RunRunning {
				runningByPod[run.Status.AssignedPod]++
			}
			if isTerminal(run.Status.Phase) {
				terminal++
			}
		}
		runningTotal := 0
		for pod, count := range runningByPod {
			runningTotal += count
			if count > capReport.MaxObservedRunningByPod[pod] {
				capReport.MaxObservedRunningByPod[pod] = count
			}
		}
		if runningTotal > capReport.MaxObservedRunningRuns {
			capReport.MaxObservedRunningRuns = runningTotal
		}
		return terminal == len(observations), nil
	})
	if err != nil {
		return fmt.Errorf("wait for benchmark Runs to finish: %w", err)
	}
	return nil
}

func benchmarkRun(opts options, benchID string, index int, sleep time.Duration) *v1alpha1.Run {
	ttl := int32(300)
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bench-",
			Namespace:    opts.Namespace,
			Labels: map[string]string{
				benchmarkLabel:   "true",
				benchmarkIDLabel: benchID,
			},
		},
		Spec: v1alpha1.RunSpec{
			Runtime:                 opts.RuntimeName,
			Args:                    []string{fmt.Sprintf("sleep %.3f; echo benchmark-run-%d", sleep.Seconds(), index)},
			TTLSecondsAfterFinished: &ttl,
		},
	}
}

func buildReport(opts options, benchID string, startedAt, completedAt time.Time, observations map[string]*runObservation, createLatencies, listLatencies, getLatencies []time.Duration, capReport capacityReport, controlPlanePods []componentPodInfo) report {
	schedule := []time.Duration{}
	dispatch := []time.Duration{}
	execution := []time.Duration{}
	complete := []time.Duration{}
	phases := map[string]int{}
	successful := 0
	failed := 0

	for _, obs := range observations {
		phase := string(obs.Phase)
		if phase == "" {
			phase = string(v1alpha1.RunPending)
		}
		phases[phase]++
		if obs.Phase == v1alpha1.RunSucceeded {
			successful++
		} else {
			failed++
		}
		if !obs.ScheduledAt.IsZero() {
			schedule = append(schedule, obs.ScheduledAt.Sub(obs.CreatedAt))
		}
		if !obs.StartedAt.IsZero() {
			dispatch = append(dispatch, obs.StartedAt.Sub(obs.CreatedAt))
		}
		if !obs.StartedAt.IsZero() && !obs.FinishedAt.IsZero() && !obs.FinishedAt.Before(obs.StartedAt) {
			execution = append(execution, obs.FinishedAt.Sub(obs.StartedAt))
		}
		if !obs.FinishedAt.IsZero() {
			complete = append(complete, obs.FinishedAt.Sub(obs.CreatedAt))
		}
	}

	wall := completedAt.Sub(startedAt).Seconds()
	rps := 0.0
	if wall > 0 {
		rps = float64(successful) / wall
	}
	return report{
		BenchmarkID: benchID,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		Options: reportOptions{
			Namespace:             opts.Namespace,
			ControlPlaneNamespace: opts.ControlPlaneNamespace,
			RuntimeName:           opts.RuntimeName,
			Runs:                  opts.Runs,
			Concurrency:           opts.Concurrency,
			Replicas:              opts.Replicas,
			CapacityPerPod:        opts.Capacity,
			SleepSeconds:          opts.Sleep.Seconds(),
			TimeoutSeconds:        opts.Timeout.Seconds(),
			PollIntervalSeconds:   opts.PollInterval.Seconds(),
			CapacityProbe:         opts.CapacityProbe,
		},
		Latency: latencyReport{
			Schedule:  summarize(schedule),
			Dispatch:  summarize(dispatch),
			Execution: summarize(execution),
			Complete:  summarize(complete),
		},
		Throughput: throughputReport{
			SuccessfulRuns: successful,
			FailedRuns:     failed,
			WallSeconds:    wall,
			RunsPerSecond:  rps,
		},
		Capacity: capReport,
		ControlPlane: controlPlaneReport{
			APICreate: summarize(createLatencies),
			APIList:   summarize(listLatencies),
			APIGet:    summarize(getLatencies),
			Pods:      controlPlanePods,
		},
		TerminalPhase: phases,
	}
}

func summarize(values []time.Duration) latencyStats {
	if len(values) == 0 {
		return latencyStats{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return latencyStats{
		Count: len(values),
		MinMS: millis(values[0]),
		P50MS: millis(percentile(values, 0.50)),
		P95MS: millis(percentile(values, 0.95)),
		MaxMS: millis(values[len(values)-1]),
	}
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	if len(values) == 1 {
		return values[0]
	}
	pos := p * float64(len(values)-1)
	idx := int(pos)
	if idx >= len(values)-1 {
		return values[len(values)-1]
	}
	frac := pos - float64(idx)
	return values[idx] + time.Duration(frac*float64(values[idx+1]-values[idx]))
}

func millis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func listControlPlanePods(ctx context.Context, coreClient *kubernetes.Clientset, namespace string) ([]componentPodInfo, error) {
	pods, err := coreClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=kruntimes",
	})
	if err != nil {
		return nil, fmt.Errorf("list control-plane pods: %w", err)
	}
	out := make([]componentPodInfo, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		component := pod.Labels["app.kubernetes.io/component"]
		if component != "scheduler" && component != "controller" {
			continue
		}
		out = append(out, componentPodInfo{
			Name:      pod.Name,
			Ready:     podReady(pod),
			Restarts:  podRestarts(pod),
			Component: component,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func runtimePodRestarts(pods []corev1.Pod) map[string]int32 {
	out := map[string]int32{}
	for i := range pods {
		out[pods[i].Name] = podRestarts(&pods[i])
	}
	return out
}

func runtimePodNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for i := range pods {
		names = append(names, pods[i].Name)
	}
	sort.Strings(names)
	return names
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podRestarts(pod *corev1.Pod) int32 {
	var restarts int32
	for _, status := range pod.Status.ContainerStatuses {
		restarts += status.RestartCount
	}
	return restarts
}

func cleanup(ctx context.Context, k8sClient client.Client, opts options, benchID string) {
	runs := &v1alpha1.RunList{}
	if err := k8sClient.List(ctx, runs, client.InNamespace(opts.Namespace), client.MatchingLabels{benchmarkIDLabel: benchID}); err == nil {
		for i := range runs.Items {
			_ = k8sClient.Delete(ctx, &runs.Items[i])
		}
	}
	rt := &v1alpha1.Runtime{ObjectMeta: metav1.ObjectMeta{Name: opts.RuntimeName, Namespace: opts.Namespace}}
	_ = k8sClient.Delete(ctx, rt)
}

func deleteRuns(ctx context.Context, k8sClient client.Client, namespace string, names []string) {
	for _, name := range names {
		run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		_ = k8sClient.Delete(ctx, run)
	}
}

func isTerminal(phase v1alpha1.RunPhase) bool {
	switch phase {
	case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
		return true
	default:
		return false
	}
}

func mergeStringMap(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		fail("%s must be an integer", name)
	}
	return parsed
}

func envDurationOrDefault(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		fail("%s must be a Go duration: %v", name, err)
	}
	return parsed
}

func envBoolOrDefault(name string, fallback bool) bool {
	value := strings.ToLower(os.Getenv(name))
	switch value {
	case "":
		return fallback
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		fail("%s must be a boolean", name)
		return fallback
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
