package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	workspaceadmission "github.com/kruntimes/kruntimes/internal/admission"
	runtimecontroller "github.com/kruntimes/kruntimes/internal/controller"
	"github.com/kruntimes/kruntimes/internal/runtimed"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
	"github.com/kruntimes/kruntimes/internal/scheduler"
	"github.com/kruntimes/kruntimes/runtimes/bash"
)

var (
	testEnv         *envtest.Environment
	k8sClient       client.Client
	testMgr         ctrl.Manager
	mgrCtx          context.Context
	mgrCancel       context.CancelFunc
	functionRuntime = newIntegrationFunctionRuntime()
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "kruntimes", "crds")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("failed to start testenv: " + err.Error())
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic("failed to create client: " + err.Error())
	}

	skipNameValidation := true
	testMgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		WebhookServer: webhookserver.NewServer(webhookserver.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
		Controller: config.Controller{
			SkipNameValidation: &skipNameValidation,
		},
	})
	if err != nil {
		panic("failed to create manager: " + err.Error())
	}
	workspaceadmission.RegisterRunAdmissionValidator(
		testMgr.GetWebhookServer(),
		testMgr.GetAPIReader(),
		allowSubjectAccessReviewer{},
		workspaceadmission.ServiceAccountIdentity{},
		scheme,
	)
	functionRuntimeServer := grpc.NewServer()
	pb.RegisterRuntimeServer(functionRuntimeServer, integrationUnavailableRuntime{})
	pb.RegisterFunctionRuntimeServer(functionRuntimeServer, functionRuntime)
	functionRuntimeListener, err := net.Listen("tcp", "127.0.0.1:19091")
	if err != nil {
		panic("failed to listen for Function Runtime: " + err.Error())
	}
	go func() { _ = functionRuntimeServer.Serve(functionRuntimeListener) }()

	if err := (&scheduler.RunReconciler{
		Client: testMgr.GetClient(),
		Log:    ctrl.Log.WithName("scheduler"),
	}).SetupWithManager(testMgr); err != nil {
		panic("failed to setup scheduler: " + err.Error())
	}
	integrationWorkspacePath, err := os.MkdirTemp("", "kruntimes-integration-workspace-")
	if err != nil {
		panic("failed to create integration workspace: " + err.Error())
	}
	defer os.RemoveAll(integrationWorkspacePath)
	if err := (&runtimed.Controller{
		Client:          testMgr.GetClient(),
		Log:             ctrl.Log.WithName("runtimed"),
		PodName:         "test-runtimed-pod",
		RuntimeEndpoint: "127.0.0.1:19091",
		WorkspacePath:   integrationWorkspacePath,
		Workers:         1,
	}).SetupWithManager(testMgr); err != nil {
		panic("failed to setup runtimed: " + err.Error())
	}

	mgrCtx, mgrCancel = context.WithCancel(context.Background())
	defer mgrCancel()

	go func() {
		if err := testMgr.Start(mgrCtx); err != nil {
			panic("manager error: " + err.Error())
		}
	}()

	code := m.Run()

	mgrCancel()
	functionRuntimeServer.Stop()
	if err := testEnv.Stop(); err != nil {
		panic("failed to stop testenv: " + err.Error())
	}

	os.Exit(code)
}

type integrationFunctionRuntime struct {
	pb.UnimplementedFunctionRuntimeServer

	mu            sync.Mutex
	registrations map[string]*pb.FunctionRegistration
	unregisters   map[string]int
}

// integrationUnavailableRuntime preserves the existing integration contract
// for one-shot Runs: runtimed can reach a Runtime Server, but execution itself
// is unavailable and must become a terminal failure. FunctionRuntime is
// registered on the same test server for the Function lifecycle test below.
type integrationUnavailableRuntime struct {
	pb.UnimplementedRuntimeServer
}

func (integrationUnavailableRuntime) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Healthy: true}, nil
}

func (integrationUnavailableRuntime) Execute(context.Context, *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	return nil, status.Error(codes.Unavailable, "one-shot execution is unavailable in integration tests")
}

func (integrationUnavailableRuntime) Status(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	return nil, status.Error(codes.NotFound, "execution not found")
}

func (integrationUnavailableRuntime) List(context.Context, *pb.ListRequest) (*pb.ListResponse, error) {
	return &pb.ListResponse{}, nil
}

func newIntegrationFunctionRuntime() *integrationFunctionRuntime {
	return &integrationFunctionRuntime{
		registrations: make(map[string]*pb.FunctionRegistration),
		unregisters:   make(map[string]int),
	}
}

func (s *integrationFunctionRuntime) RegisterFunction(_ context.Context, request *pb.RegisterFunctionRequest) (*pb.RegisterFunctionResponse, error) {
	if request.GetRunUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "run UID is required")
	}
	registration := &pb.FunctionRegistration{
		RunUid:         request.GetRunUid(),
		RegistrationId: request.GetRunUid() + "-registration",
	}
	s.mu.Lock()
	s.registrations[registration.GetRegistrationId()] = registration
	s.mu.Unlock()
	return &pb.RegisterFunctionResponse{Registration: registration, State: pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY}, nil
}

func (s *integrationFunctionRuntime) FunctionStatus(_ context.Context, request *pb.FunctionStatusRequest) (*pb.FunctionStatusResponse, error) {
	registration := request.GetRegistration()
	if registration == nil || registration.GetRegistrationId() == "" {
		return nil, status.Error(codes.NotFound, "function registration not found")
	}
	s.mu.Lock()
	registered := s.registrations[registration.GetRegistrationId()]
	s.mu.Unlock()
	if registered == nil {
		return nil, status.Error(codes.NotFound, "function registration not found")
	}
	return &pb.FunctionStatusResponse{
		Registration:         registered,
		State:                pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY,
		LastActivityUnixNano: time.Now().UnixNano(),
	}, nil
}

