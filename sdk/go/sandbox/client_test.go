package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSandboxCreatesAndExecutes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	doer := httpDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/namespaces/default/runtimes/bash/sessions/run-uid/operations:execute" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"command":{"exitCode":0,"stdout":"b2s="}}`))}, nil
	})
	runs := fake.NewClientBuilder().WithScheme(scheme).Build()
	client, err := New(Config{Runs: runs, HTTPClient: doer, BearerToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.create(t.Context(), "default", "bash", AcquireOptions{Name: "session", Env: map[string]string{"B": "2", "A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.run.Spec.Mode.Session == nil || len(sandbox.run.Spec.Env) != 2 || sandbox.run.Spec.Env[0].Name != "A" {
		t.Fatalf("created Run = %#v", sandbox.run.Spec)
	}
	sandbox.run.UID = types.UID("run-uid")
	sandbox.run.Status = v1alpha1.RunStatus{Phase: v1alpha1.RunReady, Endpoint: &v1alpha1.RunEndpoint{URL: "http://gateway/v1/namespaces/default/runtimes/bash/sessions/run-uid"}}
	if _, err := sandbox.Execute(context.Background(), Command{Argv: []string{"echo", "ok"}}); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxFileMutationsUseGatewayOperationNames(t *testing.T) {
	operations := []string{}
	sandbox := readySandbox(t, httpDoer(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		for name := range payload {
			operations = append(operations, name)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	ctx := context.Background()
	if err := sandbox.CreateDirectory(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.DeleteFile(ctx, "notes/old", true); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.RenameFile(ctx, "notes/a", "notes/b", true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(operations, ",") != "createDirectory,deleteFile,renameFile" {
		t.Fatalf("operation names = %v", operations)
	}
}

func TestSandboxReadFilePreservesPathForRuntimeValidation(t *testing.T) {
	paths := []string{}
	sandbox := readySandbox(t, httpDoer(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.EscapedPath())
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"contents":""}`))}, nil
	}))
	if _, _, err := sandbox.ReadFile(t.Context(), "notes/file.txt", 64); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sandbox.ReadFile(t.Context(), "../outside.txt", 0); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/v1/namespaces/default/runtimes/bash/sessions/run-uid/files/notes/file.txt,/v1/namespaces/default/runtimes/bash/sessions/run-uid/files/../outside.txt" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestSandboxListFilesUsesExplicitPageOptions(t *testing.T) {
	requests := []string{}
	sandbox := readySandbox(t, httpDoer(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.String())
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"entries":[{"path":"build.log","sizeBytes":12}],"nextPageToken":"next"}`))}, nil
	}))
	page, err := sandbox.ListFiles(t.Context(), ListFilesOptions{Directory: "notes", Limit: 2, PageToken: "after-notes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Path != "build.log" || page.NextPageToken != "next" {
		t.Fatalf("page = %#v", page)
	}
	if got, want := strings.Join(requests, ","), "http://gateway/v1/namespaces/default/runtimes/bash/sessions/run-uid/files?limit=2&pageToken=after-notes&path=notes"; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
	if _, err := sandbox.ListFiles(t.Context(), ListFilesOptions{Limit: -1}); err == nil {
		t.Fatal("ListFiles accepted a negative limit")
	}
}

func TestSandboxLogsFilterRunUID(t *testing.T) {
	reader := logReader(func(context.Context, string, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("not-json\n{\"run_uid\":\"other\",\"stream\":\"stdout\"}\n{\"run_uid\":\"run-uid\",\"stream\":\"audit\",\"message\":\"session operation completed\",\"operation\":\"command\"}\n")), nil
	})
	sandbox := readySandbox(t, httpDoer(func(*http.Request) (*http.Response, error) { return nil, nil }))
	sandbox.client.logReader = reader
	sandbox.run.Name = "session"
	sandbox.run.Namespace = "default"
	sandbox.run.Status.AssignedPod = "runtime-pod"
	if err := sandbox.client.runs.Create(t.Context(), sandbox.run); err != nil {
		t.Fatal(err)
	}
	lines, err := sandbox.Logs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Stream != "audit" || lines[0].Operation != "command" {
		t.Fatalf("logs = %#v", lines)
	}
}

func TestSandboxCloseRequestsDrain(t *testing.T) {
	sandbox := terminalSandbox(t, nil)
	if err := sandbox.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sandbox.Run().Spec.Termination; got == nil || got.Mode != v1alpha1.RunTerminationDrain {
		t.Fatalf("termination = %#v, want Drain", got)
	}
}

func TestRuntimeAcquiresReadySandbox(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	runs := fake.NewClientBuilder().WithScheme(scheme).Build()
	client, err := New(Config{Runs: runs, HTTPClient: httpDoer(func(*http.Request) (*http.Response, error) { return nil, nil }), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			var list v1alpha1.RunList
			if err := runs.List(context.Background(), &list); err == nil && len(list.Items) == 1 {
				run := list.Items[0].DeepCopy()
				run.Status.Phase = v1alpha1.RunReady
				_ = runs.Update(context.Background(), run)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	sandbox, err := client.Runtime("default", "bash").AcquireSandbox(t.Context(), AcquireOptions{Name: "session", Env: map[string]string{"A": "1"}})
	if err != nil {
		t.Fatalf("AcquireSandbox: %v", err)
	}
	if got := sandbox.Run(); got.Spec.Runtime != "bash" || got.Namespace != "default" || got.Status.Phase != v1alpha1.RunReady {
		t.Fatalf("acquired Sandbox Run = %#v", got)
	}
}

func TestSandboxReleaseDeletesSucceededRun(t *testing.T) {
	sandbox := terminalSandbox(t, nil)
	if err := sandbox.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	run := &v1alpha1.Run{}
	if err := sandbox.client.runs.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "session"}, run); err == nil {
		t.Fatalf("released Run still exists: %#v", run)
	}
}

func TestSandboxCancelRequestsImmediateAndEscalatesDrain(t *testing.T) {
	sandbox := terminalSandboxWithPhase(t, &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain}, v1alpha1.RunCancelled)
	if err := sandbox.Cancel(t.Context()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := sandbox.Run().Spec.Termination; got == nil || got.Mode != v1alpha1.RunTerminationImmediate {
		t.Fatalf("termination = %#v, want Immediate", got)
	}
}

func TestSandboxCloseDoesNotDowngradeImmediateTermination(t *testing.T) {
	sandbox := terminalSandbox(t, &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate})
	if err := sandbox.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sandbox.Run().Spec.Termination; got == nil || got.Mode != v1alpha1.RunTerminationImmediate {
		t.Fatalf("termination = %#v, want Immediate", got)
	}
}

func TestSandboxTerminationReportsUnexpectedTerminalPhase(t *testing.T) {
	closeSandbox := terminalSandboxWithPhase(t, nil, v1alpha1.RunFailed)
	if err := closeSandbox.Close(t.Context()); err == nil {
		t.Fatal("Close error = nil, want StateError")
	} else {
		var stateErr *StateError
		if !errors.As(err, &stateErr) || stateErr.Run.Status.Phase != v1alpha1.RunFailed {
			t.Fatalf("Close error = %T %v, want StateError for Failed Run", err, err)
		}
	}

	cancelSandbox := terminalSandboxWithPhase(t, nil, v1alpha1.RunSucceeded)
	if err := cancelSandbox.Cancel(t.Context()); err == nil {
		t.Fatal("Cancel error = nil, want StateError")
	} else {
		var stateErr *StateError
		if !errors.As(err, &stateErr) || stateErr.Run.Status.Phase != v1alpha1.RunSucceeded {
			t.Fatalf("Cancel error = %T %v, want StateError for Succeeded Run", err, err)
		}
	}
}

type httpDoer func(*http.Request) (*http.Response, error)

func (do httpDoer) Do(request *http.Request) (*http.Response, error) { return do(request) }

type logReader func(context.Context, string, string, string) (io.ReadCloser, error)

func (read logReader) Open(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
	return read(ctx, namespace, pod, container)
}

func readySandbox(t *testing.T, doer HTTPDoer) *Sandbox {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Runs: fake.NewClientBuilder().WithScheme(scheme).Build(), HTTPClient: doer})
	if err != nil {
		t.Fatal(err)
	}
	return &Sandbox{client: client, run: &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{UID: "run-uid"}, Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, Endpoint: &v1alpha1.RunEndpoint{URL: "http://gateway/v1/namespaces/default/runtimes/bash/sessions/run-uid"}}}}
}

