package runtimed

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSessionOperationStorePersistsAndReplaysEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	apiClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: types.UID("run-uid")}}
	store := newSessionOperationStore(apiClient)
	if _, created, err := store.Begin(t.Context(), run, "turn-1", "digest"); err != nil || !created {
		t.Fatalf("Begin() = created %t, err %v", created, err)
	}
	accepted := sessionOperationAccepted(1, "turn-1")
	completed := &pb.SessionOperationEvent{Sequence: 2, Event: &pb.SessionOperationEvent_Completed{Completed: &pb.ExecuteSessionOperationResponse{Command: &pb.SessionCommandResult{ExitCode: 0}}}}
	if err := store.Append(t.Context(), run, "turn-1", accepted); err != nil {
		t.Fatalf("append accepted: %v", err)
	}
	if err := store.Append(t.Context(), run, "turn-1", completed); err != nil {
		t.Fatalf("append completed: %v", err)
	}

	// A fresh store simulates a runtimed restart. It has no in-memory state but
	// can still replay the durable journal exactly from a cursor.
	restarted := newSessionOperationStore(apiClient)
	events, updates, cancel, err := restarted.Subscribe(context.Background(), run, "turn-1", 1)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer cancel()
	if len(events) != 1 || events[0].GetSequence() != 2 || events[0].GetCompleted() == nil {
		t.Fatalf("replayed events = %#v", events)
	}
	if _, ok := <-updates; ok {
		t.Fatal("terminal operation left a live subscriber channel")
	}
}

func TestSessionOperationStoreRejectsIdempotencyKeyReuse(t *testing.T) {
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: types.UID("run-uid")}}
	store := newSessionOperationStore(nil)
	if _, _, err := store.Begin(t.Context(), run, "turn", "first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Begin(t.Context(), run, "turn", "second"); err == nil {
		t.Fatal("Begin accepted a different payload for the same idempotency key")
	}
}