func (s *integrationFunctionRuntime) UnregisterFunction(_ context.Context, request *pb.UnregisterFunctionRequest) (*pb.UnregisterFunctionResponse, error) {
	registration := request.GetRegistration()
	if registration == nil || registration.GetRegistrationId() == "" {
		return nil, status.Error(codes.NotFound, "function registration not found")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registrations[registration.GetRegistrationId()] == nil {
		return nil, status.Error(codes.NotFound, "function registration not found")
	}
	delete(s.registrations, registration.GetRegistrationId())
	s.unregisters[registration.GetRunUid()]++
	return &pb.UnregisterFunctionResponse{Registration: registration}, nil
}

func (s *integrationFunctionRuntime) unregisterCount(runUID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unregisters[runUID]
}

func TestRunWorkspaceAdmissionWebhook(t *testing.T) {
	configuration := runWorkspaceValidatingWebhookConfiguration()
	testEnv.WebhookInstallOptions.ValidatingWebhooks = []*admissionregistrationv1.ValidatingWebhookConfiguration{configuration}
	if err := testEnv.WebhookInstallOptions.ModifyWebhookDefinitions(); err != nil {
		t.Fatalf("configure Run validating webhook for envtest: %v", err)
	}
	if err := k8sClient.Create(context.Background(), configuration); err != nil {
		t.Fatalf("create Run validating webhook configuration: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), configuration) }()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-admission-"}}
	if err := k8sClient.Create(context.Background(), namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), namespace) }()

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: namespace.Name},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(context.Background(), workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}

	allowed := integrationRun(namespace.Name, "build")
	if err := k8sClient.Create(context.Background(), allowed); err != nil {
		t.Fatalf("create authorized Run: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		rejected := integrationRun(namespace.Name, "missing")
		err := k8sClient.Create(context.Background(), rejected)
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "does not exist") {
			return
		}
		if err == nil {
			if deleteErr := k8sClient.Delete(context.Background(), rejected); deleteErr != nil {
				t.Fatalf("delete Run created before webhook became active: %v", deleteErr)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing workspace Run was not rejected by validating webhook: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRuntimeReadyReplicasTracksDeploymentStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	reconciler := &runtimecontroller.RuntimeReconciler{
		Client:             k8sClient,
		Log:                ctrl.Log.WithName("runtime-controller"),
		Scheme:             testMgr.GetScheme(),
		DefaultDaemonImage: "runtimed:integration",
		ConsoleURL:         "https://console.integration",
		ConsoleCABundle:    []byte("test-ca"),
	}
	namespace := testNamespace(t, "runtime-readiness-")
	runtimeResource := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: namespace.Name},
		Spec: v1alpha1.RuntimeSpec{
			Replicas: 2,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runtime", Image: "runtime:integration"}},
			}},
		},
	}
	if err := k8sClient.Create(ctx, runtimeResource); err != nil {
		t.Fatalf("create Runtime: %v", err)
	}

	deploymentKey := client.ObjectKey{Namespace: namespace.Name, Name: "runtime-ready"}
	reconcileRuntimeReadiness(t, ctx, reconciler, client.ObjectKeyFromObject(runtimeResource))
	if err := k8sClient.Get(ctx, deploymentKey, &appsv1.Deployment{}); err != nil {
		t.Fatalf("get Runtime Deployment: %v", err)
	}

	updateDeploymentReadyReplicas(t, ctx, deploymentKey, 2)
	reconcileRuntimeReadiness(t, ctx, reconciler, client.ObjectKeyFromObject(runtimeResource))
	waitForRuntimeReadyReplicas(t, ctx, client.ObjectKeyFromObject(runtimeResource), 2)

	updateDeploymentReadyReplicas(t, ctx, deploymentKey, 0)
	reconcileRuntimeReadiness(t, ctx, reconciler, client.ObjectKeyFromObject(runtimeResource))
	waitForRuntimeReadyReplicas(t, ctx, client.ObjectKeyFromObject(runtimeResource), 0)
}

func reconcileRuntimeReadiness(t *testing.T, ctx context.Context, reconciler *runtimecontroller.RuntimeReconciler, key client.ObjectKey) {
	t.Helper()
	for range 8 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile Runtime: %v", err)
		}
	}
}

func updateDeploymentReadyReplicas(t *testing.T, ctx context.Context, key client.ObjectKey, readyReplicas int32) {
	t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, key, deployment); err != nil {
			return err
		}
		if deployment.Status.Replicas < readyReplicas {
			deployment.Status.Replicas = readyReplicas
		}
		deployment.Status.ReadyReplicas = readyReplicas
		return k8sClient.Status().Update(ctx, deployment)
	}); err != nil {
		t.Fatalf("update Deployment readyReplicas to %d: %v", readyReplicas, err)
	}
}

func waitForRuntimeReadyReplicas(t *testing.T, ctx context.Context, key client.ObjectKey, want int32) {
	t.Helper()
	for {
		runtimeResource := &v1alpha1.Runtime{}
		if err := k8sClient.Get(ctx, key, runtimeResource); err == nil && runtimeResource.Status.ReadyReplicas == want {
			return
		} else if ctx.Err() != nil {
			t.Fatalf("wait for Runtime readyReplicas=%d: %v", want, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSessionFilePaginationContract(t *testing.T) {
	workDir := t.TempDir()
	runtimeServer := grpc.NewServer()
	pb.RegisterSessionRuntimeServer(runtimeServer, bash.NewServer(workDir))
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen for Session Runtime: %v", err)
	}
	defer listener.Close()
	go func() { _ = runtimeServer.Serve(listener) }()
	defer runtimeServer.Stop()

	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial Session Runtime: %v", err)
	}
	defer connection.Close()
	client := pb.NewSessionRuntimeClient(connection)
	identity := &pb.SessionIdentity{RunUid: "pagination-run", AssignedPodUid: "runtime-pod"}
	if _, err := client.RegisterSession(t.Context(), &pb.RegisterSessionRequest{Identity: identity, WorkingDir: workDir}); err != nil {
		t.Fatalf("register Session: %v", err)
	}
	for _, name := range []string{"z", "a", "\u00e9", "b"} {
		stream, err := client.StreamSessionOperation(t.Context(), &pb.ExecuteSessionOperationRequest{
			Identity: identity,
			Operation: &pb.ExecuteSessionOperationRequest_WriteFile{WriteFile: &pb.SessionFileWrite{
				Path: "pages/" + name, Contents: []byte(name), CreateParents: true,
			}},
		})
		if err != nil {
			t.Fatalf("write Session file %q: %v", name, err)
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				t.Fatalf("receive Session file operation %q: %v", name, err)
			}
			if event.GetCompleted() != nil {
				break
			}
		}
	}

	first, err := client.ListSessionFiles(t.Context(), &pb.ListSessionFilesRequest{Identity: identity, Path: "pages", Limit: 2})
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if got, want := integrationSessionFilePaths(first.Entries), []string{"a", "b"}; !slices.Equal(got, want) || first.NextPageToken == "" {
		t.Fatalf("first page = %#v, want %#v and next token", first, want)
	}
	second, err := client.ListSessionFiles(t.Context(), &pb.ListSessionFilesRequest{Identity: identity, Path: "pages", Limit: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if got, want := integrationSessionFilePaths(second.Entries), []string{"z", "\u00e9"}; !slices.Equal(got, want) || second.NextPageToken != "" {
		t.Fatalf("second page = %#v, want %#v and no next token", second, want)
	}

	for _, request := range []*pb.ListSessionFilesRequest{
		{Identity: identity, Path: "pages", Limit: 1001},
		{Identity: identity, Path: "other", PageToken: first.NextPageToken},
		{Identity: identity, Path: "../outside"},
	} {
		if _, err := client.ListSessionFiles(t.Context(), request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("ListSessionFiles(%#v) error = %v, want InvalidArgument", request, err)
		}
	}
}

func integrationSessionFilePaths(entries []*pb.SessionFileInfo) []string {
	paths := make([]string, len(entries))
	for i := range entries {
		paths[i] = entries[i].Path
	}
	return paths
}

func integrationRun(namespace, workspaceName string) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-admission-", Namespace: namespace},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}}},
			Workspace: &v1alpha1.RunWorkspaceReference{
				Name: workspaceName,
			},
		},
	}
}

