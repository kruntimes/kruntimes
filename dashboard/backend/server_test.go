package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

type staticRequestClients struct {
	client client.Client
	err    error
}

type staticTokenAuthenticator struct {
	accountName string
	err         error
	token       string
}

func (a *staticTokenAuthenticator) Authenticate(_ context.Context, token string) (string, error) {
	a.token = token
	if a.err != nil {
		return "", a.err
	}
	return a.accountName, nil
}

func (s staticRequestClients) ClientForRequest(*http.Request) (client.Client, error) {
	return s.client, s.err
}

type staticRunLogClient struct {
	statusCode int
	body       string
	err        error
	request    struct {
		token, namespace, runUID string
		tailLines                int64
		follow                   bool
	}
}

func (g *staticRunLogClient) RunLogs(_ context.Context, token, namespace, runUID string, tailLines int64, follow bool) (*http.Response, error) {
	g.request.token, g.request.namespace, g.request.runUID = token, namespace, runUID
	g.request.tailLines, g.request.follow = tailLines, follow
	if g.err != nil {
		return nil, g.err
	}
	statusCode := g.statusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	contentType := "application/json"
	if follow {
		contentType = "application/x-ndjson"
	}
	return &http.Response{StatusCode: statusCode, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(g.body))}, nil
}

func TestServerListsNamespacesAndRuns(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC))
	readyRun := dashboardRun("ready", "team-a", "python", v1alpha1.RunReady, now)
	readyRun.Labels = map[string]string{"app": "dashboard"}
	taskRun := dashboardRun("task", "team-a", "bash", v1alpha1.RunSucceeded, now)
	otherNamespace := dashboardRun("other", "team-b", "python", v1alpha1.RunReady, now)
	server := dashboardTestServer(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}},
		readyRun, taskRun, otherNamespace,
	)

	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list namespaces status = %d, body = %s", response.Code, response.Body.String())
	}
	var namespaces namespaceListResponse
	decodeDashboardResponse(t, response, &namespaces)
	if len(namespaces.Items) != 2 || namespaces.Items[0] != "team-a" || namespaces.Items[1] != "team-b" {
		t.Fatalf("namespaces = %#v, want team-a and team-b", namespaces.Items)
	}

	response = requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs?labelSelector=app%3Ddashboard", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list Runs status = %d, body = %s", response.Code, response.Body.String())
	}
	var runs runListResponse
	decodeDashboardResponse(t, response, &runs)
	if len(runs.Items) != 1 {
		t.Fatalf("Run list = %#v, want one matching Run", runs.Items)
	}
	if got := runs.Items[0]; got.Name != "ready" || got.Namespace != "team-a" || got.Runtime != "python" || got.Phase != v1alpha1.RunReady || got.LastTransitionReason != "FunctionRegistered" {
		t.Fatalf("Run summary = %#v", got)
	}
}

func TestServerGetsAuthorizedRunDetail(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC))
	run := dashboardRun("detail", "team-a", "python", v1alpha1.RunReady, now)
	run.Spec.Env = []corev1.EnvVar{{Name: "SECRET", Value: "must-not-leak"}}
	run.Status.Message = "ready for requests"
	run.Status.Outputs = map[string]string{"answer": "42"}
	run.Status.ArtifactRefs = []v1alpha1.ArtifactRef{{Name: "result", Driver: v1alpha1.ArtifactDriverS3}}
	server := dashboardTestServer(t, run)

	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs/detail", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get Run status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	decodeDashboardResponse(t, response, &body)
	if _, exists := body["spec"]; !exists {
		t.Fatalf("Dashboard Run detail must expose Run spec to its authorized caller: %s", response.Body.String())
	}
	if _, exists := body["status"]; !exists {
		t.Fatalf("Dashboard Run detail must expose Run status to its authorized caller: %s", response.Body.String())
	}
	var outputs map[string]string
	if err := json.Unmarshal(body["outputs"], &outputs); err != nil {
		t.Fatalf("decode outputs: %v", err)
	}
	if outputs["answer"] != "42" {
		t.Fatalf("outputs = %#v", outputs)
	}
	var artifacts []v1alpha1.ArtifactRef
	if err := json.Unmarshal(body["artifactRefs"], &artifacts); err != nil {
		t.Fatalf("decode artifact refs: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Name != "result" {
		t.Fatalf("artifact refs = %#v", artifacts)
	}
}