func terminalSandbox(t *testing.T, termination *v1alpha1.RunTermination) *Sandbox {
	return terminalSandboxWithPhase(t, termination, v1alpha1.RunSucceeded)
}

func terminalSandboxWithPhase(t *testing.T, termination *v1alpha1.RunTermination, phase v1alpha1.RunPhase) *Sandbox {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "run-uid"},
		Spec: v1alpha1.RunSpec{
			Mode:        v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
			Termination: termination,
		},
		Status: v1alpha1.RunStatus{Phase: phase},
	}
	runs := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	client, err := New(Config{Runs: runs, HTTPClient: httpDoer(func(*http.Request) (*http.Response, error) { return nil, nil })})
	if err != nil {
		t.Fatal(err)
	}
	return &Sandbox{client: client, run: run.DeepCopy()}
}

func TestSandboxRejectsEndpointForAnotherRun(t *testing.T) {
	sandbox := &Sandbox{run: &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{UID: "run-uid"}, Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, Endpoint: &v1alpha1.RunEndpoint{URL: "http://gateway/v1/namespaces/default/runtimes/bash/sessions/other"}}}}
	if _, err := sandbox.endpoint("files"); err == nil {
		t.Fatal("endpoint() error = nil, want UID fencing error")
	}
}

func TestConsolePortForwardPreservesRunEndpointPath(t *testing.T) {
	forward := &ConsolePortForward{
		httpClient: httpDoer(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://127.0.0.1:19090/v1/namespaces/default/runtimes/bash/sessions/run-uid/files?maxBytes=10" {
				t.Fatalf("forwarded URL = %q", request.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}),
		localURL: &url.URL{Scheme: "http", Host: "127.0.0.1:19090"},
	}
	request, err := http.NewRequest(http.MethodGet, "https://gateway/v1/namespaces/default/runtimes/bash/sessions/run-uid/files?maxBytes=10", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := forward.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestReadyConsolePodSelectsReadyServiceBackend(t *testing.T) {
	clientset := kubernetesfake.NewSimpleClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "platform"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "gateway"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-z", Namespace: "platform", Labels: map[string]string{"app": "gateway"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-a", Namespace: "platform", Labels: map[string]string{"app": "gateway"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "not-ready", Namespace: "platform", Labels: map[string]string{"app": "gateway"}}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
	)
	pod, err := readyConsolePod(t.Context(), clientset.CoreV1(), "platform", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if pod != "gateway-a" {
		t.Fatalf("gateway Pod = %q, want gateway-a", pod)
	}
}

func TestReadyConsoleBackendUsesServiceTargetPort(t *testing.T) {
	clientset := kubernetesfake.NewSimpleClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "platform"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "gateway"}, Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(8084)}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "platform", Labels: map[string]string{"app": "gateway"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
	)
	pod, port, tlsEnabled, err := readyConsoleBackend(t.Context(), clientset.CoreV1(), "platform", "gateway", 80)
	if err != nil {
		t.Fatal(err)
	}
	if pod != "gateway" || port != 8084 || tlsEnabled {
		t.Fatalf("gateway backend = %q:%d TLS=%t, want gateway:8084 TLS=false", pod, port, tlsEnabled)
	}
}
