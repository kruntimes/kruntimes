package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSandboxCreatesSessionRun(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	runs := fake.NewClientBuilder().WithScheme(scheme).Build()
	client, err := New(Config{RESTConfig: &rest.Config{Host: "http://api-server"}, Runs: runs})
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
}

func TestSandboxReadFilePreservesPathForRuntimeValidation(t *testing.T) {
	paths := []string{}
	sandbox := readySandbox(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.EscapedPath())
		_, _ = io.WriteString(writer, `{"contents":""}`)
	}))
	if _, _, err := sandbox.ReadFile(t.Context(), "notes/file.txt", 64); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sandbox.ReadFile(t.Context(), "../outside.txt", 0); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/apis/sessions.kruntimes.io/v1alpha1/namespaces/default/runs/session/session/files/notes/file.txt,/apis/sessions.kruntimes.io/v1alpha1/namespaces/default/runs/session/session/files/../outside.txt" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestSandboxListFilesUsesExplicitPageOptions(t *testing.T) {
	requests := []string{}
	sandbox := readySandbox(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.String())
		_, _ = io.WriteString(writer, `{"entries":[{"path":"build.log","sizeBytes":12}],"nextPageToken":"next"}`)
	}))
	page, err := sandbox.ListFiles(t.Context(), ListFilesOptions{Directory: "notes", Limit: 2, PageToken: "after-notes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Path != "build.log" || page.NextPageToken != "next" {
		t.Fatalf("page = %#v", page)
	}
	if !strings.Contains(requests[0], "/apis/sessions.kruntimes.io/v1alpha1/namespaces/default/runs/session/session/files") || !strings.Contains(requests[0], "limit=2") || !strings.Contains(requests[0], "pageToken=after-notes") || !strings.Contains(requests[0], "path=notes") {
		t.Fatalf("request = %q", requests[0])
	}
	if _, err := sandbox.ListFiles(t.Context(), ListFilesOptions{Limit: -1}); err == nil {
		t.Fatal("ListFiles accepted a negative limit")
	}
}

func TestSandboxLogsFilterRunUID(t *testing.T) {
	reader := logReader(func(context.Context, string, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("not-json\n{\"run_uid\":\"other\",\"stream\":\"stdout\"}\n{\"run_uid\":\"run-uid\",\"stream\":\"audit\",\"message\":\"session operation completed\",\"operation\":\"command\"}\n")), nil
	})
	sandbox := readySandbox(t, nil)
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
	client, err := New(Config{RESTConfig: &rest.Config{Host: "http://api-server"}, Runs: runs, PollInterval: time.Millisecond})
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

func TestRuntimeDeletesRunWhenAcquireFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	runs := fake.NewClientBuilder().WithScheme(scheme).Build()
	sandboxClient, err := New(Config{RESTConfig: &rest.Config{Host: "http://api-server"}, Runs: runs, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the post-create Wait failure with a Run that transitions to Failed.
	go func() {
		for {
			var run v1alpha1.Run
			if err := runs.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "failed"}, &run); err == nil {
				run.Status.Phase = v1alpha1.RunFailed
				_ = runs.Update(context.Background(), &run)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if _, err := sandboxClient.Runtime("default", "bash").AcquireSandbox(t.Context(), AcquireOptions{Name: "failed"}); err == nil {
		t.Fatal("AcquireSandbox succeeded for failed Run")
	}
	var run v1alpha1.Run
	if err := runs.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "failed"}, &run); err == nil {
		t.Fatal("failed AcquireSandbox left its Run behind")
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

func testServerClient(t *testing.T, handler http.Handler, runs client.Client) *Client {
	t.Helper()
	if handler == nil {
		handler = http.NewServeMux()
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{RESTConfig: &rest.Config{Host: server.URL, BearerToken: "token"}, Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func readySandbox(t *testing.T, handler http.Handler) *Sandbox {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := testServerClient(t, handler, fake.NewClientBuilder().WithScheme(scheme).Build())
	return &Sandbox{client: client, run: readySessionRun()}
}

func readySessionRun() *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "run-uid"},
		Spec:       v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunReady},
	}
}

type logReader func(context.Context, string, string, string) (io.ReadCloser, error)

func (read logReader) Open(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
	return read(ctx, namespace, pod, container)
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
	client, err := New(Config{RESTConfig: &rest.Config{Host: "http://api-server"}, Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	return &Sandbox{client: client, run: run.DeepCopy()}
}