func runWorkspaceValidatingWebhookConfiguration() *admissionregistrationv1.ValidatingWebhookConfiguration {
	failurePolicy := admissionregistrationv1.Fail
	matchPolicy := admissionregistrationv1.Equivalent
	sideEffects := admissionregistrationv1.SideEffectClassNone
	timeoutSeconds := int32(5)
	path := workspaceadmission.RunValidationPath
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "run-workspace.integration.kruntimes.io"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name:                    "run-workspace.integration.kruntimes.io",
			FailurePolicy:           &failurePolicy,
			MatchPolicy:             &matchPolicy,
			SideEffects:             &sideEffects,
			TimeoutSeconds:          &timeoutSeconds,
			AdmissionReviewVersions: []string{"v1"},
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Namespace: "default",
					Name:      "integration-webhook",
					Path:      &path,
				},
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{v1alpha1.GroupVersion.Group},
					APIVersions: []string{v1alpha1.GroupVersion.Version},
					Resources:   []string{"runs"},
				},
			}},
		}},
	}
}

type allowSubjectAccessReviewer struct{}

func (allowSubjectAccessReviewer) Review(context.Context, authorizationv1.SubjectAccessReview) (authorizationv1.SubjectAccessReviewStatus, error) {
	return authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil
}

func TestSchedulerReconcile(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bash-pod-",
			Namespace:    ns.Name,
			Labels:       map[string]string{"runtime": "bash"},
			Annotations: map[string]string{
				runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "1",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "runtimed", Image: "busybox", Command: []string{"sleep", "999"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		{
			Type:               v1alpha1.RuntimePodRuntimedReadyCondition,
			Status:             corev1.ConditionTrue,
			LastProbeTime:      metav1.Now(),
			LastTransitionTime: metav1.Now(),
		},
	}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Wait for scheduler to assign the run.
	var updated v1alpha1.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if updated.Status.Phase == v1alpha1.RunScheduled && updated.Status.AssignedPod != "" {
			t.Logf("Task %s scheduled to pod %s", updated.Name, updated.Status.AssignedPod)
			return
		}
	}
	t.Errorf("expected Scheduled, got phase=%s assignedPod=%s", updated.Status.Phase, updated.Status.AssignedPod)
}

func TestSchedulerWakesPendingRunWhenWorkspaceBinds(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-scheduler-workspace-")
	pod := createReadyRuntimePod(t, ctx, ns.Name, "runtime-workspace", 1)
	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: ns.Name},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
		Status:     v1alpha1.PersistentWorkspaceStatus{Phase: v1alpha1.PersistentWorkspacePending},
	}
	if err := k8sClient.Create(ctx, workspace); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-run", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	var pending v1alpha1.Run
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), &pending); err != nil {
		t.Fatalf("get pending run: %v", err)
	}
	if pending.Status.AssignedPod != "" {
		t.Fatalf("pending workspace run assigned to %q, want no assignment", pending.Status.AssignedPod)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
			return err
		}
		workspace.Status.Phase = v1alpha1.PersistentWorkspaceBound
		workspace.Status.BoundPod = pod.Name
		workspace.Status.BoundPodUID = string(pod.UID)
		return k8sClient.Status().Update(ctx, workspace)
	}); err != nil {
		t.Fatalf("bind workspace: %v", err)
	}

	assigned := waitForRunAssignment(t, ctx, run)
	if assigned.Status.AssignedPod != pod.Name {
		t.Fatalf("assigned pod = %q, want workspace Pod %q", assigned.Status.AssignedPod, pod.Name)
	}
}

func TestSchedulerAggregatesAffinityAndCapacityScores(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-scheduler-score-")

	runtimeA := createReadyRuntimePod(t, ctx, ns.Name, "runtime-a", 2)
	runtimeB := createReadyRuntimePod(t, ctx, ns.Name, "runtime-b", 4)
	runtimeC := createReadyRuntimePod(t, ctx, ns.Name, "runtime-c", 4)
	// The active Runs provide both affinity targets and current resource usage.
	// Once weighted-score is placed, projected runs usage is A=2/2, B=3/4,
	// and C=2/4. LeastLoaded therefore assigns normalized scores A=0, B=50,
	// and C=100.
	createAssignedRun(t, ctx, ns.Name, "target-a", runtimeA.Name, map[string]string{"zone": "blue", "tier": "gold"})
	createAssignedRun(t, ctx, ns.Name, "target-b-1", runtimeB.Name, nil)
	createAssignedRun(t, ctx, ns.Name, "target-b-2", runtimeB.Name, nil)
	createAssignedRun(t, ctx, ns.Name, "target-c", runtimeC.Name, map[string]string{"zone": "blue"})

	term := func(labels map[string]string) v1alpha1.WeightedRunAffinityTerm {
		return v1alpha1.WeightedRunAffinityTerm{
			Weight: 1,
			RunAffinityTerm: v1alpha1.RunAffinityTerm{
				TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
				LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
			},
		}
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "weighted-score", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
			Affinity: &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
				// A matches both terms and has affinity score 100; C matches only
				// zone=blue and has score 50; B has score 0. With equal plugin
				// weights, totals are A=100, B=50, C=150, so C must win. This
				// verifies weighted aggregation rather than strict affinity precedence.
				PreferredDuringSchedulingIgnoredDuringExecution: []v1alpha1.WeightedRunAffinityTerm{
					term(map[string]string{"zone": "blue"}),
					term(map[string]string{"tier": "gold"}),
				},
			}},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create weighted-score run: %v", err)
	}

	updated := waitForRunAssignment(t, ctx, run)
	if updated.Status.AssignedPod != runtimeC.Name {
		t.Fatalf("assigned pod = %q, want %q; the equal-weight aggregate must allow lower load to outweigh one missing preferred term", updated.Status.AssignedPod, runtimeC.Name)
	}
}

func TestRuntimedClaimAndExecute(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Re-fetch and update, retrying on conflict with scheduler.
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		run.Status.Phase = v1alpha1.RunScheduled
		run.Status.AssignedPod = "test-runtimed-pod"
		if err := k8sClient.Status().Update(context.Background(), run); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if run.Status.Phase != v1alpha1.RunScheduled {
		t.Fatalf("failed to set phase after retries")
	}

	// Wait for runtimed to pick up and fail because one-shot execution is
	// deliberately unavailable from the shared integration Runtime Server.
	var final v1alpha1.Run
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &final); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if final.Status.Phase == v1alpha1.RunFailed {
			t.Logf("Task %s completed: phase=%s, msg=%s", final.Name, final.Status.Phase, final.Status.Message)
			return
		}
	}
	t.Errorf("expected Failed due to no runtime, got phase=%s msg=%s", final.Status.Phase, final.Status.Message)
}

func TestRuntimedFunctionRegistrationLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	namespace := testNamespace(t, "function-lifecycle-")

	newFunctionRun := func(name string) *v1alpha1.Run {
		t.Helper()
		run := &v1alpha1.Run{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name},
			Spec: v1alpha1.RunSpec{
				Runtime: "bash",
				Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
			},
		}
		if err := k8sClient.Create(ctx, run); err != nil {
			t.Fatalf("create Function Run %q: %v", name, err)
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
				return err
			}
			run.Status.Phase = v1alpha1.RunScheduled
			run.Status.AssignedPod = "test-runtimed-pod"
			run.Status.AssignedPodUID = "test-runtimed-pod-uid"
			return k8sClient.Status().Update(ctx, run)
		}); err != nil {
			t.Fatalf("assign Function Run %q: %v", name, err)
		}
		return waitForIntegrationRunPhase(t, ctx, run, v1alpha1.RunReady)
	}

	t.Run("cancellation unregisters the function", func(t *testing.T) {
		run := newFunctionRun("cancel")
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
				return err
			}
			run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate}
			return k8sClient.Update(ctx, run)
		}); err != nil {
			t.Fatalf("cancel Function Run: %v", err)
		}
		cancelled := waitForIntegrationRunPhase(t, ctx, run, v1alpha1.RunCancelled)
		if got := functionRuntime.unregisterCount(string(cancelled.UID)); got != 1 {
			t.Fatalf("unregister calls = %d, want 1", got)
		}
	})

	t.Run("deletion unregisters the function", func(t *testing.T) {
		run := newFunctionRun("delete")
		runUID := string(run.UID)
		if err := k8sClient.Delete(ctx, run); err != nil {
			t.Fatalf("delete Function Run: %v", err)
		}
		waitForIntegrationRunDeleted(t, ctx, run)
		if got := functionRuntime.unregisterCount(runUID); got != 1 {
			t.Fatalf("unregister calls = %d, want 1", got)
		}
	})
}

func waitForIntegrationRunPhase(t *testing.T, ctx context.Context, run *v1alpha1.Run, want v1alpha1.RunPhase) *v1alpha1.Run {
	t.Helper()
	current := &v1alpha1.Run{}
	for {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), current); err != nil {
			t.Fatalf("get Run %q: %v", run.Name, err)
		}
		if current.Status.Phase == want {
			return current
		}
		if current.Status.Phase == v1alpha1.RunFailed || current.Status.Phase == v1alpha1.RunTimeout || current.Status.Phase == v1alpha1.RunCancelled {
			t.Fatalf("Run %q phase = %s, want %s: %s", run.Name, current.Status.Phase, want, current.Status.Message)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Run %q phase %s: %v", run.Name, want, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func waitForIntegrationRunDeleted(t *testing.T, ctx context.Context, run *v1alpha1.Run) {
	t.Helper()
	for {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), &v1alpha1.Run{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get deleting Run %q: %v", run.Name, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Run %q deletion: %v", run.Name, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestSchedulerKeepsPendingWhenNoMatchingPod(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "nonexistent-runtime",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Wait for scheduler to observe the run without failing it.
	var updated v1alpha1.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if updated.Status.Phase == v1alpha1.RunFailed {
			t.Fatalf("expected Pending when no matching pod, got Failed: %s", updated.Status.Message)
		}
		if updated.Status.Phase == v1alpha1.RunPending && updated.Status.Message != "" {
			if updated.Status.AssignedPod != "" {
				t.Fatalf("expected no assigned pod, got %s", updated.Status.AssignedPod)
			}
			return
		}
	}
	t.Errorf("expected Pending when no matching pod, got %s: %s", updated.Status.Phase, updated.Status.Message)
}

func TestSchedulerSkipsNotReadyPod(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bash-pod-",
			Namespace:    ns.Name,
			Labels:       map[string]string{"runtime": "bash"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "runtimed", Image: "busybox", Command: []string{"sleep", "999"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.Now()},
	}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	var updated v1alpha1.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if updated.Status.Phase == v1alpha1.RunScheduled {
			t.Fatalf("expected NotReady pod to be skipped, got scheduled to %s", updated.Status.AssignedPod)
		}
		if updated.Status.Phase == v1alpha1.RunPending && updated.Status.Message != "" {
			if updated.Status.AssignedPod != "" {
				t.Fatalf("expected no assigned pod, got %s", updated.Status.AssignedPod)
			}
			return
		}
	}
	t.Errorf("expected Pending when matching pod is not ready, got %s: %s", updated.Status.Phase, updated.Status.Message)
}

func TestRunArtifactRefValidation(t *testing.T) {
	ctx := context.Background()
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-artifact-ref-"
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, ns) }()
	invalidInput := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-artifact-input", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash", Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
			ArtifactInputs: []v1alpha1.ArtifactInput{{
				Path: "../escape",
				Ref: v1alpha1.ArtifactRef{
					Name: "report", Driver: v1alpha1.ArtifactDriverFilesystem, Type: v1alpha1.ArtifactTypeFile,
					Location:  v1alpha1.ArtifactLocation{Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "runs/source/report"}},
					CreatedAt: metav1.Now(),
				},
			}},
		},
	}
	if err := k8sClient.Create(ctx, invalidInput); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid artifact input path error = %v, want Invalid", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "artifact-ref", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	artifactRefs := []v1alpha1.ArtifactRef{
		{
			Name:   "report",
			Driver: v1alpha1.ArtifactDriverFilesystem,
			Type:   v1alpha1.ArtifactTypeFile,
			Location: v1alpha1.ArtifactLocation{
				Filesystem: &v1alpha1.FilesystemArtifactLocation{
					Path:            "namespaces/default/runs/uid/report",
					VolumeClaimName: "artifacts",
				},
			},
			CreatedAt: metav1.Now(),
		},
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return err
		}
		run.Status.ArtifactRefs = artifactRefs
		run.Status.Phase = v1alpha1.RunPending
		return k8sClient.Status().Update(ctx, run)
	}); err != nil {
		t.Fatalf("update valid artifact ref: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatalf("get run before invalid update: %v", err)
	}
	run.Status.ArtifactRefs[0].Location.S3 = &v1alpha1.S3ArtifactLocation{
		Bucket: "artifacts",
		Key:    "report",
	}
	if err := k8sClient.Status().Update(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid mixed artifact locations error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidRunModeTaskEntrypoint(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "/escape"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid run entrypoint error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsIgnoredInlineRunModeTaskEntrypoint(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")
	inline := "echo inline"

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "ignored-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "/ignored"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("inline run with ignored entrypoint should be valid: %v", err)
	}
}

func TestCRDValidationRejectsRunWithoutMode(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-mode", Namespace: ns.Name},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("missing run mode error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsRunModeWithMultipleModes(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	for _, mode := range []v1alpha1.RunMode{
		{
			Task:    &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			Session: &v1alpha1.RunSessionMode{},
		},
		{
			Function: &v1alpha1.RunFunctionMode{Handler: "main.invoke"},
			Session:  &v1alpha1.RunSessionMode{},
		},
	} {
		run := &v1alpha1.Run{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "mixed-mode-", Namespace: ns.Name},
			Spec: v1alpha1.RunSpec{
				Runtime: "bash",
				Mode:    mode,
			},
		}
		if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
			t.Fatalf("mixed run mode error = %v, want Invalid", err)
		}
	}
}

func TestCRDValidationRejectsSessionRunWorkspace(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session-workspace", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Mode:      v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
			Workspace: &v1alpha1.RunWorkspaceReference{Name: "workspace"},
		},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("session Run workspace error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidRunModeTaskEntrypointTraversal(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-mode-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "../escape"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid mode task entrypoint error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsIgnoredInlineRunModeTaskEntrypointTraversal(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")
	inline := "echo inline"

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "ignored-mode-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "/ignored"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("inline run with ignored mode task entrypoint should be valid: %v", err)
	}
}

