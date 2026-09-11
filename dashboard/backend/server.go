package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	defaultRunListLimit int64 = 50
	maxRunListLimit     int64 = 200
	defaultLogTailLines int64 = 100
	maxLogTailLines     int64 = 500
	maxLogBytes         int64 = 1 << 20
)

// RequestClientProvider resolves a Kubernetes client for one HTTP request.
// RequestClientFactory is the production implementation.
type RequestClientProvider interface {
	ClientForRequest(*http.Request) (client.Client, error)
}

// Server exposes the Dashboard's internal, read-only HTTP API.
type Server struct {
	Clients RequestClientProvider
	// Authenticator validates login credentials with Kubernetes TokenReview.
	// It is separate from Clients because TokenReview must run as the Dashboard
	// ServiceAccount, while resource requests run as the logged-in user.
	Authenticator TokenAuthenticator
	// PublicReadClient is intentionally optional. When configured it serves the
	// safe namespace and Run list endpoints, and resolves a Run's assigned Pod
	// for unauthenticated list requests. Reading logs always requires the
	// caller's credential and goes through Gateway.
	PublicReadClient client.Client
	Gateway          RunLogGateway
	Assets           fs.FS

	routesOnce sync.Once
	routes     http.Handler
}

type errorResponse struct {
	Error string `json:"error"`
}

type namespaceListResponse struct {
	Items []string `json:"items"`
}

type runListResponse struct {
	Items    []runSummary `json:"items"`
	Continue string       `json:"continue,omitempty"`
}

type runSummary struct {
	Name                 string            `json:"name"`
	Namespace            string            `json:"namespace"`
	UID                  types.UID         `json:"uid"`
	Runtime              string            `json:"runtime"`
	Mode                 string            `json:"mode"`
	Phase                v1alpha1.RunPhase `json:"phase"`
	AssignedPod          string            `json:"assignedPod,omitempty"`
	Attempt              int32             `json:"attempt,omitempty"`
	CreationTimestamp    metav1.Time       `json:"creationTimestamp"`
	StartTime            *metav1.Time      `json:"startTime,omitempty"`
	CompletionTime       *metav1.Time      `json:"completionTime,omitempty"`
	LastTransitionReason string            `json:"lastTransitionReason,omitempty"`
}

type runDetailResponse struct {
	runSummary
	Spec         v1alpha1.RunSpec       `json:"spec"`
	Status       v1alpha1.RunStatus     `json:"status"`
	Message      string                 `json:"message,omitempty"`
	Endpoint     *v1alpha1.RunEndpoint  `json:"endpoint,omitempty"`
	Conditions   []metav1.Condition     `json:"conditions,omitempty"`
	Outputs      map[string]string      `json:"outputs,omitempty"`
	ArtifactRefs []v1alpha1.ArtifactRef `json:"artifactRefs,omitempty"`
}

type runtimeSummary struct {
	Name          string            `json:"name"`
	Namespace     string            `json:"namespace"`
	Replicas      int32             `json:"replicas"`
	ReadyReplicas int32             `json:"readyReplicas"`
	Capacity      map[string]string `json:"capacity,omitempty"`
	RunCount      int               `json:"runCount"`
	Healthy       bool              `json:"healthy"`
}

type runtimeListResponse struct {
	Items []runtimeSummary `json:"items"`
}

type runtimeDetailResponse struct {
	Runtime runtimeSummary         `json:"runtime"`
	Spec    v1alpha1.RuntimeSpec   `json:"spec"`
	Status  v1alpha1.RuntimeStatus `json:"status"`
	Pods    []runtimePodSummary    `json:"pods"`
}

type runtimePodSummary struct {
	Name          string          `json:"name"`
	Phase         corev1.PodPhase `json:"phase"`
	Ready         bool            `json:"ready"`
	RuntimedReady bool            `json:"runtimedReady"`
	Runs          []runSummary    `json:"runs"`
}