func TestServerRejectsMissingTokenAndMapsKubernetesErrors(t *testing.T) {
	server := &Server{Clients: staticRequestClients{err: ErrMissingBearerToken}}
	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want %d", response.Code, http.StatusUnauthorized)
	}

	server.Clients = staticRequestClients{err: apierrors.NewForbidden(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "runs"}, "run", errors.New("denied"))}
	response = requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs", nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("forbidden status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestServerPublicReadOnlyPermitsLists(t *testing.T) {
	scheme := dashboardScheme(t)
	publicClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		dashboardRun("example", "team-a", "python", v1alpha1.RunPending, metav1.Now()),
		&v1alpha1.Runtime{ObjectMeta: metav1.ObjectMeta{Name: "python", Namespace: "team-a"}},
		&v1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: "team-a"}},
	).Build()
	server := &Server{Clients: staticRequestClients{err: apierrors.NewForbidden(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "runs"}, "example", errors.New("caller cannot list Runs"))}, PublicReadClient: publicClient}
	for _, path := range []string{"/api/namespaces", "/api/namespaces/team-a/runs", "/api/namespaces/team-a/runtimes", "/api/namespaces/team-a/workflowruns"} {
		response := requestDashboard(t, server, http.MethodGet, path, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("public %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs/example", nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("public detail status = %d, want forbidden", response.Code)
	}
}

func TestServerRelaysLogsThroughAggregatedAPI(t *testing.T) {
	scheme := dashboardScheme(t)
	run := dashboardRun("logs", "team-a", "python", v1alpha1.RunRunning, metav1.Now())
	clients := &staticRequestClients{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()}
	server := dashboardTestServerWithClients(t, clients)
	logs := &staticRunLogClient{body: `{"items":[{"stream":"stdout","message":"visible"}]}`}
	server.Logs = logs

	request := httptest.NewRequest(http.MethodGet, "/api/namespaces/team-a/runs/logs/logs", nil)
	request.Header.Set("Authorization", "Bearer caller-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "visible") {
		t.Fatalf("get logs status = %d, body = %s", response.Code, response.Body.String())
	}
	if logs.request.token != "caller-token" || logs.request.namespace != "team-a" || logs.request.runUID != "logs" {
		t.Fatalf("aggregated log request = %#v", logs.request)
	}
}

func TestServerListsRuntimeTopologyAndWorkflowRuns(t *testing.T) {
	now := metav1.Now()
	runtimeObject := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "python", Namespace: "team-a"},
		Spec: v1alpha1.RuntimeSpec{Replicas: 1, Capacity: &v1alpha1.RuntimeCapacity{Resources: corev1.ResourceList{
			v1alpha1.RuntimeResourceRuns: resource.MustParse("2"),
		}}},
		Status: v1alpha1.RuntimeStatus{ReadyReplicas: 1},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "python-0", Namespace: "team-a", Labels: map[string]string{"runtime": "python"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}, {Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue}}}}
	idlePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "python-1", Namespace: "team-a", Labels: map[string]string{"runtime": "python"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	run := dashboardRun("owned", "team-a", "python", v1alpha1.RunRunning, now)
	run.Status.AssignedPod = "python-0"
	workflowRun := &v1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: "team-a", UID: "workflow-uid", CreationTimestamp: now}, Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{"build": {}}}, Status: v1alpha1.WorkflowRunStatus{Phase: v1alpha1.WorkflowRunning, Jobs: map[string]v1alpha1.JobStatus{"build": {Phase: v1alpha1.JobRunning}}}}
	server := dashboardTestServer(t, runtimeObject, pod, idlePod, run, workflowRun)

	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runtimes", nil)
	var runtimes runtimeListResponse
	decodeDashboardResponse(t, response, &runtimes)
	if response.Code != http.StatusOK || len(runtimes.Items) != 1 || !runtimes.Items[0].Healthy || runtimes.Items[0].RunCount != 1 || runtimes.Items[0].Capacity["runs"] != "2" {
		t.Fatalf("runtime list = %#v, status = %d", runtimes, response.Code)
	}

	response = requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runtimes/python", nil)
	var runtimeDetail runtimeDetailResponse
	decodeDashboardResponse(t, response, &runtimeDetail)
	if response.Code != http.StatusOK || len(runtimeDetail.Pods) != 2 || !runtimeDetail.Pods[0].RuntimedReady || len(runtimeDetail.Pods[0].Runs) != 1 || runtimeDetail.Pods[1].Runs == nil {
		t.Fatalf("runtime detail = %#v, status = %d", runtimeDetail, response.Code)
	}

	response = requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/workflowruns", nil)
	var workflowRuns workflowRunListResponse
	decodeDashboardResponse(t, response, &workflowRuns)
	if response.Code != http.StatusOK || len(workflowRuns.Items) != 1 || workflowRuns.Items[0].JobCount != 1 {
		t.Fatalf("workflow list = %#v, status = %d", workflowRuns, response.Code)
	}
	response = requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/workflowruns/workflow", nil)
	var workflowDetail workflowRunDetailResponse
	decodeDashboardResponse(t, response, &workflowDetail)
	if response.Code != http.StatusOK || workflowDetail.Status.Jobs["build"].Phase != v1alpha1.JobRunning {
		t.Fatalf("workflow detail = %#v, status = %d", workflowDetail, response.Code)
	}
}