func TestCRDValidationFunctionInlinePath(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-function-inline-path-")
	inline := "def invoke(request): return request"

	tests := []struct {
		name      string
		mode      v1alpha1.RunMode
		source    *v1alpha1.CodeSource
		wantValid bool
	}{
		{
			name:      "allows function inline source path",
			mode:      v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.invoke"}},
			source:    &v1alpha1.CodeSource{Inline: &inline, InlinePath: "functions/app.py"},
			wantValid: true,
		},
		{
			name:   "rejects function inline source without path",
			mode:   v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.invoke"}},
			source: &v1alpha1.CodeSource{Inline: &inline},
		},
		{
			name:   "rejects task inline source path",
			mode:   v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
			source: &v1alpha1.CodeSource{Inline: &inline, InlinePath: "script.py"},
		},
		{
			name:   "rejects path without inline source",
			mode:   v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.invoke"}},
			source: &v1alpha1.CodeSource{InlinePath: "app.py"},
		},
		{
			name:   "rejects traversal path",
			mode:   v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.invoke"}},
			source: &v1alpha1.CodeSource{Inline: &inline, InlinePath: "../app.py"},
		},
		{
			name:   "rejects absolute path",
			mode:   v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.invoke"}},
			source: &v1alpha1.CodeSource{Inline: &inline, InlinePath: "/app.py"},
		},
		{
			name:   "rejects current directory path",
			mode:   v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.invoke"}},
			source: &v1alpha1.CodeSource{Inline: &inline, InlinePath: "./app.py"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &v1alpha1.Run{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "function-inline-path-", Namespace: ns.Name},
				Spec: v1alpha1.RunSpec{
					Runtime: "python",
					Mode:    tt.mode,
					Source:  tt.source,
				},
			}
			err := k8sClient.Create(ctx, run)
			if tt.wantValid {
				if err != nil {
					t.Fatalf("create valid Run: %v", err)
				}
				return
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("create invalid Run error = %v, want Invalid", err)
			}
		})
	}
}

func TestCRDValidationAllowsRunWorkspaceAndAffinity(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-workspace-affinity-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
			Workspace: &v1alpha1.RunWorkspaceReference{Name: "build-workspace"},
			Affinity: &v1alpha1.RunAffinity{
				RunAffinity: &v1alpha1.RunAffinityRules{
					RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"workflow": "build"}},
						TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
					}},
				},
				RunAntiAffinity: &v1alpha1.RunAffinityRules{
					PreferredDuringSchedulingIgnoredDuringExecution: []v1alpha1.WeightedRunAffinityTerm{{
						Weight: 100,
						RunAffinityTerm: v1alpha1.RunAffinityTerm{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"run-type": "exclusive"}},
							TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
						},
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run with workspace and affinity: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatalf("get run with workspace and affinity: %v", err)
	}
	if got := run.Spec.Workspace.Kind; got != v1alpha1.RunWorkspaceReferenceKindPersistentWorkspace {
		t.Fatalf("workspace kind = %q, want %q", got, v1alpha1.RunWorkspaceReferenceKindPersistentWorkspace)
	}
	if got := run.Spec.Workspace.APIGroup; got != v1alpha1.RunWorkspaceReferenceAPIGroup {
		t.Fatalf("workspace apiGroup = %q, want %q", got, v1alpha1.RunWorkspaceReferenceAPIGroup)
	}
}

func TestCRDValidationRejectsInvalidRunWorkspaceOrAffinity(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-invalid-run-workspace-affinity-")

	tests := []struct {
		name string
		spec v1alpha1.RunSpec
	}{
		{
			name: "unsupported-workspace-kind",
			spec: v1alpha1.RunSpec{
				Runtime: "bash", Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
				Workspace: &v1alpha1.RunWorkspaceReference{Name: "workspace", Kind: "OtherWorkspace"},
			},
		},
		{
			name: "unsupported-workspace-api-group",
			spec: v1alpha1.RunSpec{
				Runtime: "bash", Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
				Workspace: &v1alpha1.RunWorkspaceReference{Name: "workspace", APIGroup: "example.com/v1"},
			},
		},
		{
			name: "empty-affinity-selector",
			spec: runSpecWithRequiredAffinity(v1alpha1.RunAffinityTerm{
				LabelSelector: &metav1.LabelSelector{},
				TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
			}),
		},
		{
			name: "unsupported-affinity-topology",
			spec: runSpecWithRequiredAffinity(v1alpha1.RunAffinityTerm{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"workflow": "build"}},
				TopologyKey:   "topology.kubernetes.io/zone",
			}),
		},
		{
			name: "zero-preferred-affinity-weight",
			spec: v1alpha1.RunSpec{
				Runtime: "bash", Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
				Affinity: &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
					PreferredDuringSchedulingIgnoredDuringExecution: []v1alpha1.WeightedRunAffinityTerm{{
						Weight: 0,
						RunAffinityTerm: v1alpha1.RunAffinityTerm{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"workflow": "build"}},
							TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
						},
					}},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &v1alpha1.Run{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: ns.Name},
				Spec:       tt.spec,
			}
			if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
				t.Fatalf("create invalid run error = %v, want Invalid", err)
			}
		})
	}
}

func TestCRDValidationRejectsRunWorkspaceAndAffinityMutation(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-workspace-affinity-immutable-")
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
			Workspace: &v1alpha1.RunWorkspaceReference{Name: "build-workspace"},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	key := client.ObjectKeyFromObject(run)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, key, run); err != nil {
			return err
		}
		run.Spec.Workspace.Name = "other-workspace"
		return k8sClient.Update(ctx, run)
	})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("mutating run workspace error = %v, want Invalid", err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, key, run); err != nil {
			return err
		}
		run.Spec.Affinity = &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"workflow": "build"}},
				TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
			}},
		}}
		return k8sClient.Update(ctx, run)
	})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("mutating run affinity error = %v, want Invalid", err)
	}
}