type workflowRunSummary struct {
	Name              string                 `json:"name"`
	Namespace         string                 `json:"namespace"`
	UID               types.UID              `json:"uid"`
	Phase             v1alpha1.WorkflowPhase `json:"phase"`
	JobCount          int                    `json:"jobCount"`
	CreationTimestamp metav1.Time            `json:"creationTimestamp"`
}

type workflowRunListResponse struct {
	Items []workflowRunSummary `json:"items"`
}

type workflowRunDetailResponse struct {
	workflowRunSummary
	Spec   v1alpha1.WorkflowRunSpec   `json:"spec"`
	Status v1alpha1.WorkflowRunStatus `json:"status"`
}

type sessionRequest struct {
	Token string `json:"token"`
}

type sessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	AccountName   string `json:"accountName,omitempty"`
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && !(request.URL.Path == "/api/session" && (request.Method == http.MethodPost || request.Method == http.MethodDelete)) {
		s.writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.routesOnce.Do(s.registerRoutes)
	s.routes.ServeHTTP(writer, request)
}

func (s *Server) registerRoutes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/session", s.createSession)
	mux.HandleFunc("GET /api/session", s.getSession)
	mux.HandleFunc("DELETE /api/session", s.deleteSession)
	mux.HandleFunc("GET /api/namespaces", s.withReadClient(s.listNamespaces))
	mux.HandleFunc("GET /api/namespaces/{namespace}/runs", s.withReadClient(s.listRuns))
	mux.HandleFunc("GET /api/namespaces/{namespace}/runs/{name}", s.withRequestClient(s.getRun))
	mux.HandleFunc("GET /api/namespaces/{namespace}/runs/{name}/logs", s.getRunLogs)
	mux.HandleFunc("GET /api/namespaces/{namespace}/runtimes", s.withReadClient(s.listRuntimes))
	mux.HandleFunc("GET /api/namespaces/{namespace}/runtimes/{name}", s.withRequestClient(s.getRuntime))
	mux.HandleFunc("GET /api/namespaces/{namespace}/workflowruns", s.withReadClient(s.listWorkflowRuns))
	mux.HandleFunc("GET /api/namespaces/{namespace}/workflowruns/{name}", s.withRequestClient(s.getWorkflowRun))
	mux.HandleFunc("GET /", s.serveFrontend)
	s.routes = mux
}

func (s *Server) getSession(writer http.ResponseWriter, request *http.Request) {
	cookie, err := request.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		s.writeJSON(writer, http.StatusOK, sessionResponse{})
		return
	}
	accountName, err := s.authenticate(request, cookie.Value)
	if errors.Is(err, ErrInvalidBearerToken) {
		s.clearSessionCookie(writer)
		s.writeJSON(writer, http.StatusOK, sessionResponse{})
		return
	}
	if err != nil {
		s.writeError(writer, http.StatusServiceUnavailable, "verify Dashboard session")
		return
	}
	s.writeJSON(writer, http.StatusOK, sessionResponse{Authenticated: true, AccountName: accountName})
}