func TestServerRejectsInvalidListRequests(t *testing.T) {
	server := dashboardTestServer(t)
	for _, path := range []string{
		"/api/namespaces/team-a/runs?limit=0",
		"/api/namespaces/team-a/runs?limit=201",
		"/api/namespaces/team-a/runs?labelSelector=app%20in%20(",
		"/api/namespaces/team-a/runs?phase=Ready",
		"/api/namespaces/team-a/runs?runtime=python",
	} {
		response := requestDashboard(t, server, http.MethodGet, path, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want %d", path, response.Code, http.StatusBadRequest)
		}
	}
}

func TestServerRoutesMethodsAndMissingEndpoints(t *testing.T) {
	server := dashboardTestServer(t)

	response := requestDashboard(t, server, http.MethodPost, "/api/namespaces", nil)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	var methodError errorResponse
	decodeDashboardResponse(t, response, &methodError)
	if methodError.Error != "method not allowed" {
		t.Fatalf("POST error = %q", methodError.Error)
	}

	response = requestDashboard(t, server, http.MethodGet, "/api/unknown", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing route status = %d, want %d", response.Code, http.StatusNotFound)
	}
	var routeError errorResponse
	decodeDashboardResponse(t, response, &routeError)
	if routeError.Error != "endpoint not found" {
		t.Fatalf("missing route error = %q", routeError.Error)
	}
}

func TestServerCreatesAndClearsHTTPSessionCookie(t *testing.T) {
	server := dashboardTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"token":"caller-token"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create session status = %d, body = %s", response.Code, response.Body.String())
	}
	var session sessionResponse
	decodeDashboardResponse(t, response, &session)
	if !session.Authenticated || session.AccountName != "system:serviceaccount:team-a:viewer" {
		t.Fatalf("create session = %#v", session)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookieName || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %#v", cookies)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	decodeDashboardResponse(t, response, &session)
	if !session.Authenticated || session.AccountName != "system:serviceaccount:team-a:viewer" {
		t.Fatalf("session = %#v, want authenticated", session)
	}
	response = requestDashboard(t, server, http.MethodGet, "/api/session", nil)
	decodeDashboardResponse(t, response, &session)
	if session.Authenticated {
		t.Fatalf("session = %#v, want unauthenticated", session)
	}
	response = requestDashboard(t, server, http.MethodDelete, "/api/session", nil)
	if response.Code != http.StatusNoContent || response.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("clear session response = %d, cookies = %#v", response.Code, response.Result().Cookies())
	}
}

func TestServerRejectsInvalidSessionTokenAndClearsExpiredSession(t *testing.T) {
	server := dashboardTestServer(t)
	authenticator := server.Authenticator.(*staticTokenAuthenticator)
	authenticator.err = ErrInvalidBearerToken
	response := requestDashboard(t, server, http.MethodPost, "/api/session", http.Header{"Content-Type": {"application/json"}})
	// A request with no body is malformed before it reaches TokenReview.
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing token status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"token":"invalid-token"}`))
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "expired-token"})
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var session sessionResponse
	decodeDashboardResponse(t, response, &session)
	if session.Authenticated || len(response.Result().Cookies()) != 1 || response.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("expired session = %#v, cookies = %#v", session, response.Result().Cookies())
	}
}

func TestServerServesFilesystemFrontend(t *testing.T) {
	server := dashboardTestServer(t)
	response := requestDashboard(t, server, http.MethodGet, "/", nil)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("frontend status/content type = %d/%q", response.Code, response.Header().Get("Content-Type"))
	}
	if !strings.Contains(response.Body.String(), "id=\"root\"") || !strings.Contains(response.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("frontend response is missing expected content or CSP: %q", response.Body.String())
	}

	response = requestDashboard(t, server, http.MethodGet, "/assets/dashboard.js", nil)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || !strings.Contains(response.Body.String(), "createRoot") {
		t.Fatalf("frontend script status/content = %d/%q/%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	response = requestDashboard(t, server, http.MethodGet, "/namespaces/team-a/runs/example", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "id=\"root\"") {
		t.Fatalf("deep-link frontend status/body = %d/%q", response.Code, response.Body.String())
	}
}

func TestServerRelaysAggregatedRunLogs(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC))
	run := dashboardRun("logs", "team-a", "python", v1alpha1.RunRunning, now)
	server := dashboardTestServer(t, run)
	logs := &staticRunLogClient{body: `{"items":[{"stream":"stderr","message":"second"},{"stream":"audit","message":"finished","invocationId":"invoke-1","durationMilliseconds":12}]}`}
	server.Logs = logs

	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs/logs/logs?tail=2", http.Header{"Authorization": {"Bearer caller-token"}})
	if response.Code != http.StatusOK {
		t.Fatalf("get logs status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"durationMilliseconds":12`) || logs.request.tailLines != 2 || logs.request.follow {
		t.Fatalf("response/aggregated request = %s/%#v", response.Body.String(), logs.request)
	}
}