func TestCRDValidationRunExecutionInputsAreImmutable(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-immutable-inputs-")
	inline := "echo original"
	timeout := metav1.Duration{Duration: time.Minute}
	ttl := int32(60)

	newRun := func(t *testing.T) *v1alpha1.Run {
		t.Helper()
		run := &v1alpha1.Run{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "build-", Namespace: ns.Name},
			Spec: v1alpha1.RunSpec{
				Runtime: "bash",
				Source:  &v1alpha1.CodeSource{Inline: &inline},
				Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{
					Entrypoint: "script",
					Args:       []string{"--verbose"},
				}},
				ArtifactInputs: []v1alpha1.ArtifactInput{{
					Path: "inputs/report.txt",
					Ref: v1alpha1.ArtifactRef{
						Name: "report.txt", Driver: v1alpha1.ArtifactDriverFilesystem, Type: v1alpha1.ArtifactTypeFile,
						Location:  v1alpha1.ArtifactLocation{Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "runs/source/report.txt"}},
						CreatedAt: metav1.Now(),
					},
				}},
				Env:     []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
				Timeout: &timeout,
				RetryPolicy: &v1alpha1.RetryPolicy{
					MaxAttempts: 2,
				},
				Resources: &v1alpha1.RunResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceName("example.com/gpu"): resource.MustParse("1"),
				}},
				TTLSecondsAfterFinished: &ttl,
			},
		}
		if err := k8sClient.Create(ctx, run); err != nil {
			t.Fatalf("create run: %v", err)
		}
		return run
	}
	updateRun := func(run *v1alpha1.Run, mutate func(*v1alpha1.Run)) error {
		key := client.ObjectKeyFromObject(run)
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &v1alpha1.Run{}
			if err := k8sClient.Get(ctx, key, current); err != nil {
				return err
			}
			mutate(current)
			return k8sClient.Update(ctx, current)
		})
	}

	tests := []struct {
		name   string
		mutate func(*v1alpha1.Run)
	}{
		{
			name: "runtime",
			mutate: func(run *v1alpha1.Run) {
				run.Spec.Runtime = "python"
			},
		},
		{
			name: "source",
			mutate: func(run *v1alpha1.Run) {
				updated := "echo updated"
				run.Spec.Source.Inline = &updated
			},
		},
		{
			name: "mode",
			mutate: func(run *v1alpha1.Run) {
				run.Spec.Mode = v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "main.handle"}}
			},
		},
		{
			name: "artifact-inputs",
			mutate: func(run *v1alpha1.Run) {
				run.Spec.ArtifactInputs[0].Path = "inputs/updated.txt"
			},
		},
		{
			name: "env",
			mutate: func(run *v1alpha1.Run) {
				run.Spec.Env[0].Value = "debug"
			},
		},
		{
			name: "timeout",
			mutate: func(run *v1alpha1.Run) {
				run.Spec.Timeout.Duration = 2 * time.Minute
			},
		},
		{
			name: "retry-policy",
			mutate: func(run *v1alpha1.Run) {
				run.Spec.RetryPolicy.MaxAttempts = 3
			},
		},
		{
			name: "resources",
			mutate: func(run *v1alpha1.Run) {
				run.Spec.Resources.Requests[corev1.ResourceName("example.com/gpu")] = resource.MustParse("2")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := newRun(t)
			if err := updateRun(run, tt.mutate); !apierrors.IsInvalid(err) {
				t.Fatalf("mutating run %s error = %v, want Invalid", tt.name, err)
			}
		})
	}

	t.Run("termination-request-is-one-way", func(t *testing.T) {
		run := newRun(t)
		if err := updateRun(run, func(run *v1alpha1.Run) {
			run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate}
		}); err != nil {
			t.Fatalf("request immediate run termination: %v", err)
		}
		if err := updateRun(run, func(run *v1alpha1.Run) {
			run.Spec.Termination = nil
		}); !apierrors.IsInvalid(err) {
			t.Fatalf("clearing run termination error = %v, want Invalid", err)
		}
	})

	t.Run("drain-termination-requires-session-mode", func(t *testing.T) {
		run := newRun(t)
		if err := updateRun(run, func(run *v1alpha1.Run) {
			run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain}
		}); !apierrors.IsInvalid(err) {
			t.Fatalf("request drain termination for task Run error = %v, want Invalid", err)
		}
	})

	t.Run("drain-termination-is-one-way", func(t *testing.T) {
		run := &v1alpha1.Run{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "session-", Namespace: ns.Name},
			Spec: v1alpha1.RunSpec{
				Runtime:     "bash",
				Mode:        v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
				Termination: &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain},
			},
		}
		if err := k8sClient.Create(ctx, run); err != nil {
			t.Fatalf("create draining Session Run: %v", err)
		}
		if err := updateRun(run, func(run *v1alpha1.Run) {
			run.Spec.Termination.Mode = v1alpha1.RunTerminationImmediate
		}); err != nil {
			t.Fatalf("escalating drain termination error = %v", err)
		}
		if err := updateRun(run, func(run *v1alpha1.Run) {
			run.Spec.Termination.Mode = v1alpha1.RunTerminationDrain
		}); !apierrors.IsInvalid(err) {
			t.Fatalf("downgrading immediate termination error = %v, want Invalid", err)
		}
	})

	t.Run("ttl-remains-mutable", func(t *testing.T) {
		run := newRun(t)
		updatedTTL := int32(120)
		if err := updateRun(run, func(run *v1alpha1.Run) {
			run.Spec.TTLSecondsAfterFinished = &updatedTTL
		}); err != nil {
			t.Fatalf("update run ttl: %v", err)
		}
	})
}

func runSpecWithRequiredAffinity(term v1alpha1.RunAffinityTerm) v1alpha1.RunSpec {
	return v1alpha1.RunSpec{
		Runtime: "bash",
		Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		Affinity: &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{term},
		}},
	}
}

func TestCRDValidationRejectsInvalidWorkflowNeeds(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflow-validation-")

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "unknown-need", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "compile", Run: "echo build"}},
				},
				"deploy": {
					RunsOn: "bash",
					Needs:  []string{"missing"},
					Steps:  []v1alpha1.StepSpec{{Name: "ship", Run: "echo deploy"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflow); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid workflow needs error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsWorkflowWithoutNeeds(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflow-no-needs-")

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "no-needs", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "hello", Run: "echo hello"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflow); err != nil {
		t.Fatalf("create workflow without needs: %v", err)
	}
}

func TestCRDValidationAllowsReusableWorkflowFields(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflow-reusable-")

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "reusable", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowSpec{
			Inputs: map[string]v1alpha1.WorkflowInputSpec{
				"image": {Type: "string", Required: true},
			},
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"image": {Value: "${{ jobs.build.outputs.image }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "package",
						Run:  "echo image=${{ inputs.image }} >> \"$KRUNTIME_OUTPUTS\"",
					}},
				},
				"release": {
					Needs: []string{"build"},
					Uses:  "publish",
					With:  map[string]string{"image": "${{ jobs.build.outputs.image }}"},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
}

func TestCRDValidationRejectsInvalidWorkflowDefinitionShape(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflow-definition-validation-")

	tests := []struct {
		name string
		spec v1alpha1.WorkflowSpec
	}{
		{
			name: "invalid-input-type",
			spec: v1alpha1.WorkflowSpec{
				Inputs: map[string]v1alpha1.WorkflowInputSpec{
					"image": {Type: "number"},
				},
				Jobs: map[string]v1alpha1.JobSpec{
					"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "package", Run: "echo build"}}},
				},
			},
		},
		{
			name: "job-missing-steps-and-uses",
			spec: v1alpha1.WorkflowSpec{
				Jobs: map[string]v1alpha1.JobSpec{
					"build": {RunsOn: "bash"},
				},
			},
		},
		{
			name: "job-steps-and-uses",
			spec: v1alpha1.WorkflowSpec{
				Jobs: map[string]v1alpha1.JobSpec{
					"build": {
						RunsOn: "bash",
						Uses:   "other-workflow",
						Steps:  []v1alpha1.StepSpec{{Name: "package", Run: "echo build"}},
					},
				},
			},
		},
		{
			name: "job-inline-with-inputs",
			spec: v1alpha1.WorkflowSpec{
				Jobs: map[string]v1alpha1.JobSpec{
					"build": {
						RunsOn: "bash",
						With:   map[string]string{"image": "app"},
						Steps:  []v1alpha1.StepSpec{{Name: "package", Run: "echo build"}},
					},
				},
			},
		},
		{
			name: "invalid-job-uses-name",
			spec: v1alpha1.WorkflowSpec{
				Jobs: map[string]v1alpha1.JobSpec{
					"build": {Uses: "bad/name"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: ns.Name},
				Spec:       tt.spec,
			}
			if err := k8sClient.Create(ctx, workflow); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid workflow definition error = %v, want Invalid", err)
			}
		})
	}
}