func (s *Server) createSession(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	var body sessionRequest
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		s.writeError(writer, http.StatusBadRequest, "a Kubernetes bearer token is required")
		return
	}
	accountName, err := s.authenticate(request, strings.TrimSpace(body.Token))
	if errors.Is(err, ErrInvalidBearerToken) {
		s.writeError(writer, http.StatusUnauthorized, "Kubernetes bearer token is invalid")
		return
	}
	if err != nil {
		s.writeError(writer, http.StatusServiceUnavailable, "verify Kubernetes bearer token")
		return
	}
	http.SetCookie(writer, &http.Cookie{Name: SessionCookieName, Value: strings.TrimSpace(body.Token), Path: "/", MaxAge: 8 * 60 * 60, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	s.writeJSON(writer, http.StatusOK, sessionResponse{Authenticated: true, AccountName: accountName})
}

func (s *Server) deleteSession(writer http.ResponseWriter, _ *http.Request) {
	s.clearSessionCookie(writer)
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticate(request *http.Request, token string) (string, error) {
	if s.Authenticator == nil {
		return "", errors.New("dashboard TokenReview authenticator is not configured")
	}
	return s.Authenticator.Authenticate(request.Context(), token)
}

func (s *Server) clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) serveFrontend(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	requestPath := path.Clean(request.URL.Path)
	if requestPath == "/" {
		requestPath = "/assets/index.html"
	}
	if strings.HasPrefix(requestPath, "/api/") {
		s.writeError(writer, http.StatusNotFound, "endpoint not found")
		return
	}
	if !strings.HasPrefix(requestPath, "/assets/") {
		// Client-side routes are real, bookmarkable pages. Asset paths always
		// live below /assets, so serving index.html here cannot mask one.
		requestPath = "/assets/index.html"
	}
	if s.Assets == nil {
		s.writeError(writer, http.StatusInternalServerError, "dashboard assets are not configured")
		return
	}
	assetPath := strings.TrimPrefix(requestPath, "/assets/")
	asset, err := fs.ReadFile(s.Assets, assetPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.writeError(writer, http.StatusNotFound, "page not found")
			return
		}
		s.writeError(writer, http.StatusInternalServerError, "dashboard asset request failed")
		return
	}
	http.ServeContent(writer, request, path.Base(assetPath), time.Time{}, bytes.NewReader(asset))
}

func (s *Server) withReadClient(handler func(http.ResponseWriter, *http.Request, client.Client)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if s.Clients == nil {
			s.writeError(writer, http.StatusServiceUnavailable, "dashboard Kubernetes client is not configured")
			return
		}
		// These endpoints are intentionally public when PublicReadClient is
		// configured. Do not switch to a cookie token merely because one exists:
		// a logs-only token need not have Namespace or Run list permissions.
		if s.PublicReadClient != nil {
			handler(writer, request, s.PublicReadClient)
			return
		}
		kubernetesClient, err := s.Clients.ClientForRequest(request)
		if err == nil {
			handler(writer, request, kubernetesClient)
			return
		}
		s.writeKubernetesError(writer, err)
	}
}

func (s *Server) withRequestClient(handler func(http.ResponseWriter, *http.Request, client.Client)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if s.Clients == nil {
			s.writeError(writer, http.StatusServiceUnavailable, "dashboard Kubernetes client is not configured")
			return
		}
		kubernetesClient, err := s.Clients.ClientForRequest(request)
		if err != nil {
			s.writeKubernetesError(writer, err)
			return
		}
		handler(writer, request, kubernetesClient)
	}
}

func (s *Server) listNamespaces(writer http.ResponseWriter, request *http.Request, kubernetesClient client.Client) {
	var namespaces corev1.NamespaceList
	if err := kubernetesClient.List(request.Context(), &namespaces); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	items := make([]string, 0, len(namespaces.Items))
	for _, namespace := range namespaces.Items {
		items = append(items, namespace.Name)
	}
	s.writeJSON(writer, http.StatusOK, namespaceListResponse{Items: items})
}