func TestServerRelaysAggregatedAuthorizationFailure(t *testing.T) {
	run := dashboardRun("logs", "team-a", "python", v1alpha1.RunRunning, metav1.Now())
	server := dashboardTestServer(t, run)
	server.Logs = &staticRunLogClient{statusCode: http.StatusForbidden, body: `{"error":"Kubernetes authorization denied"}`}

	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs/logs/logs", http.Header{"Authorization": {"Bearer caller-token"}})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "Kubernetes authorization denied") {
		t.Fatalf("log authorization response = %d/%s", response.Code, response.Body.String())
	}
}

func TestServerStreamsAggregatedRunLogs(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC))
	run := dashboardRun("follow", "team-a", "python", v1alpha1.RunRunning, now)
	server := dashboardTestServer(t, run)
	logs := &staticRunLogClient{body: `{"stream":"stdout","message":"followed"}` + "\n"}
	server.Logs = logs

	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs/follow/logs?follow=true", http.Header{"Authorization": {"Bearer caller-token"}})
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("follow status/content type = %d/%q", response.Code, response.Header().Get("Content-Type"))
	}
	if !strings.Contains(response.Body.String(), "followed") || !logs.request.follow {
		t.Fatalf("follow response/aggregated request = %q/%#v", response.Body.String(), logs.request)
	}
}

func TestServerRejectsInvalidLogRequests(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC))
	unassigned := dashboardRun("unassigned", "team-a", "python", v1alpha1.RunPending, now)
	server := dashboardTestServer(t, unassigned)
	for _, path := range []string{
		"/api/namespaces/team-a/runs/unassigned/logs?tail=0",
		"/api/namespaces/team-a/runs/unassigned/logs?tail=501",
		"/api/namespaces/team-a/runs/unassigned/logs?follow=maybe",
	} {
		response := requestDashboard(t, server, http.MethodGet, path, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want %d", path, response.Code, http.StatusBadRequest)
		}
	}
	server.Logs = nil
	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs/unassigned/logs", nil)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "aggregated Run log API") {
		t.Fatalf("unconfigured log API status = %d, body = %s", response.Code, response.Body.String())
	}
}

func dashboardTestServer(t *testing.T, objects ...client.Object) *Server {
	t.Helper()
	scheme := dashboardScheme(t)
	return dashboardTestServerWithClients(t, &staticRequestClients{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()})
}

func dashboardTestServerWithClients(t *testing.T, clients *staticRequestClients, objects ...client.Object) *Server {
	t.Helper()
	if clients.client == nil {
		scheme := dashboardScheme(t)
		clients.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	}
	return &Server{Clients: clients, Authenticator: &staticTokenAuthenticator{accountName: "system:serviceaccount:team-a:viewer"}, Logs: &staticRunLogClient{body: `{"items":[]}`}, Assets: fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<div id=\"root\"></div>")},
		"dashboard.js":  &fstest.MapFile{Data: []byte("createRoot(document.getElementById('root'))")},
		"dashboard.css": &fstest.MapFile{Data: []byte("body { margin: 0; }")},
	}}
}

func dashboardScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}
	return scheme
}

func dashboardRun(name, namespace, runtimeName string, phase v1alpha1.RunPhase, now metav1.Time) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name + "-uid"), CreationTimestamp: now},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName},
		Status: v1alpha1.RunStatus{
			Phase:       phase,
			AssignedPod: "runtime-pod",
			Attempt:     1,
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "FunctionRegistered",
				LastTransitionTime: now,
			}},
		},
	}
}

func requestDashboard(t *testing.T, server http.Handler, method, path string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func decodeDashboardResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}