func TestCRDValidationAllowsInlineWorkflowRun(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflowrun-validation-")

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "inline", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflowRun); err != nil {
		t.Fatalf("create inline workflowrun: %v", err)
	}
}

func TestCRDValidationAllowsWorkflowRunTerminalAPI(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflowrun-terminal-api-")

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cancelled", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowRunSpec{
			CancelRequested: true,
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}},
				},
				"test": {
					RunsOn: "bash",
					Needs:  []string{"build"},
					Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflowRun); err != nil {
		t.Fatalf("create cancelling workflowrun: %v", err)
	}

	workflowRun.Status.Phase = v1alpha1.WorkflowCancelled
	workflowRun.Status.Jobs = map[string]v1alpha1.JobStatus{
		"build": {Phase: v1alpha1.JobFailed},
		"test":  {Phase: v1alpha1.JobSkipped, Pre: []string{"build"}},
	}
	if err := k8sClient.Status().Update(ctx, workflowRun); err != nil {
		t.Fatalf("update workflowrun terminal status: %v", err)
	}
}

func TestCRDValidationAllowsWorkflowRunSnapshotAndCallStatus(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflowrun-snapshot-status-")

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"deploy": {Uses: "deploy-workflow"},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	workflowRun.Status.Phase = v1alpha1.WorkflowPending
	workflowRun.Status.SnapshotName = "release-snapshot-a1b2c3d4"
	workflowRun.Status.Jobs = map[string]v1alpha1.JobStatus{
		"deploy": {Phase: v1alpha1.JobRunning, WorkflowRunName: "release-deploy-a1b2c3d4"},
	}
	if err := k8sClient.Status().Update(ctx, workflowRun); err != nil {
		t.Fatalf("update workflowrun snapshot status: %v", err)
	}
}

func TestCRDValidationWorkflowRunExecutionInputsAreImmutable(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflowrun-immutable-inputs-")

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	workflowRun.Spec.Jobs["build"] = v1alpha1.JobSpec{RunsOn: "python", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}}
	if err := k8sClient.Update(ctx, workflowRun); !apierrors.IsInvalid(err) {
		t.Fatalf("mutating workflowrun jobs error = %v, want Invalid", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
		t.Fatalf("get workflowrun after rejected update: %v", err)
	}
	workflowRun.Spec.CancelRequested = true
	if err := k8sClient.Update(ctx, workflowRun); err != nil {
		t.Fatalf("request workflowrun cancellation: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
		t.Fatalf("get cancelled workflowrun: %v", err)
	}
	workflowRun.Spec.CancelRequested = false
	if err := k8sClient.Update(ctx, workflowRun); !apierrors.IsInvalid(err) {
		t.Fatalf("clearing workflowrun cancellation error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidWorkflowRunShape(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflowrun-shape-validation-")

	tests := []struct {
		name string
		spec v1alpha1.WorkflowRunSpec
	}{
		{
			name: "missing-jobs",
			spec: v1alpha1.WorkflowRunSpec{},
		},
		{
			name: "unknown-need",
			spec: v1alpha1.WorkflowRunSpec{
				Jobs: map[string]v1alpha1.JobSpec{
					"test": {
						RunsOn: "bash",
						Needs:  []string{"missing"},
						Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflowRun := &v1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: ns.Name},
				Spec:       tt.spec,
			}
			if err := k8sClient.Create(ctx, workflowRun); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid workflowrun error = %v, want Invalid", err)
			}
		})
	}
}

func TestCRDValidationAllowsWorkflowActionCallStep(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-call-step-")

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "action-call", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{{
					Name: "setup",
					Uses: "setup-python-tools",
					With: map[string]string{"version": "3.13"},
				}},
			},
		}},
	}
	if err := k8sClient.Create(ctx, workflow); err != nil {
		t.Fatalf("create workflow with Action call: %v", err)
	}
}

func TestCRDValidationRejectsInvalidWorkflowStepShape(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-step-validation-")

	tests := []struct {
		name string
		step v1alpha1.StepSpec
	}{
		{
			name: "missing-run-and-uses",
			step: v1alpha1.StepSpec{Name: "compile"},
		},
		{
			name: "run-and-uses",
			step: v1alpha1.StepSpec{Name: "compile", Run: "echo build", Uses: "future/action"},
		},
		{
			name: "with-without-uses",
			step: v1alpha1.StepSpec{Name: "compile", Run: "echo build", With: map[string]string{"version": "3.13"}},
		},
		{
			name: "action-call-with-args",
			step: v1alpha1.StepSpec{Name: "compile", Uses: "setup-python-tools", Args: []string{"3.13"}},
		},
		{
			name: "action-call-with-env",
			step: v1alpha1.StepSpec{Name: "compile", Uses: "setup-python-tools", Env: map[string]string{"VERSION": "3.13"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: ns.Name},
				Spec: v1alpha1.WorkflowSpec{
					Jobs: map[string]v1alpha1.JobSpec{
						"build": {
							RunsOn: "bash",
							Steps:  []v1alpha1.StepSpec{tt.step},
						},
					},
				},
			}
			if err := k8sClient.Create(ctx, workflow); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid workflow step error = %v, want Invalid", err)
			}
		})
	}
}