func (s *Server) listRuns(writer http.ResponseWriter, request *http.Request, kubernetesClient client.Client) {
	options, err := parseRunListOptions(request)
	if err != nil {
		s.writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	var runs v1alpha1.RunList
	if err := kubernetesClient.List(request.Context(), &runs, append(options, client.InNamespace(request.PathValue("namespace")))...); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	items := make([]runSummary, 0, len(runs.Items))
	for i := range runs.Items {
		items = append(items, summaryForRun(&runs.Items[i]))
	}
	s.writeJSON(writer, http.StatusOK, runListResponse{Items: items, Continue: runs.Continue})
}

func parseRunListOptions(request *http.Request) ([]client.ListOption, error) {
	query := request.URL.Query()
	if query.Get("phase") != "" || query.Get("runtime") != "" {
		return nil, errors.New("phase and runtime filters are not supported yet")
	}
	limit := defaultRunListLimit
	if rawLimit := query.Get("limit"); rawLimit != "" {
		parsed, err := strconv.ParseInt(rawLimit, 10, 64)
		if err != nil || parsed < 1 || parsed > maxRunListLimit {
			return nil, fmt.Errorf("limit must be between 1 and %d", maxRunListLimit)
		}
		limit = parsed
	}
	options := []client.ListOption{client.Limit(limit)}
	if continuation := query.Get("continue"); continuation != "" {
		options = append(options, client.Continue(continuation))
	}
	if selector := query.Get("labelSelector"); selector != "" {
		parsed, err := labels.Parse(selector)
		if err != nil {
			return nil, fmt.Errorf("invalid labelSelector: %w", err)
		}
		options = append(options, client.MatchingLabelsSelector{Selector: parsed})
	}
	return options, nil
}

func (s *Server) getRun(writer http.ResponseWriter, request *http.Request, kubernetesClient client.Client) {
	run := &v1alpha1.Run{}
	if err := kubernetesClient.Get(request.Context(), client.ObjectKey{Namespace: request.PathValue("namespace"), Name: request.PathValue("name")}, run); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	s.writeJSON(writer, http.StatusOK, runDetailResponse{
		runSummary:   summaryForRun(run),
		Spec:         *run.Spec.DeepCopy(),
		Status:       *run.Status.DeepCopy(),
		Message:      run.Status.Message,
		Endpoint:     run.Status.Endpoint,
		Conditions:   append([]metav1.Condition(nil), run.Status.Conditions...),
		Outputs:      copyStringMap(run.Status.Outputs),
		ArtifactRefs: append([]v1alpha1.ArtifactRef(nil), run.Status.ArtifactRefs...),
	})
}

func (s *Server) listRuntimes(writer http.ResponseWriter, request *http.Request, kubernetesClient client.Client) {
	var runtimes v1alpha1.RuntimeList
	if err := kubernetesClient.List(request.Context(), &runtimes, client.InNamespace(request.PathValue("namespace"))); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	var runs v1alpha1.RunList
	if err := kubernetesClient.List(request.Context(), &runs, client.InNamespace(request.PathValue("namespace"))); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	counts := make(map[string]int)
	for i := range runs.Items {
		counts[runs.Items[i].Spec.Runtime]++
	}
	items := make([]runtimeSummary, 0, len(runtimes.Items))
	for i := range runtimes.Items {
		items = append(items, summaryForRuntime(&runtimes.Items[i], counts[runtimes.Items[i].Name]))
	}
	s.writeJSON(writer, http.StatusOK, runtimeListResponse{Items: items})
}

func (s *Server) getRuntime(writer http.ResponseWriter, request *http.Request, kubernetesClient client.Client) {
	namespace, name := request.PathValue("namespace"), request.PathValue("name")
	runtimeObject := &v1alpha1.Runtime{}
	if err := kubernetesClient.Get(request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, runtimeObject); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	var pods corev1.PodList
	if err := kubernetesClient.List(request.Context(), &pods, client.InNamespace(namespace), client.MatchingLabels{"runtime": name}); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	var runs v1alpha1.RunList
	if err := kubernetesClient.List(request.Context(), &runs, client.InNamespace(namespace)); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	byPod := make(map[string][]runSummary)
	runCount := 0
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.Spec.Runtime != name {
			continue
		}
		runCount++
		if run.Status.AssignedPod != "" {
			byPod[run.Status.AssignedPod] = append(byPod[run.Status.AssignedPod], summaryForRun(run))
		}
	}
	podItems := make([]runtimePodSummary, 0, len(pods.Items))
	for i := range pods.Items {
		podItems = append(podItems, summaryForRuntimePod(&pods.Items[i], byPod[pods.Items[i].Name]))
	}
	s.writeJSON(writer, http.StatusOK, runtimeDetailResponse{Runtime: summaryForRuntime(runtimeObject, runCount), Spec: *runtimeObject.Spec.DeepCopy(), Status: *runtimeObject.Status.DeepCopy(), Pods: podItems})
}

func summaryForRuntime(runtimeObject *v1alpha1.Runtime, runCount int) runtimeSummary {
	capacity := map[string]string{}
	if runtimeObject.Spec.Capacity != nil {
		for name, value := range runtimeObject.Spec.Capacity.Resources {
			capacity[string(name)] = value.String()
		}
	}
	if len(capacity) == 0 {
		capacity = nil
	}
	return runtimeSummary{Name: runtimeObject.Name, Namespace: runtimeObject.Namespace, Replicas: runtimeObject.Spec.Replicas, ReadyReplicas: runtimeObject.Status.ReadyReplicas, Capacity: capacity, RunCount: runCount, Healthy: runtimeObject.Spec.Replicas > 0 && runtimeObject.Status.ReadyReplicas >= runtimeObject.Spec.Replicas}
}

func summaryForRuntimePod(pod *corev1.Pod, runs []runSummary) runtimePodSummary {
	if runs == nil {
		runs = []runSummary{}
	}
	item := runtimePodSummary{Name: pod.Name, Phase: pod.Status.Phase, Runs: runs}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			item.Ready = condition.Status == corev1.ConditionTrue
		}
		if condition.Type == v1alpha1.RuntimePodRuntimedReadyCondition {
			item.RuntimedReady = condition.Status == corev1.ConditionTrue
		}
	}
	return item
}

func (s *Server) listWorkflowRuns(writer http.ResponseWriter, request *http.Request, kubernetesClient client.Client) {
	var workflowRuns v1alpha1.WorkflowRunList
	if err := kubernetesClient.List(request.Context(), &workflowRuns, client.InNamespace(request.PathValue("namespace"))); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	items := make([]workflowRunSummary, 0, len(workflowRuns.Items))
	for i := range workflowRuns.Items {
		items = append(items, summaryForWorkflowRun(&workflowRuns.Items[i]))
	}
	s.writeJSON(writer, http.StatusOK, workflowRunListResponse{Items: items})
}

func (s *Server) getWorkflowRun(writer http.ResponseWriter, request *http.Request, kubernetesClient client.Client) {
	workflowRun := &v1alpha1.WorkflowRun{}
	if err := kubernetesClient.Get(request.Context(), client.ObjectKey{Namespace: request.PathValue("namespace"), Name: request.PathValue("name")}, workflowRun); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	s.writeJSON(writer, http.StatusOK, workflowRunDetailResponse{workflowRunSummary: summaryForWorkflowRun(workflowRun), Spec: *workflowRun.Spec.DeepCopy(), Status: *workflowRun.Status.DeepCopy()})
}

func summaryForWorkflowRun(workflowRun *v1alpha1.WorkflowRun) workflowRunSummary {
	return workflowRunSummary{Name: workflowRun.Name, Namespace: workflowRun.Namespace, UID: workflowRun.UID, Phase: workflowRun.Status.Phase, JobCount: len(workflowRun.Spec.Jobs), CreationTimestamp: workflowRun.CreationTimestamp}
}