func TestCRDValidationRejectsInvalidRuntimeImage(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-runtime-validation-")

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-image", Namespace: ns.Name},
		Spec: v1alpha1.RuntimeSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runtime", Image: ""}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, rt); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid runtime image error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidRuntimeServiceAccountName(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-runtime-sa-validation-")

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-service-account", Namespace: ns.Name},
		Spec: v1alpha1.RuntimeSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "Bad_Name",
					Containers:         []corev1.Container{{Name: "runtime", Image: "runtime:latest"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, rt); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid runtime serviceAccountName error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsMultipleRuntimeWorkspaceVolumeSources(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-runtime-workspace-validation-")

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-workspace", Namespace: ns.Name},
		Spec: v1alpha1.RuntimeSpec{
			Workspace: &v1alpha1.RuntimeWorkspaceSpec{
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "workspace",
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runtime", Image: "runtime:latest"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, rt); !apierrors.IsInvalid(err) {
		t.Fatalf("multiple runtime workspace volume sources error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidPersistentWorkspaceRuntime(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-persistent-workspace-runtime-validation-")

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-runtime", Namespace: ns.Name},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime: "bad/runtime",
		},
	}
	if err := k8sClient.Create(ctx, workspace); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid persistent workspace runtime error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsReadyFunctionEndpointStatus(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-function-endpoint-status-")
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "function", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "python",
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "handler.invoke"}},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return err
		}

		return updateReadyFunctionEndpointStatus(ctx, run)
	}); err != nil {
		t.Fatalf("update ready function status: %v", err)
	}
}

func updateReadyFunctionEndpointStatus(ctx context.Context, run *v1alpha1.Run) error {
	run.Status.Phase = v1alpha1.RunReady
	run.Status.AssignedPod = "runtime-python-abc"
	run.Status.AssignedPodUID = "a0dc4d2d-2cf3-4f91-a0f2-d7d7bb3c7ae4"
	run.Status.Endpoint = &v1alpha1.RunEndpoint{
		Protocol: v1alpha1.RunEndpointProtocolHTTPS,
		URL:      "https://python-gateway.default.svc.cluster.local/v1/namespaces/default/runs/function/uid/invoke",
		CABundle: []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----"),
	}
	return k8sClient.Status().Update(ctx, run)
}

func TestCRDValidationRejectsUnsupportedFunctionEndpointProtocol(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-function-endpoint-protocol-")
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "function", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "python",
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "handler.invoke"}},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return err
		}
		run.Status.Phase = v1alpha1.RunReady
		run.Status.Endpoint = &v1alpha1.RunEndpoint{Protocol: v1alpha1.RunEndpointProtocol("FTP"), URL: "ftp://example.invalid/invoke"}
		return k8sClient.Status().Update(ctx, run)
	})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("invalid endpoint protocol error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsPersistentWorkspaceBindingStatus(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-persistent-workspace-binding-status-")

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "bound-workspace", Namespace: ns.Name},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}
	workspace.Status = v1alpha1.PersistentWorkspaceStatus{
		Phase:       v1alpha1.PersistentWorkspaceBound,
		Runtime:     "bash",
		BoundPod:    "runtime-bash-abcde",
		BoundPodUID: "2c24c1f0-9f8f-4f80-82d5-3dd16a12d1e6",
		Path:        "/workspace/persistent/bound-workspace",
	}
	if err := k8sClient.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("update PersistentWorkspace status: %v", err)
	}

	var got v1alpha1.PersistentWorkspace
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatalf("get PersistentWorkspace: %v", err)
	}
	if got.Status.Phase != v1alpha1.PersistentWorkspaceBound || got.Status.BoundPodUID != workspace.Status.BoundPodUID {
		t.Fatalf("PersistentWorkspace status = %#v, want Bound status with fenced Pod UID", got.Status)
	}
}

func TestCRDValidationRejectsInvalidPersistentWorkspaceMode(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-persistent-workspace-mode-validation-")

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-mode", Namespace: ns.Name},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime:       "bash",
			Mode:          v1alpha1.PersistentWorkspaceMode("PVC"),
			CleanupPolicy: v1alpha1.PersistentWorkspaceDeleteAfterTTL,
		},
	}
	if err := k8sClient.Create(ctx, workspace); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid persistent workspace mode error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsActionWithoutSteps(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-steps-validation-")

	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns.Name},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{},
		},
	}
	if err := k8sClient.Create(ctx, action); !apierrors.IsInvalid(err) {
		t.Fatalf("empty action steps error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidActionInputType(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-input-validation-")

	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-input", Namespace: ns.Name},
		Spec: v1alpha1.ActionSpec{
			Inputs: map[string]v1alpha1.ActionInputSpec{
				"version": {Type: v1alpha1.ActionInputType("number")},
			},
			Steps: []v1alpha1.StepSpec{{Name: "setup", Run: "echo setup"}},
		},
	}
	if err := k8sClient.Create(ctx, action); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid action input type error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsActionStepUses(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-step-uses-validation-")

	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "nested-uses", Namespace: ns.Name},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: "another-action"}},
		},
	}
	if err := k8sClient.Create(ctx, action); !apierrors.IsInvalid(err) {
		t.Fatalf("action step uses error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsActionStepStatus(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-step-status-")

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: "setup-python-tools"}}},
		}},
	}
	if err := k8sClient.Create(ctx, workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	workflowRun.Status.Phase = v1alpha1.WorkflowRunning
	workflowRun.Status.Jobs = map[string]v1alpha1.JobStatus{
		"build": {
			Phase: v1alpha1.JobRunning,
			Steps: []v1alpha1.StepStatus{{
				Name:  "setup",
				Phase: v1alpha1.StepRunning,
				ActionSteps: []v1alpha1.ActionStepStatus{{
					Name:    "install",
					Phase:   v1alpha1.StepSucceeded,
					RunName: "build-setup-install",
					Outputs: map[string]string{"version": "3.13"},
				}},
			}},
		},
	}
	if err := k8sClient.Status().Update(ctx, workflowRun); err != nil {
		t.Fatalf("update Action step status: %v", err)
	}
}

func testNamespace(t *testing.T, generateName string) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{}
	ns.GenerateName = generateName
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ns) })
	return ns
}

func createReadyRuntimePod(t *testing.T, ctx context.Context, namespace, name string, runsCapacity int64) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"runtime": "bash"},
			Annotations: map[string]string{
				runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): resource.NewQuantity(runsCapacity, resource.DecimalSI).String(),
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "busybox"}}},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create runtime pod %q: %v", name, err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: metav1.Now(), LastTransitionTime: metav1.Now()},
	}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update runtime pod %q status: %v", name, err)
	}
	return pod
}

func createAssignedRun(t *testing.T, ctx context.Context, namespace, name, podName string, labels map[string]string) {
	t.Helper()
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create assigned run %q: %v", name, err)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return err
		}
		run.Status.Phase = v1alpha1.RunScheduled
		run.Status.AssignedPod = podName
		return k8sClient.Status().Update(ctx, run)
	}); err != nil {
		t.Fatalf("assign run %q to pod %q: %v", name, podName, err)
	}
}

func waitForRunAssignment(t *testing.T, ctx context.Context, run *v1alpha1.Run) v1alpha1.Run {
	t.Helper()
	var updated v1alpha1.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get run %q: %v", run.Name, err)
		}
		if updated.Status.Phase == v1alpha1.RunScheduled && updated.Status.AssignedPod != "" {
			return updated
		}
	}
	t.Fatalf("run %q was not scheduled: phase=%s assignedPod=%q", run.Name, updated.Status.Phase, updated.Status.AssignedPod)
	return v1alpha1.Run{}
}