func (s *Server) getRunLogs(writer http.ResponseWriter, request *http.Request) {
	if s.Clients == nil {
		s.writeError(writer, http.StatusServiceUnavailable, "dashboard Kubernetes client is not configured")
		return
	}
	if s.Gateway == nil {
		s.writeError(writer, http.StatusServiceUnavailable, "Runtime Gateway log API is not configured")
		return
	}
	tailLines, follow, err := parseLogOptions(request)
	if err != nil {
		s.writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	kubernetesClient, err := s.Clients.ClientForRequest(request)
	if err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	run := &v1alpha1.Run{}
	if err := kubernetesClient.Get(request.Context(), client.ObjectKey{Namespace: request.PathValue("namespace"), Name: request.PathValue("name")}, run); err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	token, err := bearerToken(request)
	if err != nil {
		s.writeKubernetesError(writer, err)
		return
	}
	logTarget := string(run.UID)
	if gateway, ok := s.Gateway.(interface{ UsesRunName() bool }); ok && gateway.UsesRunName() {
		logTarget = run.Name
	}
	response, err := s.Gateway.RunLogs(request.Context(), token, run.Namespace, run.Spec.Runtime, logTarget, tailLines, follow)
	if err != nil {
		s.writeError(writer, http.StatusServiceUnavailable, "Runtime Gateway log API is unavailable")
		return
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	writer.WriteHeader(response.StatusCode)
	if follow && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		flush := http.NewResponseController(writer).Flush
		buffer := make([]byte, 32*1024)
		for {
			count, readErr := response.Body.Read(buffer)
			if count > 0 {
				if _, writeErr := writer.Write(buffer[:count]); writeErr != nil {
					return
				}
				if flush() != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}
	_, _ = io.Copy(writer, response.Body)
}

func parseLogOptions(request *http.Request) (int64, bool, error) {
	query := request.URL.Query()
	tailLines := defaultLogTailLines
	if rawTail := query.Get("tail"); rawTail != "" {
		parsed, err := strconv.ParseInt(rawTail, 10, 64)
		if err != nil || parsed < 1 || parsed > maxLogTailLines {
			return 0, false, fmt.Errorf("tail must be between 1 and %d", maxLogTailLines)
		}
		tailLines = parsed
	}
	follow := false
	if rawFollow := query.Get("follow"); rawFollow != "" {
		parsed, err := strconv.ParseBool(rawFollow)
		if err != nil {
			return 0, false, errors.New("follow must be true or false")
		}
		follow = parsed
	}
	return tailLines, follow, nil
}

func summaryForRun(run *v1alpha1.Run) runSummary {
	return runSummary{
		Name:                 run.Name,
		Namespace:            run.Namespace,
		UID:                  run.UID,
		Runtime:              run.Spec.Runtime,
		Mode:                 runModeName(run.Spec.Mode),
		Phase:                run.Status.Phase,
		AssignedPod:          run.Status.AssignedPod,
		Attempt:              run.Status.Attempt,
		CreationTimestamp:    run.CreationTimestamp,
		StartTime:            run.Status.StartTime,
		CompletionTime:       run.Status.CompletionTime,
		LastTransitionReason: lastTransitionReason(run.Status.Conditions),
	}
}

func runModeName(mode v1alpha1.RunMode) string {
	switch {
	case mode.Function != nil:
		return "Function"
	case mode.Session != nil:
		return "Session"
	default:
		return "Task"
	}
}

func lastTransitionReason(conditions []metav1.Condition) string {
	var latest *metav1.Condition
	for i := range conditions {
		condition := &conditions[i]
		if latest == nil || condition.LastTransitionTime.After(latest.LastTransitionTime.Time) {
			latest = condition
		}
	}
	if latest == nil {
		return ""
	}
	return latest.Reason
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func (s *Server) writeKubernetesError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrMissingBearerToken), apierrors.IsUnauthorized(err):
		s.writeError(writer, http.StatusUnauthorized, "Kubernetes bearer token is required")
	case apierrors.IsForbidden(err):
		s.writeError(writer, http.StatusForbidden, "Kubernetes authorization denied")
	case apierrors.IsNotFound(err):
		s.writeError(writer, http.StatusNotFound, "resource not found")
	default:
		s.writeError(writer, http.StatusInternalServerError, "dashboard request failed")
	}
}

func (s *Server) writeError(writer http.ResponseWriter, status int, message string) {
	s.writeJSON(writer, status, errorResponse{Error: message})
}

func (s *Server) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

var _ http.Handler = (*Server)(nil)
